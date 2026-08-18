"""Unit tests for examples/jira-question-round-trip/decide_reply.py (task
t13, spec claims c38/h23).

Loaded by file path, the same reason tests/test_pr_upkeep_sweep.py loads
sweep.py: it is a code-node/consumer-side payload living beside its
workflow, not part of the culture_nodes package.

No network beyond the loopback `fake_api` fixture (`tests/fake_api.py`, the
same stand-in `tests/test_ledger_review.py` and `tests/test_merge_gate.py`
use for "the control plane is stubbed"), and no credential: every token used
below is a fixture literal, never resolved from the environment or
~/.culture-nodes/operator.env.
"""

from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path

import pytest

EXAMPLE_DIR = Path(__file__).resolve().parents[1] / "examples" / "jira-question-round-trip"

RECORD_ID = "01M09SZ68ECPAF75YE0DMVV63A"


def _load_decide_reply():
    spec = importlib.util.spec_from_file_location("decide_reply", EXAMPLE_DIR / "decide_reply.py")
    module = importlib.util.module_from_spec(spec)
    # Registered in sys.modules BEFORE exec: decide_reply.py's `@dataclass`
    # fields use `from __future__ import annotations`, and dataclasses
    # resolves a class's module via `sys.modules[cls.__module__]` while
    # building it -- an unregistered module fails with a bare AttributeError
    # at class-definition time (see tests/test_stickiness_ab.py for the same
    # note against the same failure mode).
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


decide_reply = _load_decide_reply()


# --- decision-comment format --------------------------------------------------


def test_decision_prompt_names_the_record_id_and_both_accepted_verbs():
    prompt = decide_reply.decision_prompt_text(RECORD_ID)
    assert RECORD_ID in prompt
    assert f"approve {RECORD_ID}" in prompt
    assert f"reject {RECORD_ID}" in prompt


# --- parse_decision_reply: the conservative parser -----------------------------


class TestExactMatchIsADecision:
    def test_approve_is_parsed(self):
        decision = decide_reply.parse_decision_reply(RECORD_ID, f"approve {RECORD_ID}")
        assert decision == decide_reply.Decision(
            record_id=RECORD_ID, verb="approve", verdict="confirm"
        )

    def test_reject_is_parsed(self):
        decision = decide_reply.parse_decision_reply(RECORD_ID, f"reject {RECORD_ID}")
        assert decision == decide_reply.Decision(
            record_id=RECORD_ID, verb="reject", verdict="reject"
        )

    def test_surrounding_whitespace_is_tolerated(self):
        decision = decide_reply.parse_decision_reply(RECORD_ID, f"  approve {RECORD_ID}  \n")
        assert decision is not None
        assert decision.record_id == RECORD_ID


class TestAnythingElseIsNotADecision:
    """The acceptance criterion in one class: an ambiguous or partial reply
    parses to None, never to a low-confidence guess."""

    @pytest.mark.parametrize(
        "body",
        [
            "",
            "approve",
            "reject",
            f"approve {RECORD_ID} please",
            f"I approve {RECORD_ID}",
            f"approve {RECORD_ID[:-1]}",  # wrong id, one character short
            "approve  wrong-record-id",
            f"APPROVE {RECORD_ID}",  # verb casing not accepted -- exact match only
            f"approve\n{RECORD_ID}",  # not one line
            f"approve {RECORD_ID}\nreject {RECORD_ID}",  # multi-line
            f"maybe approve {RECORD_ID}",
            f"approve {RECORD_ID}.",  # trailing punctuation
        ],
    )
    def test_ambiguous_or_partial_replies_parse_to_none(self, body):
        assert decide_reply.parse_decision_reply(RECORD_ID, body) is None

    def test_a_decision_for_a_different_record_id_does_not_match(self):
        assert decide_reply.parse_decision_reply(RECORD_ID, "approve some-other-record") is None

    def test_non_string_body_parses_to_none(self):
        assert decide_reply.parse_decision_reply(RECORD_ID, None) is None  # type: ignore[arg-type]


# --- decide_or_reask: the sweep answer-event payload shape --------------------


def test_exact_reply_from_the_answer_event_yields_a_decision():
    answer = {"comment_id": "1009", "body": f"approve {RECORD_ID}"}
    outcome = decide_reply.decide_or_reask(RECORD_ID, answer)
    assert isinstance(outcome, decide_reply.Decision)
    assert outcome.record_id == RECORD_ID
    assert outcome.verdict == "confirm"


def test_ambiguous_reply_from_the_answer_event_yields_a_reask_naming_the_comment():
    answer = {"comment_id": "1009", "body": "sounds good to me"}
    outcome = decide_reply.decide_or_reask(RECORD_ID, answer)
    assert isinstance(outcome, decide_reply.ReAsk)
    assert outcome.comment_id == "1009"
    assert outcome.body == "sounds good to me"
    assert outcome.reason  # names why, never silent


# --- build_review_commit: the payload must reference the transcribed comment --


def test_build_review_commit_references_the_transcribed_jira_comment():
    decision = decide_reply.Decision(record_id=RECORD_ID, verb="approve", verdict="confirm")
    payload = decide_reply.build_review_commit(decision, "1009", expected_ledger_version=4)
    assert payload["decisions"] == {RECORD_ID: "confirm"}
    assert payload["expected_ledger_version"] == 4
    assert "1009" in payload["rationale"]
    assert RECORD_ID in payload["rationale"]


def test_build_review_request_matches_decide_claims_shape():
    payload = decide_reply.build_review_request(RECORD_ID, 4, "actor://company/human-ops")
    assert payload == {
        "record_ids": [RECORD_ID],
        "ledger_version": 4,
        "reviewer_actor_id": "actor://company/human-ops",
    }


# --- handle_reply: the wired path, fixture control plane, no real network -----


@pytest.fixture
def review_server(fake_api):
    """A fake control plane recording every review-create / review-commit
    call it receives, so a test can assert commit_decision hit the SAME two
    routes scripts/decide-claims.py uses -- without a real network call or a
    real credential."""
    calls = {"reviews": [], "commits": []}

    def create(handler, match, query, body):
        calls["reviews"].append(json.loads(body))
        handler.send_json(
            201,
            {
                "id": "rev-1",
                "run_id": match.group("id"),
                "status": "requested",
                "ledger_version": 4,
                "record_ids": json.loads(body)["record_ids"],
            },
        )

    def commit(handler, match, query, body):
        calls["commits"].append(json.loads(body))
        handler.send_json(
            200,
            {
                "review_id": match.group("id"),
                "records": [{"id": RECORD_ID}],
                "ledger_version": 5,
            },
        )

    fake_api.route("POST", r"/v1alpha1/runs/(?P<id>[^/]+)/reviews", create)
    fake_api.route("POST", r"/v1alpha1/reviews/(?P<id>[^/]+)/commit", commit)
    fake_api.start()
    return calls


def test_exact_reply_commits_a_review_naming_the_comment(fake_api, review_server):
    answer = {"comment_id": "1009", "body": f"approve {RECORD_ID}"}
    result = decide_reply.handle_reply(
        base=fake_api.base_url,
        token="fixture-token",
        run_id="run-1",
        expected_record_id=RECORD_ID,
        ledger_version=4,
        answer=answer,
        reviewer_actor_id="actor://company/human-ops",
    )
    assert isinstance(result, decide_reply.Committed)
    assert result.decision.verdict == "confirm"
    assert result.response["review_id"] == "rev-1"

    # Exactly one review created, over exactly the decided record.
    assert review_server["reviews"] == [
        {
            "record_ids": [RECORD_ID],
            "ledger_version": 4,
            "reviewer_actor_id": "actor://company/human-ops",
        }
    ]
    # The commit's rationale names the transcribed Jira comment.
    (commit_body,) = review_server["commits"]
    assert commit_body["decisions"] == {RECORD_ID: "confirm"}
    assert "1009" in commit_body["rationale"]


def test_ambiguous_reply_commits_nothing_and_reasks(fake_api, review_server):
    answer = {"comment_id": "1010", "body": "not sure, ask someone else"}
    result = decide_reply.handle_reply(
        base=fake_api.base_url,
        token="fixture-token",
        run_id="run-1",
        expected_record_id=RECORD_ID,
        ledger_version=4,
        answer=answer,
        reviewer_actor_id="actor://company/human-ops",
    )
    assert isinstance(result, decide_reply.ReAsk)
    assert result.comment_id == "1010"

    # Nothing committed: neither review route was ever called.
    assert review_server["reviews"] == []
    assert review_server["commits"] == []
    assert fake_api.requests == []


def test_commit_decision_defaults_the_reviewer_via_decide_claims_first_human_actor(
    fake_api, review_server
):
    fake_api.route(
        "GET",
        r"/v1alpha1/actors",
        lambda h, m, q, b: h.send_json(
            200, {"items": [{"id": "actor://company/human-ops", "kind": "human"}]}
        ),
    )
    decision = decide_reply.Decision(record_id=RECORD_ID, verb="approve", verdict="confirm")
    response = decide_reply.commit_decision(
        base=fake_api.base_url,
        token="fixture-token",
        run_id="run-1",
        decision=decision,
        comment_id="1009",
        ledger_version=4,
        reviewer_actor_id=None,
    )
    assert response["review_id"] == "rev-1"
    (review_body,) = review_server["reviews"]
    assert review_body["reviewer_actor_id"] == "actor://company/human-ops"


def test_commit_decision_refuses_when_no_reviewer_can_be_found(fake_api):
    fake_api.route("GET", r"/v1alpha1/actors", lambda h, m, q, b: h.send_json(200, {"items": []}))
    fake_api.start()
    decision = decide_reply.Decision(record_id=RECORD_ID, verb="approve", verdict="confirm")
    with pytest.raises(ValueError, match="no reviewer"):
        decide_reply.commit_decision(
            base=fake_api.base_url,
            token="fixture-token",
            run_id="run-1",
            decision=decision,
            comment_id="1009",
            ledger_version=4,
            reviewer_actor_id=None,
        )
