#!/usr/bin/env python3
"""The marked-question resume correlation, factored for reuse (task t9 leg 3).

The t17 mechanism this generalizes was proven for jira-intake's question
round trip (this directory's workflow.yaml): a flow posts a question through
the narrow jira-comment actor, whose marker carries a `question_id`; the
sweep (`examples/pr-upkeep/sweep.py`) later emits the human's reply as a
`pr-upkeep.jira.comment` fact stamped with `originating_question_id`; and the
resumed leg must decide whether the fact that woke it actually answers the
question IT asked -- a `kind: wait` signal park wakes on every event of that
name, with no payload filter (internal/worker/wait.go), so correlation is a
consumer-side act by construction.

Until t9 that consumer-side act had no shared home: decide_reply.py performed
it inline for claim decisions, and any future flow (the spec-chain leg, plan
task t10) would have re-derived it. This module is the one citable place.

## The marker contract (the three parties, and who owns what)

1. THE ASKER (any workflow node using `actor://company/jira-comment` with a
   `question_id` binding). The bridge -- adapters/jira/src/jira_bridge/
   mapping.py, `Comment.marked_text` -- appends the marker line to the posted
   comment:

       [culture-nodes:jira-actor question_id=<id>]

   `<id>` matches mapping.py's `_QUESTION_ID` pattern
   (``[A-Za-z0-9][A-Za-z0-9._:-]{0,127}``), mirrored here as
   `QUESTION_ID_RE`. The workflow never writes the marker itself; binding
   `question_id` on the post_comment verb is the whole ask-side contract.

2. THE EMITTER (`examples/pr-upkeep/sweep.py`). A later human comment on the
   issue is emitted as a `pr-upkeep.jira.comment` fact whose payload carries
   `originating_question_id` (the `<id>` from the nearest preceding marked
   question -- `jira_question_id_for_answer`) and
   `answer: {"comment_id": ..., "body": ...}`. The sweep's own self-echo
   filter means a marker-bearing or bot-authored comment never becomes such
   a fact at all.

3. THE RESUMED CONSUMER (this module's caller). Given the resume event's
   payload and the `question_id` it asked, `answer_for` returns the answer
   exactly when the fact correlates -- same `originating_question_id`, an
   answer present, and not flagged self-originated. Anything else returns
   None, which a consumer treats as "not my answer" (re-park, re-ask, or
   route onward -- the caller's policy, never this module's).

Deliberately stdlib-only and side-effect-free, like decide_reply.py beside
it: flows CITE this file (the repo's cite-don't-import discipline), tests
load it by path.
"""

from __future__ import annotations

import re
from typing import Any

#: The fixed prefix of every comment the jira-comment actor posts
#: (adapters/jira/src/jira_bridge/mapping.py's COMMENT_MARKER family).
ACTOR_MARKER = "culture-nodes:jira-actor"

#: The full marker line a marked question carries. The id charset mirrors
#: the bridge's own `_QUESTION_ID` pattern and the sweep's
#: `_JIRA_QUESTION_MARKER_RE` -- one vocabulary, three parties.
QUESTION_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")
QUESTION_MARKER_RE = re.compile(
    r"\[culture-nodes:jira-actor question_id=([A-Za-z0-9][A-Za-z0-9._:-]{0,127})\]"
)


def question_marker(question_id: str) -> str:
    """The marker line the bridge appends for `question_id`.

    Provided so an asking flow's tests can pin what will appear on the
    ticket without importing the adapter; raises ValueError on an id the
    bridge itself would refuse, so a bad id fails at the asker, loudly.
    """
    if not QUESTION_ID_RE.fullmatch(question_id):
        raise ValueError(f"question_id {question_id!r} is not a valid marker identifier")
    return f"[{ACTOR_MARKER} question_id={question_id}]"


def is_self_originated(payload: dict[str, Any]) -> bool:
    """True when the fact says the system itself caused it.

    Reads the `self_originated` flag the author-id transition discipline
    stamps (WP-E; absent on older facts, which is falsy by design -- the
    sweep already suppresses its own comments at the emitter, so this flag
    is a second, consumer-side guard, not the only one).
    """
    return payload.get("self_originated") is True


def originating_question_id(payload: dict[str, Any]) -> str:
    """The marked-question id this comment fact answers, or ''."""
    value = payload.get("originating_question_id", "")
    return value if isinstance(value, str) else ""


def correlates(payload: dict[str, Any], question_id: str) -> bool:
    """True exactly when `payload` is the human answer to `question_id`.

    Requires all three: the fact names this question, it carries an answer
    object, and it is not flagged self-originated. `question_id` must be
    non-empty -- correlating "no question" against "no question" is the
    self-answer bug, not a match.
    """
    return bool(
        question_id
        and originating_question_id(payload) == question_id
        and isinstance(payload.get("answer"), dict)
        and not is_self_originated(payload)
    )


def answer_for(payload: dict[str, Any], question_id: str) -> dict[str, Any] | None:
    """The `{"comment_id": ..., "body": ...}` answer, or None.

    None means "this fact is not the answer to the question you asked" --
    a mismatched or missing question id, a fact with no answer body, or a
    self-originated echo. The caller decides what a non-answer means for
    its flow (decide_reply.py treats the analogous case as a re-ask).
    """
    return payload["answer"] if correlates(payload, question_id) else None
