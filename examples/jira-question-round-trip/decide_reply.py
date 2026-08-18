#!/usr/bin/env python3
"""Conservative Jira reply parser for claim decisions (task t13).

The jira-question-round-trip pattern (see this directory's workflow.yaml and
README) already carries a question from a run to a Jira comment and a reply
back into the run: `post-question` posts a marked comment naming a
`question_id`, `await-answer` parks on `until.signal`, and
`examples/pr-upkeep/sweep.py` raises the resume event once a non-self-echoed
reply lands, naming `originating_question_id` and the reply's own
`answer.comment_id` / `answer.body` (sweep.py's `jira_question_id_for_answer`
and its `JIRA_COMMENT_EVENT_NAME` payload).

This module is the SAME channel used for a narrower purpose: deciding a
`proposed` ledger record. A record's id doubles as the round trip's
`question_id` (see `decision_prompt_text`), so the marker the jira-comment
actor appends already carries it (adapters/jira/README.md, `mapping.py`'s
`Comment.marked_text`) -- nothing new has to be invented to correlate a reply
with the record it answers.

## The decision-comment format (documented at length in this directory's
README, "Claim decisions round-trip through Jira")

The posted comment names the record id and the two accepted verbs:

    A ledger record is awaiting your decision: <record-id>

    Reply with exactly one of: `approve <record-id>` or `reject <record-id>`
    Any other reply will not be understood as a decision and this question
    will be re-asked.

    [culture-nodes:jira-actor question_id=<record-id>]

The last line is the jira-comment actor's own marker (task t10/t11), not
authored by this module -- it is what makes the reply's `originating_
question_id` resolve back to `<record-id>` in the first place.

## Parsing is conservative on purpose

`parse_decision_reply` accepts ONLY a reply whose entire (stripped) body is
exactly `approve <record-id>` or `reject <record-id>` for the SAME record id
the comment asked about. Anything else -- a typo'd verb, a different record
id, extra words, a multi-line reply, a reply to the wrong question -- returns
None. There is no partial-match path: this function returns
`Decision | None`, never a best-effort guess, which is what makes "a misread
reply must never commit" true by construction rather than by a caller
remembering to check something. Every caller in this module treats None as a
re-ask, never as a decision with low confidence.

## Committing reuses scripts/decide-claims.py, it does not reimplement it

Task t13's instruction is explicit: the committed review must go through
"the ledger's review-commit route (the same one scripts/decide-claims.py
uses -- factor/reuse, do not duplicate custody logic)". `commit_decision`
below dynamically loads `scripts/decide-claims.py` (the repo's own pattern
for a hyphenated script -- see tests/test_merge_gate.py,
tests/test_pr_upkeep_sweep.py, tests/test_cycle_accounting.py, all of which
use `importlib.util.spec_from_file_location` for exactly this reason: a
script's file name is not a valid Python identifier) and calls that module's
`request()` -- the one function that knows how to authenticate, POST JSON,
and turn an HTTPError into the same diagnostic decide-claims.py itself
prints. No second implementation of that plumbing exists in this file.
"""

from __future__ import annotations

import importlib.util
import re
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any

#: The two accepted verbs, mapped onto the review-commit vocabulary
#: (`POST /v1alpha1/reviews/{id}/commit`'s `decisions` map takes "confirm" or
#: "reject" -- see scripts/decide-claims.py and tests/test_ledger_review.py).
#: "approve" is the word a human types in a Jira comment; "confirm" is the
#: ledger's own verb for the same act. They are kept visibly distinct here
#: rather than renamed to match, because the Jira-facing vocabulary and the
#: ledger's vocabulary are allowed to diverge without either side having to
#: change.
_VERB_TO_VERDICT = {"approve": "confirm", "reject": "reject"}
ACCEPTED_VERBS = tuple(_VERB_TO_VERDICT)

#: EXACT verb+id match only. The record id pattern mirrors the jira-comment
#: actor's own `question_id` pattern (adapters/jira/src/jira_bridge/
#: mapping.py's `_QUESTION_ID`), since a record id doubles as the
#: round trip's question_id here -- a decision reply that could not have been
#: a valid question_id in the first place could not have named a real
#: record either.
_DECISION_RE = re.compile(r"^(approve|reject)[ \t]+([A-Za-z0-9][A-Za-z0-9._:-]{0,127})$")

#: scripts/decide-claims.py, resolved relative to this file so the module
#: works regardless of the caller's own working directory.
DECIDE_CLAIMS_PATH = Path(__file__).resolve().parents[2] / "scripts" / "decide-claims.py"


def _load_decide_claims():
    """Load scripts/decide-claims.py as an importable module.

    Reused, not duplicated: this returns the SAME `request` function
    decide-claims.py's own `main()` calls for both the review-create and the
    review-commit POST, so authentication, JSON encoding, and HTTP-error
    reporting live in exactly one place in this repository.
    """
    spec = importlib.util.spec_from_file_location("decide_claims", DECIDE_CLAIMS_PATH)
    if spec is None or spec.loader is None:  # pragma: no cover - defensive
        raise ImportError(f"cannot load {DECIDE_CLAIMS_PATH}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


@dataclass(frozen=True)
class Decision:
    """An EXACT verb+id match parsed from a Jira reply."""

    record_id: str
    verb: str
    verdict: str  # "confirm" | "reject" -- the review-commit vocabulary


@dataclass(frozen=True)
class ReAsk:
    """What an ambiguous, partial, or mismatched reply yields.

    Carries the reply that could not be understood, so a caller can log or
    surface WHY this became a re-ask rather than a silent no-op. Nothing
    about a ReAsk ever reaches `commit_decision` -- see `handle_reply`.
    """

    reason: str
    comment_id: str = ""
    body: str = ""


def decision_prompt_text(record_id: str) -> str:
    """The human-readable body of a decision-comment, before the jira-comment
    actor appends its `[culture-nodes:jira-actor question_id=...]` marker
    (adapters/jira/src/jira_bridge/mapping.py's `Comment.marked_text`).

    This is the single source of truth for "the decision-comment format the
    loop posts" (task t13); the round-trip README quotes this function's
    output rather than restating it, so the two cannot drift apart.
    """
    verbs = " or ".join(f"`{verb} {record_id}`" for verb in ACCEPTED_VERBS)
    return (
        f"A ledger record is awaiting your decision: {record_id}\n\n"
        f"Reply with exactly one of: {verbs}\n"
        "Any other reply will not be understood as a decision and this "
        "question will be re-asked."
    )


def parse_decision_reply(expected_record_id: str, body: str) -> Decision | None:
    """Parse one Jira reply body against the decision-comment format.

    Returns a `Decision` only when `body`, stripped, is EXACTLY
    "<approve|reject> <expected_record_id>" -- nothing before, nothing
    after, no second line. Anything else returns None, which every caller
    here treats as a re-ask. See the module docstring's "Parsing is
    conservative on purpose" for why this is `Decision | None` rather than a
    best-effort classification.
    """
    if not isinstance(body, str):
        return None
    match = _DECISION_RE.match(body.strip())
    if not match:
        return None
    verb, record_id = match.groups()
    if record_id != expected_record_id:
        return None
    return Decision(record_id=record_id, verb=verb, verdict=_VERB_TO_VERDICT[verb])


def decide_or_reask(expected_record_id: str, answer: dict[str, Any]) -> Decision | ReAsk:
    """`answer` (the `answer` member of a `pr-upkeep.jira.comment` event
    payload -- `{"comment_id": ..., "body": ...}`, see sweep.py's
    `raise_event` call for that event name) -> a `Decision` or a `ReAsk`.
    """
    body = answer.get("body", "")
    comment_id = str(answer.get("comment_id") or "")
    decision = parse_decision_reply(expected_record_id, body)
    if decision is None:
        return ReAsk(
            reason=(
                "reply did not exactly match "
                f"'<{'|'.join(ACCEPTED_VERBS)}> {expected_record_id}'"
            ),
            comment_id=comment_id,
            body=body,
        )
    return decision


def build_review_request(record_id: str, ledger_version: int, reviewer_actor_id: str) -> dict:
    """`POST /v1alpha1/runs/{run_id}/reviews` body -- the same shape
    scripts/decide-claims.py's `main()` builds."""
    return {
        "record_ids": [record_id],
        "ledger_version": ledger_version,
        "reviewer_actor_id": reviewer_actor_id,
    }


def build_review_commit(decision: Decision, comment_id: str, expected_ledger_version: int) -> dict:
    """`POST /v1alpha1/reviews/{review_id}/commit` body.

    `rationale` is where the committed review names the Jira comment it
    transcribed (task t13's acceptance: "the record names the Jira comment it
    transcribed"). decide-claims.py already writes its `--why` argument into
    `rationale` (task t30) and the control plane records it on each review
    record, so this reuses that exact field instead of inventing a second
    place to carry provenance.
    """
    return {
        "decisions": {decision.record_id: decision.verdict},
        "expected_ledger_version": expected_ledger_version,
        "rationale": (
            f"transcribed from Jira comment {comment_id}: "
            f"reply was '{decision.verb} {decision.record_id}'"
        ),
    }


def commit_decision(
    base: str,
    token: str | None,
    run_id: str,
    decision: Decision,
    comment_id: str,
    ledger_version: int,
    reviewer_actor_id: str | None = None,
) -> dict:
    """Commit a parsed `Decision` through the SAME review-create +
    review-commit route scripts/decide-claims.py drives.

    `reviewer_actor_id` defaults the same way decide-claims.py's `main()`
    does -- `decide_claims.first_human_actor(base)` -- reusing that helper
    rather than re-deriving "the reviewer defaults to a registered human"
    a second time.

    Only `decide_or_reask`'s `Decision` branch is meant to reach this
    function; a `ReAsk` is a dead end for it by construction (see
    `handle_reply`), not by a caller's discipline.
    """
    decide_claims = _load_decide_claims()
    reviewer_actor_id = reviewer_actor_id or decide_claims.first_human_actor(base)
    if not reviewer_actor_id:
        raise ValueError(
            "no reviewer given and no registered human actor found; "
            "an agent may not decide its own claim"
        )
    review = decide_claims.request(
        f"{base}/v1alpha1/runs/{run_id}/reviews",
        build_review_request(decision.record_id, ledger_version, reviewer_actor_id),
        token=token,
    )
    review_id = review.get("id")
    return decide_claims.request(
        f"{base}/v1alpha1/reviews/{review_id}/commit",
        build_review_commit(decision, comment_id, ledger_version),
        token=token,
    )


@dataclass(frozen=True)
class Committed:
    """The result of a `Decision` that was successfully committed."""

    decision: Decision
    response: dict


def handle_reply(
    base: str,
    token: str | None,
    run_id: str,
    expected_record_id: str,
    ledger_version: int,
    answer: dict[str, Any],
    reviewer_actor_id: str | None = None,
) -> Committed | ReAsk:
    """The full round trip for one reply: parse, and commit only on an exact
    match. A `ReAsk` is returned untouched and `commit_decision` is never
    called for it -- "a misread reply must never commit" is enforced by this
    function's control flow, not by a downstream check.
    """
    outcome = decide_or_reask(expected_record_id, answer)
    if isinstance(outcome, ReAsk):
        return outcome
    comment_id = str(answer.get("comment_id") or "")
    response = commit_decision(
        base, token, run_id, outcome, comment_id, ledger_version, reviewer_actor_id
    )
    return Committed(decision=outcome, response=response)


if __name__ == "__main__":  # pragma: no cover - library module, no CLI surface
    sys.exit(
        "decide_reply.py is a library module for the jira-question-round-trip "
        "consumer side; it has no standalone CLI."
    )
