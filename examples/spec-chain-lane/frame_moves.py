#!/usr/bin/env python3
"""The deterministic half of the board-driven /think leg (task t13, issue
#199 / #230; frame decision q4 = B of jira-flow-spec-read-related-bugs).

The lane workflow beside this file (`workflow.yaml`) runs the devague moves
as SESSIONS on the developer lane, in its own worktree, with `.devague/`
committed after every move. A session is judgement; devague itself stays
deterministic (devague#20: no LLM inside the CLI). What must NOT be left to
judgement is the one act the spec's honesty condition names: **a human's
answer to a marked question transacts exactly the stated devague move and
no other.** This module is where that act is pinned, so the test beside it
(tests/test_spec_chain_lane.py) can assert it byte-for-byte:

  * `question_id(slug, qid)`  — the marker id a marked question carries on
    the ticket: `<frame-slug>.<devague question id>`, e.g. `scrum-9.q1`.
    The frame slug is the ticket id lowercased (`frame_slug`), so the id
    names the frame AND the question with no lookup table anywhere.
  * `stated_move(question_id, body)` — the ONE argv an answer transacts:
    `devague question --resolve <qid> --decision <body> --frame <slug>`.
  * `transact(payload, question_id)` — correlation THEN the move: the
    resume event's payload (a sweep comment fact or a ticket-page reply
    fact — one schema, schemas/events/jira_comment.schema.json) is judged
    by the shared `question_correlation.answer_for`; None means "not my
    answer", anything else is exactly `stated_move(...)`.
  * `next_envelope(...)` — the final message a session ends with, so the
    bridge's declared-result override (`{"outcome", "output"}`) routes the
    graph on a fact read out of devague's own JSON rather than on prose.

Stdlib-only, side-effect-free, loaded by path (cite-don't-import): the
correlation helper is CITED from examples/jira-question-round-trip by file
path rather than re-derived — exactly the reuse that helper was factored
for (its docstring names this leg as the flow that would otherwise have
re-derived it).
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import re
import sys
from pathlib import Path
from typing import Any

HERE = Path(__file__).resolve().parent
CORRELATION_PATH = HERE.parent / "jira-question-round-trip" / "question_correlation.py"

#: `devague new --title <TICKET-ID>` derives its slug the same way; pinned
#: here so the lane never has to ask devague what it called the frame.
_SLUG_RE = re.compile(r"[^a-z0-9]+")
#: devague's own question ids (`q1`, `q12`, ...).
_DEVAGUE_QID_RE = re.compile(r"^q[0-9]+$")


def _load_correlation():
    spec = importlib.util.spec_from_file_location("question_correlation", CORRELATION_PATH)
    module = importlib.util.module_from_spec(spec)
    sys.modules.setdefault(spec.name, module)
    spec.loader.exec_module(module)
    return module


correlation = _load_correlation()


def frame_slug(ticket_id: str) -> str:
    """`SCRUM-9` -> `scrum-9`: the frame a ticket's /think leg lives in."""
    slug = _SLUG_RE.sub("-", ticket_id.strip().lower()).strip("-")
    if not slug:
        raise ValueError(f"ticket id {ticket_id!r} yields no frame slug")
    return slug


def question_id(slug: str, qid: str) -> str:
    """The marker id for devague question `qid` of frame `slug`."""
    if not _DEVAGUE_QID_RE.fullmatch(qid):
        raise ValueError(f"{qid!r} is not a devague question id (q<N>)")
    marker = f"{slug}.{qid}"
    if not correlation.QUESTION_ID_RE.fullmatch(marker):
        raise ValueError(f"{marker!r} is not a valid marker identifier")
    return marker


def split_question_id(marker: str) -> tuple[str, str]:
    """Inverse of `question_id`; raises on an id this lane did not mint."""
    slug, dot, qid = marker.rpartition(".")
    if not dot or not slug or not _DEVAGUE_QID_RE.fullmatch(qid):
        raise ValueError(f"{marker!r} is not a spec-chain-lane question id (<slug>.q<N>)")
    return slug, qid


def stated_move(marker: str, body: str) -> list[str]:
    """The ONE devague argv a human answer to `marker` transacts.

    A reply resolves the question it answers with the reply's own text as
    the decision note — nothing is captured, confirmed, interrogated or
    exported on the strength of a board comment. Confirmations stay a user
    act performed in the checkout or on the page (spec c7).
    """
    slug, qid = split_question_id(marker)
    decision = body.strip()
    if not decision:
        raise ValueError("an empty reply is not a decision")
    return ["devague", "question", "--resolve", qid, "--decision", decision, "--frame", slug]


def transact(payload: dict[str, Any], marker: str) -> list[str] | None:
    """Correlate, then state the move — or None when this fact is not the
    answer to `marker` (another ticket's reply on the same event name, a
    self-originated echo, a fact with no answer body)."""
    answer = correlation.answer_for(payload, marker)
    if answer is None:
        return None
    return stated_move(marker, str(answer.get("body", "")))


def pending_question(questions: dict[str, Any]) -> dict[str, Any] | None:
    """The first unresolved question in `devague question --list --json`."""
    for entry in questions.get("questions", []):
        if not entry.get("resolved"):
            return entry
    return None


def question_comment(ticket_id: str, marker: str, text: str) -> str:
    """The marked question as posted: the question, and the exact move a
    reply transacts, so the person answering knows what their reply does.
    The jira-comment actor appends the marker line itself (binding
    `question_id`); this text must not carry one."""
    slug, qid = split_question_id(marker)
    return (
        f"Spec decision needed on {ticket_id} (frame {slug}, question {qid}):\n\n"
        f"{text.strip()}\n\n"
        "Reply to this comment (or on the ticket page) with your decision. Your reply "
        f"transacts exactly one devague move and nothing else:\n"
        f'  devague question --resolve {qid} --decision "<your reply>" --frame {slug}\n\n'
        "Claim confirmations are not made from the board; they stay a user act in the "
        "checkout or on the ticket page.\n\n- culture-nodes (Claude)"
    )


def next_envelope(
    ticket_id: str,
    questions: dict[str, Any],
    converge: dict[str, Any],
    frame_version: int,
) -> dict[str, Any]:
    """The session's final message, as the workflow contract expects it.

    Read from devague's own JSON, in this order: a pending question raises
    it (`question_raised`); no pending question and `ready_for_spec` means
    the frame converged (`converged`); otherwise the frame is blocked on
    something only a user may do — confirmations — and the run parks on a
    human task (`needs_confirmation`) rather than asking the board for an
    act the board may not perform.
    """
    slug = frame_slug(ticket_id)
    base = {"frame_slug": slug, "frame_version": int(frame_version)}
    pending = pending_question(questions)
    if pending is not None:
        marker = question_id(slug, str(pending["id"]))
        return {
            "outcome": "question_raised",
            "output": {
                **base,
                "question_id": marker,
                "question": question_comment(ticket_id, marker, str(pending["text"])),
            },
        }
    if converge.get("ready_for_spec") is True:
        return {"outcome": "converged", "output": {**base, "summary": "frame converged"}}
    return {
        "outcome": "needs_confirmation",
        "output": {**base, "blockers": [str(b) for b in converge.get("blockers", [])]},
    }


# ---------------------------------------------------------------------------
# CLI — what a session runs. Every subcommand prints ONE JSON document on
# stdout; a non-answer is exit 3 so a session cannot mistake it for a move.
# ---------------------------------------------------------------------------


def _read_json(path: str) -> Any:
    with open(path, encoding="utf-8") as handle:
        return json.load(handle)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="frame_moves.py")
    sub = parser.add_subparsers(dest="verb", required=True)

    p_slug = sub.add_parser("slug", help="frame slug for a ticket id")
    p_slug.add_argument("--ticket", required=True)

    p_tx = sub.add_parser("transact", help="the one move a resume event transacts")
    p_tx.add_argument("--question-id", required=True)
    p_tx.add_argument("--event-file", required=True, help="the resume event's payload JSON")

    p_next = sub.add_parser("next", help="the session's final-message envelope")
    p_next.add_argument("--ticket", required=True)
    p_next.add_argument("--questions-file", required=True, help="devague question --list --json")
    p_next.add_argument("--converge-file", required=True, help="devague converge --json")
    p_next.add_argument("--frame-version", required=True, type=int)

    args = parser.parse_args(argv)
    if args.verb == "slug":
        print(json.dumps({"slug": frame_slug(args.ticket)}))
        return 0
    if args.verb == "transact":
        move = transact(_read_json(args.event_file), args.question_id)
        if move is None:
            print(json.dumps({"move": None, "reason": "not the answer to " + args.question_id}))
            return 3
        print(json.dumps({"move": move}))
        return 0
    envelope = next_envelope(
        args.ticket,
        _read_json(args.questions_file),
        _read_json(args.converge_file),
        args.frame_version,
    )
    print(json.dumps(envelope))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
