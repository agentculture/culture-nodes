#!/usr/bin/env python3
"""Decide a run's proposed ledger records through the review surface.

PRD §10.4: an agent may only create ``proposed`` records, and no actor
promotes its own proposal. A human decides. This is the operator's side of
that contract — it creates a review request over the records still awaiting a
decision on a run, then commits it with a verdict and a stated reason, so the
decision lands as a first-class ledger event rather than as an operator's
silence.

    scripts/decide-claims.py <run-id> --verdict confirm --why "<reason>"
    scripts/decide-claims.py <run-id> --dry-run
    scripts/decide-claims.py --pending                 # across every run

``--why`` is mandatory on a real decision, and since task t30 it is RECORDED
rather than only printed: the control plane writes it into each review
record's ``rationale``. A confirmation with no stated reason is the failure
mode the authority model exists to prevent, because it is indistinguishable
from not having read the claim.

What is "still awaiting a decision" comes from GET
/v1alpha1/pending-decisions, not from filtering the ledger for
``authority=proposed``. Records are immutable: confirming a claim appends a
review record naming it and leaves the claim reading ``proposed`` forever, so
the authority filter would re-decide records that were already decided.

Both review routes require the decision bearer token — the same secret POST
/v1alpha1/human-tasks/{id}/decision takes (``NODES_HUMAN_DECISION_TOKEN``,
or ``NODES_HUMAN_DECISION_TOKEN_SECRET``, or the same key in
~/.culture-nodes/operator.env).

Exit codes: 0 decided (or nothing to decide); 1 refused; 2 the control plane
could not be reached.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

DEFAULT_API = "http://192.168.1.146:18080"
TOKEN_KEYS = ("NODES_HUMAN_DECISION_TOKEN", "NODES_HUMAN_DECISION_TOKEN_SECRET")


def operator_env(key: str) -> str | None:
    """Read one key from the operator env file the nodes-operator skill uses."""
    env = Path.home() / ".culture-nodes" / "operator.env"
    if not env.is_file():
        return None
    for line in env.read_text(encoding="utf-8").splitlines():
        name, _, value = line.partition("=")
        if name.strip() == key:
            return value.strip().strip("\"'")
    return None


def api_base() -> str:
    if os.environ.get("NODES_API_URL"):
        return os.environ["NODES_API_URL"].rstrip("/")
    from_file = operator_env("NODES_API_URL")
    if from_file:
        return from_file.rstrip("/")
    return DEFAULT_API


def decision_token() -> str | None:
    for key in TOKEN_KEYS:
        if os.environ.get(key):
            return os.environ[key]
        from_file = operator_env(key)
        if from_file:
            return from_file
    return None


def request(url: str, payload: dict | None = None, token: str | None = None) -> dict:
    data = None if payload is None else json.dumps(payload).encode()
    headers = {}
    if data:
        headers["content-type"] = "application/json"
    if token:
        headers["authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, data=data, headers=headers)  # noqa: S310
    try:
        with urllib.request.urlopen(req, timeout=30) as response:  # noqa: S310
            body = response.read()
            return json.loads(body) if body else {}
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode(errors="replace")[:400]
        print(f"decide-claims: HTTP {exc.code} from {url}: {detail}", file=sys.stderr)
        raise SystemExit(1) from exc
    except OSError as exc:
        print(f"decide-claims: cannot reach the control plane: {exc}", file=sys.stderr)
        raise SystemExit(2) from exc


def first_human_actor(base: str) -> str | None:
    """The reviewer defaults to a registered human, mirroring `nodes-op grade`."""
    actors = request(f"{base}/v1alpha1/actors").get("items", [])
    for actor in actors:
        if actor.get("kind") == "human":
            return actor.get("id")
    return None


def pending(base: str, run_id: str | None = None) -> list[dict]:
    """Run groups awaiting a decision, each with the ledger version to review at."""
    url = f"{base}/v1alpha1/pending-decisions"
    if run_id:
        url += f"?run_id={run_id}"
    return request(url).get("items", [])


def print_pending(groups: list[dict]) -> int:
    total = sum(len(group.get("records", [])) for group in groups)
    if not total:
        print("decide-claims: nothing is awaiting a decision")
        return 0
    print(f"decide-claims: {total} record(s) awaiting a decision across {len(groups)} run(s):")
    for group in groups:
        print(f"  {group['run_id']}  (ledger_version {group['ledger_version']})")
        for record in group.get("records", []):
            print(f"    {record['id']}  {record['record_type']}  from {record.get('origin_actor_id', '-')}")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("run_id", nargs="?", help="the run whose claims to decide")
    parser.add_argument("--verdict", choices=("confirm", "reject"))
    parser.add_argument("--why", help="why the claim was confirmed or rejected (mandatory, and recorded)")
    parser.add_argument("--reviewer", help="reviewer actor id (defaults to a registered human)")
    parser.add_argument("--dry-run", action="store_true", help="list what would be decided")
    parser.add_argument("--pending", action="store_true", help="list everything awaiting a decision, across runs")
    args = parser.parse_args()

    base = api_base()

    if args.pending or not args.run_id:
        if not args.pending and not args.run_id:
            parser.error("give a run id to decide, or --pending to see what is awaiting one")
        return print_pending(pending(base))

    groups = pending(base, args.run_id)
    if not groups:
        print(f"decide-claims: run {args.run_id} has nothing awaiting a decision")
        return 0
    group = groups[0]
    version = group["ledger_version"]
    records = [record["id"] for record in group.get("records", [])]

    if args.dry_run:
        return print_pending(groups)

    if not args.verdict or not args.why:
        print(
            "decide-claims: --verdict and --why are both mandatory on a real decision; "
            "a confirmation with no stated reason cannot be told apart from an unread one",
            file=sys.stderr,
        )
        return 1

    token = decision_token()
    if not token:
        print(
            "decide-claims: no decision token found; set NODES_HUMAN_DECISION_TOKEN (or put it in "
            "~/.culture-nodes/operator.env). The review routes are gated by the same secret the "
            "human-task decision route uses — they write human-authority records on whoever presents it",
            file=sys.stderr,
        )
        return 1

    reviewer = args.reviewer or first_human_actor(base)
    if not reviewer:
        print(
            "decide-claims: no reviewer given and no registered human actor found; "
            "pass --reviewer <actor-id> rather than letting an agent decide its own claim",
            file=sys.stderr,
        )
        return 1

    review = request(
        f"{base}/v1alpha1/runs/{args.run_id}/reviews",
        {"record_ids": records, "ledger_version": version, "reviewer_actor_id": reviewer},
        token=token,
    )
    review_id = review.get("id")
    request(
        f"{base}/v1alpha1/reviews/{review_id}/commit",
        {
            "decisions": {record_id: args.verdict for record_id in records},
            "expected_ledger_version": version,
            "rationale": args.why,
        },
        token=token,
    )
    print(
        f"decide-claims: {len(records)} record(s) on run {args.run_id} {args.verdict}ed "
        f"by {reviewer} via review {review_id}"
    )
    print(f"  reason (recorded on each review record): {args.why}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
