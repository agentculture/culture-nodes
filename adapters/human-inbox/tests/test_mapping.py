"""Submission -> §13.4 `completed` event mapping: the human's own outcome
and output pass through verbatim, the claim record is human-origin and
proposed-only, and NO usage block ever appears (humans report no token
usage — omit, never fabricate)."""

from __future__ import annotations

from human_inbox_bridge import mapping


def _ctx():
    return mapping.InvocationContext(run_id="run_1", node_run_id="nr_1", attempt_id="att_1")


def test_completed_event_shape():
    ev = mapping.completed_event(
        {"outcome": "approved", "output": {"verdict": "ship it"}, "note": "looks good"},
        _ctx(),
        actor_id="ops/humans",
        created_at="2026-08-13T00:00:00+00:00",
    )
    assert ev.kind == "completed"
    assert ev.payload["outcome"] == "approved"
    assert ev.payload["output"] == {"verdict": "ship it"}
    assert ev.payload["artifact_refs"] == []
    records = ev.payload["ledger_delta"]["records"]
    assert len(records) == 1


def test_no_usage_block_ever():
    # PRD honesty rule + protocol.go: a nil Usage stays nil — the bridge
    # must OMIT the key, never send zeros a human never measured.
    ev = mapping.completed_event(
        {"outcome": "completed"},
        _ctx(),
        actor_id="ops/humans",
        created_at="2026-08-13T00:00:00+00:00",
    )
    assert "usage" not in ev.payload


def test_claim_record_is_human_origin_proposed():
    ev = mapping.completed_event(
        {"outcome": "completed", "note": "done by hand"},
        _ctx(),
        actor_id="ops/humans",
        created_at="2026-08-13T00:00:00+00:00",
    )
    rec = ev.payload["ledger_delta"]["records"][0]
    assert rec["record_type"] == "claim"
    assert rec["origin"] == {"kind": "human", "actor_id": "ops/humans"}
    # §10.4: via the ordinary append path a human submission is proposed,
    # never self-confirmed — confirmation is a review transaction.
    assert rec["authority"] == "proposed"
    assert rec["run_id"] == "run_1"
    assert rec["node_run_id"] == "nr_1"
    assert rec["attempt_id"] == "att_1"
    assert rec["data"]["statement"] == "done by hand"
    assert rec["created_at"] == "2026-08-13T00:00:00+00:00"


def test_output_defaults_to_empty_object():
    ev = mapping.completed_event(
        {"outcome": "completed"},
        _ctx(),
        actor_id="ops/humans",
        created_at="2026-08-13T00:00:00+00:00",
    )
    assert ev.payload["output"] == {}


def test_submission_error_missing_outcome():
    assert mapping.submission_error({}) is not None
    assert mapping.submission_error({"outcome": ""}) is not None
    assert mapping.submission_error({"outcome": "   "}) is not None
    assert mapping.submission_error({"outcome": 7}) is not None


def test_submission_error_bad_output():
    assert mapping.submission_error({"outcome": "ok", "output": []}) is not None
    assert mapping.submission_error({"outcome": "ok", "output": "text"}) is not None


def test_submission_error_bad_note():
    assert mapping.submission_error({"outcome": "ok", "note": 5}) is not None


def test_submission_error_accepts_a_valid_submission():
    assert mapping.submission_error({"outcome": "ok"}) is None
    assert mapping.submission_error({"outcome": "ok", "output": {"a": 1}, "note": "fine"}) is None


def test_submission_error_validates_observed_marker():
    assert (
        mapping.submission_error(
            {
                "outcome": "merged",
                "observed": {
                    "collection_method": "github_pr_merged",
                    "merge_commit": "abc123",
                },
            }
        )
        is None
    )
    assert mapping.submission_error({"outcome": "merged", "observed": True}) is not None
    assert (
        mapping.submission_error(
            {
                "outcome": "merged",
                "observed": {"collection_method": "github_pr_merged", "merge_commit": ""},
            }
        )
        is not None
    )


def test_submission_error_validates_reply_observed_marker():
    """issue #71: the decision node's github_pr_reply/github_pr_closed
    collection methods, alongside github_pr_merged — same generic gate,
    each method's own one required evidence field (`reference`)."""
    assert (
        mapping.submission_error(
            {
                "outcome": "answered",
                "observed": {
                    "collection_method": "github_pr_reply",
                    "reference": "https://github.com/agentculture/culture-nodes/pull/54#c1",
                },
            }
        )
        is None
    )
    assert (
        mapping.submission_error(
            {
                "outcome": "dropped",
                "observed": {
                    "collection_method": "github_pr_closed",
                    "reference": "https://github.com/agentculture/culture-nodes/pull/55",
                },
            }
        )
        is None
    )
    # Missing the method's required field.
    assert (
        mapping.submission_error(
            {"outcome": "answered", "observed": {"collection_method": "github_pr_reply"}}
        )
        is not None
    )
    # Empty required field.
    assert (
        mapping.submission_error(
            {
                "outcome": "answered",
                "observed": {"collection_method": "github_pr_reply", "reference": ""},
            }
        )
        is not None
    )
    # Unknown collection method is refused, not silently accepted.
    assert (
        mapping.submission_error(
            {
                "outcome": "answered",
                "observed": {"collection_method": "something_else", "reference": "x"},
            }
        )
        is not None
    )
    # A method's field under a DIFFERENT method's key is refused (no
    # cross-method field aliasing).
    assert (
        mapping.submission_error(
            {
                "outcome": "answered",
                "observed": {"collection_method": "github_pr_reply", "merge_commit": "abc123"},
            }
        )
        is not None
    )


def test_claim_record_generalizes_over_collection_methods():
    ev = mapping.completed_event(
        {
            "outcome": "answered",
            "observed": {
                "collection_method": "github_pr_reply",
                "reference": "https://github.com/agentculture/culture-nodes/pull/54#c1",
            },
        },
        _ctx(),
        actor_id="ops/humans",
        created_at="2026-08-13T00:00:00+00:00",
    )
    data = ev.payload["ledger_delta"]["records"][0]["data"]
    assert data["kind"] == "observed-submission"
    assert data["collection_method"] == "github_pr_reply"
    assert data["reference"] == "https://github.com/agentculture/culture-nodes/pull/54#c1"
