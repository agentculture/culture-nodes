from codex_bridge import codex_cli, server
from codex_bridge.session_registry import SessionRegistry


def test_missing_provider_session_is_classified_but_other_failures_are_not():
    lost = codex_cli.SyncRunResult(1, "", "thread not found", None, False)
    ordinary = codex_cli.SyncRunResult(1, "", "rate limit exceeded", None, False)

    assert server._resume_session_lost(lost)
    assert not server._resume_session_lost(ordinary)


def test_cold_resume_rebrief_contains_question_answer_and_minimal_context():
    instruction = server._prepend_resume_rebrief(
        "Continue the parked leg.",
        {
            "question_id": "q-17",
            "question": "Which rollout window?",
            "resume_event": {
                "originating_question_id": "q-17",
                "payload": {"answer": {"comment_id": "1009", "body": "Tuesday"}},
            },
        },
    )

    assert instruction.startswith("RESUME RE-BRIEF")
    assert "Which rollout window?" in instruction
    assert "Tuesday" in instruction
    assert instruction.endswith("Continue the parked leg.")


def test_lost_resume_records_a_fork_while_warm_acquire_does_not():
    registry = SessionRegistry()
    assert registry.acquire("session-gap", "warm-holder")
    registry.release("session-gap", "warm-holder")
    assert registry.fork_events == []

    registry.record_lost_resume("session-gap", "cold-holder")
    assert len(registry.fork_events) == 1
    assert registry.fork_events[0].session_key == "session-gap"
    assert registry.fork_events[0].forked_holder == "cold-holder"
