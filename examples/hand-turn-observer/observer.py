#!/usr/bin/env python3
"""Hand-turn observer -- the agent half of the human-guided loop (decision c25).

Given one work item, this node reads what happened around it -- the item's
runs and their attempt windows, the handover refs the loop pushed, the PR's
events (commits, branch creations and deletions) and the issue/PR comments --
and PROPOSES ``hand_turn`` ledger records against a ``hand_turn_definition``
record the human confirmed. Each proposal is one ``POST /v1alpha1/hand-turns``
body: origin = this observer (an agent), authority = proposed, always. The
human confirms or rejects the batch through the ordinary review surface
(``nodes review create`` / ``commit``) and iterates the definition from what
the observer got wrong. Nothing here is evidence; it is a claim about what a
person did, filed so that #118's "how many hand-turns did this cycle cost" is
a number read from confirmed records rather than a sentence in an issue.

The core is deterministic and offline: :func:`propose` takes the pre-fetched
inputs and the definition's ``data`` and returns the proposals, so the
recognition rules can be tested against a recorded fixture
(``tests/fixtures/hand-turn-observer/pr-307.json``, PR #307's shape: eight
``review-fix/*`` branches, the cherry-pick ``a029689``, operator replies on
threads the loop fixed, an ssh checkout reset noted in an issue). The network
lives in exactly one helper, :func:`http_json`, used by :func:`fetch_runs`
and :func:`recorded_identities` (the nodes API) and :func:`post_proposals`;
PR events and comments arrive as pre-fetched JSON (``gh api`` output -- see
README.md), because this node holds no GitHub credential and should not.

Determinism is also why ``--post`` reads before it writes. The recogniser
looks at a work item's WHOLE history, so every rerun re-recognises every turn
it recognised last time, and ``POST /v1alpha1/hand-turns`` appends -- it has
no idempotency key and no existing-record check. Left alone, a second tick on
the same item, or a retry after a partial batch failed halfway, files a second
copy of turns already on the ledger, and two copies confirmed separately are
two hand-turns in ``hand_turns_by_stage`` -- the number a delivery summary
cites (docs/operations/hand-turn-ledger.md) inflated by the observer running
twice. So :func:`recorded_identities` reads the item's existing ``hand_turn``
records first and :func:`unrecorded` drops the proposals that repeat one. See
that function for what "the same turn" means, and README.md for the residual
this does not close.

Rules live in the DEFINITION, not here. A rule is ``{id, stage, description}``
(what the schema pins) plus a matcher the observer understands:

``kind: commit_without_run``
    a ``pr_events`` commit by a human login with no run attempt window
    (widened by ``window_minutes`` each side) containing its time, and whose
    sha is not a handover ref's commit -- the cherry-pick shape.
``kind: event_match``
    a ``pr_events`` entry of ``event_type`` whose ``field`` matches
    ``pattern`` -- the ``review-fix/*`` branch-delete shape.
``kind: comment_match``
    an ``issue_comments`` entry by a human login whose body matches
    ``pattern``; with ``on_loop_fixed_thread: true`` only comments the
    source flagged as replies on a thread the loop fixed -- the reply and
    the ssh-reset shapes.

Stdlib only. Runs as the culture-nodes code node in ``workflow.yaml`` or by
hand: ``python3 observer.py --inputs pr.json --definition definition.json``.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.request
from datetime import datetime, timedelta, timezone
from typing import Any
from urllib.parse import quote, urlencode

USER_AGENT = "culture-nodes-hand-turn-observer/1.0"


# ---------------------------------------------------------------------------
# The one network helper, and its callers
# ---------------------------------------------------------------------------


def http_json(method: str, url: str, body: Any = None, token: str | None = None) -> Any:
    """One JSON HTTP call. The only place this module touches the network."""
    data = None if body is None else json.dumps(body).encode("utf-8")
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("User-Agent", USER_AGENT)
    req.add_header("Accept", "application/json")
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    if not url.startswith(("http://", "https://")):
        raise ValueError(f"refusing non-HTTP URL {url!r}")
    with urllib.request.urlopen(req, timeout=60) as resp:  # nosec B310 - scheme checked above
        raw = resp.read()
    return json.loads(raw) if raw else None


def list_runs(base_url: str, work_item: str) -> list[dict[str, Any]]:
    """The item's runs as ``GET /runs?work_item=`` lists them, newest first.

    The key is escaped, not interpolated: a work item is often the transient
    ``gh:<owner>/<repo>#<n>`` form a PR with no ticket carries, and pasted raw
    into a URL everything from the ``#`` becomes a fragment the server never
    sees -- so the control plane would be asked about ``gh:owner/repo`` and
    answer, wrongly and silently, that the item has no runs.
    """
    base = base_url.rstrip("/")
    query = urlencode({"work_item": work_item, "limit": 100})
    listing = http_json("GET", f"{base}/v1alpha1/runs?{query}") or {}
    return list(listing.get("items") or [])


def fetch_runs(base_url: str, work_item: str) -> list[dict[str, Any]]:
    """The item's runs with their node runs (the listing + each run's view)."""
    base = base_url.rstrip("/")
    runs = []
    for item in list_runs(base, work_item):
        view = http_json("GET", f"{base}/v1alpha1/runs/{quote(str(item['id']), safe='')}") or {}
        run = dict(view.get("run") or item)
        run["node_runs"] = view.get("node_runs") or []
        runs.append(run)
    return runs


def recorded_identities(base_url: str, work_item: str) -> set[tuple[str, str, str, tuple]]:
    """The identities of ``hand_turn`` records already on the item's ledger.

    Read across ALL of the item's runs, not just the newest one, because that
    is the scope the count is read over: ``hand_turns_by_stage`` sums the
    confirmed records on every run the actor attempted, so a copy filed
    against an earlier run of the same item is a copy in the number. The read
    is unauthenticated (``GET /runs/{id}/ledger``, like the runs listing) --
    the observer's own bearer opens ``POST /v1alpha1/hand-turns`` and nothing
    else, and this fix must not need a wider grant to work.

    Every existing record counts, whatever its authority: a turn a person
    already REJECTED is one they have ruled on, and re-proposing it each tick
    is noise in the review surface. The fix for a wrong rejection is a new
    definition, not a resubmission.
    """
    base = base_url.rstrip("/")
    seen: set[tuple[str, str, str, tuple]] = set()
    for item in list_runs(base, work_item):
        run_id = item.get("id")
        if not run_id:
            continue
        path = f"{base}/v1alpha1/runs/{quote(str(run_id), safe='')}/ledger"
        for record in (http_json("GET", path) or {}).get("items") or []:
            if record.get("record_type") == "hand_turn":
                seen.add(hand_turn_identity(record.get("data") or {}))
    return seen


def post_proposals(
    base_url: str, token: str, actor_id: str, proposals: list[dict[str, Any]]
) -> list[dict[str, Any]]:
    """POST each proposal as this observer; returns the appended records."""
    base = base_url.rstrip("/")
    return [
        http_json("POST", f"{base}/v1alpha1/hand-turns", to_request(p, actor_id), token)
        for p in proposals
    ]


# ---------------------------------------------------------------------------
# Deterministic core
# ---------------------------------------------------------------------------


def parse_time(value: str | None) -> datetime | None:
    if not value:
        return None
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    parsed = datetime.fromisoformat(text)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def attempt_windows(runs: list[dict[str, Any]]) -> list[tuple[datetime, datetime]]:
    """Every (started_at, completed_at) attempt window across the item's runs."""
    windows = []
    for run in runs:
        for node_run in run.get("node_runs") or []:
            for attempt in node_run.get("attempts") or []:
                start = parse_time(attempt.get("started_at"))
                end = parse_time(attempt.get("completed_at")) or start
                if start is not None:
                    windows.append((start, end or start))
    return windows


def within_any_window(
    at: datetime, windows: list[tuple[datetime, datetime]], slack: timedelta
) -> bool:
    return any(start - slack <= at <= end + slack for start, end in windows)


def _proposal(
    rule: dict[str, Any],
    work_item: str,
    definition_ref: str | None,
    what: str,
    evidence: list[str],
    observed_at: str | None,
) -> dict[str, Any]:
    out: dict[str, Any] = {
        "what": what,
        "stage": rule["stage"],
        "work_item": work_item,
        "definition_ref": definition_ref,
        "rule": rule["id"],
        "evidence_refs": [e for e in evidence if e],
    }
    if observed_at:
        out["observed_at"] = observed_at
    return out


def _match_commit_without_run(
    rule: dict[str, Any], inputs: dict[str, Any], humans: set[str], definition_ref: str | None
) -> list[dict[str, Any]]:
    windows = attempt_windows(inputs.get("runs") or [])
    slack = timedelta(minutes=int(rule.get("window_minutes", 30)))
    handover_commits = {(ref.get("commit") or "")[:7] for ref in inputs.get("handover_refs") or []}
    out = []
    for event in inputs.get("pr_events") or []:
        if event.get("type") != "commit" or event.get("author") not in humans:
            continue
        sha = event.get("sha") or ""
        if sha[:7] in handover_commits:
            continue  # the loop's own landed commit is not a hand-turn
        at = parse_time(event.get("at"))
        if at is not None and within_any_window(at, windows, slack):
            continue
        message = (event.get("message") or "").splitlines()[0] if event.get("message") else ""
        minutes = slack // timedelta(minutes=1)
        what = (
            f"commit {sha[:7]} by {event.get('author')} on "
            f"{event.get('branch', 'the PR branch')} with no run attempt within "
            f"{minutes} min: {message}"
        ).rstrip(": ")
        out.append(
            _proposal(rule, inputs["work_item"], definition_ref, what, [sha], event.get("at"))
        )
    return out


def _match_event(
    rule: dict[str, Any], inputs: dict[str, Any], humans: set[str], definition_ref: str | None
) -> list[dict[str, Any]]:
    pattern = re.compile(rule.get("pattern", ".*"))
    field = rule.get("field", "branch")
    out = []
    for event in inputs.get("pr_events") or []:
        if event.get("type") != rule.get("event_type"):
            continue
        if rule.get("human_only", True) and event.get("author") not in humans:
            continue
        value = str(event.get(field) or "")
        if not pattern.search(value):
            continue
        what = f"{rule['event_type'].replace('_', ' ')} {value} by {event.get('author')}"
        evidence = [event.get("id") or value]
        out.append(
            _proposal(rule, inputs["work_item"], definition_ref, what, evidence, event.get("at"))
        )
    return out


def _match_comment(
    rule: dict[str, Any], inputs: dict[str, Any], humans: set[str], definition_ref: str | None
) -> list[dict[str, Any]]:
    pattern = re.compile(rule.get("pattern", ".*"), re.IGNORECASE | re.DOTALL)
    out = []
    for comment in inputs.get("issue_comments") or []:
        if comment.get("author") not in humans:
            continue
        if rule.get("on_loop_fixed_thread") and not comment.get("thread_fixed_by_loop"):
            continue
        if not pattern.search(comment.get("body") or ""):
            continue
        first_line = (comment.get("body") or "").strip().splitlines()[0][:80]
        where = comment.get("url") or comment.get("id") or ""
        what = f"{rule.get('what', 'comment')} by {comment.get('author')}: {first_line}"
        out.append(
            _proposal(rule, inputs["work_item"], definition_ref, what, [where], comment.get("at"))
        )
    return out


_MATCHERS = {
    "commit_without_run": _match_commit_without_run,
    "event_match": _match_event,
    "comment_match": _match_comment,
}


def propose(
    inputs: dict[str, Any], definition: dict[str, Any], definition_ref: str | None = None
) -> list[dict[str, Any]]:
    """Recognise hand-turns in ``inputs`` against ``definition`` (a
    ``hand_turn_definition`` record's ``data``). Deterministic: the result is
    sorted, so the same inputs always yield the same batch for review."""
    if not inputs.get("work_item"):
        raise ValueError("inputs.work_item is required")
    humans = set(definition.get("human_logins") or inputs.get("human_logins") or [])
    stages = set(definition.get("stages") or [])
    proposals: list[dict[str, Any]] = []
    for rule in definition.get("rules") or []:
        if rule.get("stage") not in stages:
            raise ValueError(
                f"rule {rule.get('id')!r} names stage {rule.get('stage')!r}, "
                f"not one of {sorted(stages)}"
            )
        matcher = _MATCHERS.get(rule.get("kind", ""))
        if matcher is None:
            raise ValueError(f"rule {rule.get('id')!r} has unknown kind {rule.get('kind')!r}")
        proposals.extend(matcher(rule, inputs, humans, definition_ref))
    proposals.sort(key=lambda p: (p.get("observed_at") or "", p["rule"], p["what"]))
    return proposals


def hand_turn_identity(turn: dict[str, Any]) -> tuple[str, str, str, tuple]:
    """What makes two hand-turns THE SAME turn, for a proposal or a record.

    Both shapes carry the same field names -- a proposal is the request body
    and the appended record's ``data`` is that body minus ``actor_id`` -- so
    one function reads either.

    The identity is the work item, the rule that fired, when the turn was
    observed, and the evidence the rule matched. Two fields are deliberately
    OUT of it:

    ``what``
        generated prose. It moves when a rule's wording or the slack window
        changes, and the person still performed one step. Including it would
        re-file every turn on the next tick after a cosmetic edit.
    ``definition_ref``
        iterating the definition is exactly the loop this node is built for
        (the person changes the rules from what the observer got wrong).
        Including it would make every iteration re-file the turns the old
        definition already found, which is the inflation this guards against
        wearing a different hat.

    ``observed_at`` is in it because a rule's evidence is not always unique on
    its own: ``event_match`` falls back to the matched field value, so the
    same branch created and deleted twice is one evidence string and two
    turns. It is the source event's own timestamp, not a clock read here, so
    it is stable across reruns.
    """
    return (
        str(turn.get("work_item") or ""),
        str(turn.get("rule") or ""),
        str(turn.get("observed_at") or ""),
        tuple(str(ref) for ref in turn.get("evidence_refs") or []),
    )


def unrecorded(
    proposals: list[dict[str, Any]], already: set[tuple[str, str, str, tuple]]
) -> list[dict[str, Any]]:
    """``proposals`` minus the turns already on the ledger, and minus repeats.

    ``already`` is what :func:`recorded_identities` read. The batch is also
    de-duplicated against itself as it goes, so a source list that names the
    same commit or comment twice -- one ``gh api`` page fetched twice by the
    operator lane -- files one turn, not two.
    """
    seen = set(already)
    fresh = []
    for proposal in proposals:
        identity = hand_turn_identity(proposal)
        if identity in seen:
            continue
        seen.add(identity)
        fresh.append(proposal)
    return fresh


def to_request(proposal: dict[str, Any], actor_id: str) -> dict[str, Any]:
    """A proposal as the ``CreateHandTurnRequest`` body this observer posts."""
    body = dict(proposal)
    body["actor_id"] = actor_id
    if body.get("definition_ref") is None:
        body.pop("definition_ref", None)
    return body


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


def _load_json(path: str) -> Any:
    with open(path, encoding="utf-8") as handle:
        return json.load(handle)


#: The `observe` node's input binding name (workflow.yaml).
INPUT_BINDING = "item"


def bound_input(payload: Any) -> Any:
    """The work-item document inside a code node's resolved input.

    NODES_INPUT_JSON carries the node's resolved input DOCUMENT -- one key per
    declared binding -- so ``item: /run/input`` arrives as ``{"item": {...}}``
    rather than as the run input itself. A hand run (``--inputs pr.json``)
    passes the document directly. Both are accepted: unwrap only when the
    wrapper is the binding's name and the payload carries no ``work_item`` of
    its own, so a genuinely bare input is never mistaken for a wrapper.
    """
    if isinstance(payload, dict) and "work_item" not in payload:
        inner = payload.get(INPUT_BINDING)
        if isinstance(inner, dict):
            return inner
    return payload


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Propose hand_turn records for one work item.")
    parser.add_argument(
        "--inputs",
        help=(
            "Pre-fetched inputs JSON (work_item, runs, handover_refs, pr_events, "
            "issue_comments). Default: $NODES_INPUT_JSON, the code node's "
            "resolved input, where the same document sits under the `item` "
            "binding."
        ),
    )
    parser.add_argument(
        "--definition", required=True, help="The hand_turn_definition record's data, as JSON."
    )
    parser.add_argument(
        "--definition-ref",
        default=os.environ.get("HAND_TURN_DEFINITION_REF") or None,
        help="Ledger id of the confirmed definition record.",
    )
    parser.add_argument(
        "--fetch-runs",
        action="store_true",
        help="Replace inputs.runs with a live read from $NODES_API_URL.",
    )
    parser.add_argument(
        "--post",
        action="store_true",
        help=(
            "POST the proposals to $NODES_API_URL as $NODES_OBSERVER_ACTOR_ID "
            "with $NODES_HAND_TURN_TOKEN, skipping the turns already on the "
            "item's ledger."
        ),
    )
    args = parser.parse_args(argv)

    if args.inputs:
        inputs = bound_input(_load_json(args.inputs))
    else:
        raw = os.environ.get("NODES_INPUT_JSON")
        if not raw:
            print("observer: pass --inputs or set NODES_INPUT_JSON", file=sys.stderr)
            return 2
        inputs = bound_input(json.loads(raw))
    definition = _load_json(args.definition)

    if args.fetch_runs:
        inputs["runs"] = fetch_runs(os.environ["NODES_API_URL"], inputs["work_item"])

    proposals = propose(inputs, definition, args.definition_ref)
    result: dict[str, Any] = {
        "work_item": inputs["work_item"],
        "proposals": proposals,
        "proposed": len(proposals),
    }
    if args.post and proposals:
        api_url = os.environ["NODES_API_URL"]
        # Read before writing: the recogniser re-recognises the item's whole
        # history every tick, and the create route appends unconditionally.
        fresh = unrecorded(proposals, recorded_identities(api_url, inputs["work_item"]))
        records = post_proposals(
            api_url,
            os.environ["NODES_HAND_TURN_TOKEN"],
            os.environ["NODES_OBSERVER_ACTOR_ID"],
            fresh,
        )
        result["record_ids"] = [r.get("id") for r in records]
        result["posted"] = len(records)
        result["already_recorded"] = len(proposals) - len(fresh)
    json.dump(result, sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
