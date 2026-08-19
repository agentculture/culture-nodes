"""The shared marked-question resume correlation (task t9 leg 3).

examples/jira-question-round-trip/question_correlation.py is the one citable
home of the consumer-side half of the t17 mechanism: given a resume event's
payload and the question_id a flow asked, decide whether this fact is that
question's human answer. Loaded by file path, like every code-node/consumer
payload living beside its workflow (see tests/test_decide_reply.py).

The emitter-consistency test at the bottom is the load-bearing one: it runs
the REAL sweep emitter (examples/pr-upkeep/sweep.py, untouched by t9) over a
synthetic issue history and feeds the emitted `pr-upkeep.jira.comment`
payloads straight into the helper -- so the asker's marker, the emitter's
`originating_question_id` stamp, and this consumer-side correlation are
pinned against each other rather than each against its own fixture.
"""

from __future__ import annotations

import importlib
import importlib.util
import sys
from pathlib import Path

import pytest

from tests.test_pr_upkeep_sweep import sweep  # noqa: F401 — loads the example dir onto sys.path

jira = importlib.import_module("pr_upkeep_jira")

EXAMPLE_DIR = Path(__file__).resolve().parents[1] / "examples" / "jira-question-round-trip"


def _load_correlation():
    spec = importlib.util.spec_from_file_location(
        "question_correlation", EXAMPLE_DIR / "question_correlation.py"
    )
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


correlation = _load_correlation()

QUESTION_ID = "run-17.q1"


def _answer_payload(**overrides):
    payload = {
        "source": "jira",
        "id": "SCRUM-3",
        "originating_question_id": QUESTION_ID,
        "answer": {"comment_id": "10110", "body": "Yes, ship it."},
    }
    payload.update(overrides)
    return payload


class TestCorrelation:
    def test_the_matching_answer_correlates_and_is_returned(self):
        payload = _answer_payload()
        assert correlation.correlates(payload, QUESTION_ID)
        assert correlation.answer_for(payload, QUESTION_ID) == payload["answer"]

    def test_a_different_questions_answer_does_not_correlate(self):
        payload = _answer_payload(originating_question_id="run-99.q1")
        assert correlation.answer_for(payload, QUESTION_ID) is None

    def test_a_bare_comment_fact_without_a_question_never_correlates(self):
        payload = _answer_payload()
        del payload["originating_question_id"]
        assert correlation.answer_for(payload, QUESTION_ID) is None

    def test_an_empty_question_id_never_correlates_even_against_an_unmarked_fact(self):
        # Correlating "no question" with "no question" would be the
        # self-answer bug; the helper refuses it by construction.
        payload = _answer_payload()
        del payload["originating_question_id"]
        assert not correlation.correlates(payload, "")

    def test_a_self_originated_fact_never_correlates(self):
        payload = _answer_payload(self_originated=True)
        assert correlation.answer_for(payload, QUESTION_ID) is None

    def test_a_fact_without_an_answer_object_never_correlates(self):
        payload = _answer_payload()
        del payload["answer"]
        assert correlation.answer_for(payload, QUESTION_ID) is None


class TestMarkerContract:
    def test_the_marker_matches_the_bridges_and_the_sweeps_own_pattern(self):
        marker = correlation.question_marker(QUESTION_ID)
        assert marker == f"[culture-nodes:jira-actor question_id={QUESTION_ID}]"
        match = correlation.QUESTION_MARKER_RE.search(f"Which one?\n\n{marker}")
        assert match
        assert match.group(1) == QUESTION_ID
        # The sweep's own extraction pattern (the emitter side of the
        # contract) accepts exactly the same line.
        assert jira._JIRA_QUESTION_MARKER_RE.search(marker).group(1) == QUESTION_ID

    def test_an_id_the_bridge_would_refuse_fails_at_the_asker(self):
        with pytest.raises(ValueError):
            correlation.question_marker("bad id")


def _comment(comment_id, account_id, text, created):
    return {
        "id": comment_id,
        "author": {"accountId": account_id},
        "created": created,
        "body": {
            "type": "doc",
            "version": 1,
            "content": [{"type": "paragraph", "content": [{"type": "text", "text": text}]}],
        },
    }


class TestEmitterConsistency:
    """The real emitter's facts, correlated by the shared helper."""

    def test_the_sweeps_emitted_answer_fact_correlates_and_a_bare_comment_does_not(self):
        marker = correlation.question_marker(QUESTION_ID)
        issue = {
            "key": "SCRUM-3",
            "fields": {
                "comment": {
                    "comments": [
                        _comment(
                            "10100",
                            "712020:bot",
                            f"Which behavior is intended?\n\n{marker}",
                            "2026-08-19T10:00:00.000+0000",
                        ),
                        _comment(
                            "10106",
                            "712020:alice",
                            "approve it",
                            "2026-08-19T10:05:00.000+0000",
                        ),
                    ]
                }
            },
            "changelog": {"histories": []},
        }
        facts = sweep.jira_history_facts(issue, "712020:bot")
        comment_facts = [
            payload
            for name, payload, _wm, kind, _pid in facts
            if name == sweep.JIRA_COMMENT_EVENT_NAME
        ]
        # The bot's own marked question was suppressed at the emitter; only
        # the human answer became a fact, and it correlates.
        assert len(comment_facts) == 1
        answer = correlation.answer_for(comment_facts[0], QUESTION_ID)
        assert answer == {"comment_id": "10106", "body": "approve it"}
        # The same fact is NOT the answer to any other flow's question.
        assert correlation.answer_for(comment_facts[0], "run-99.q1") is None

    def test_a_bare_human_comment_with_no_preceding_question_never_correlates(self):
        issue = {
            "key": "SCRUM-3",
            "fields": {
                "comment": {
                    "comments": [
                        _comment(
                            "10107",
                            "712020:alice",
                            "please pick this up",
                            "2026-08-19T11:00:00.000+0000",
                        ),
                    ]
                }
            },
            "changelog": {"histories": []},
        }
        facts = sweep.jira_history_facts(issue, "712020:bot")
        (payload,) = [
            payload
            for name, payload, _wm, _kind, _pid in facts
            if name == sweep.JIRA_COMMENT_EVENT_NAME
        ]
        assert correlation.originating_question_id(payload) == ""
        assert correlation.answer_for(payload, QUESTION_ID) is None
