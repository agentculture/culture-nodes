"""Recorded-fixture tests for the merge-as-action tracker (plan t16)."""

from __future__ import annotations

import io
import json
import logging
import urllib.error
from pathlib import Path

import pytest

from human_inbox_bridge import server, tracker
from human_inbox_bridge.config import Config
from human_inbox_bridge.store import STATUS_COMPLETED, STATUS_PENDING, HumanTask, TaskStore

from ._fakes import FakeCallbackReceiver
from .test_server_unit import AUTH, _invoke, _request

FIXTURES = Path(__file__).parent / "fixtures"


def _fixture(name: str) -> dict:
    return json.loads((FIXTURES / name).read_text(encoding="utf-8"))


@pytest.fixture()
def live(tmp_path):
    cfg = Config(
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
        actor_id="company/human-ops",
        callback_timeout_seconds=5.0,
        callback_max_retries=1,
        callback_retry_backoff_seconds=0.01,
    )
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    receiver = FakeCallbackReceiver()
    yield f"http://{host}:{port}", cfg, receiver
    receiver.close()
    srv.shutdown()
    srv.server_close()


def _tracker_config(base: str, cfg: Config, **overrides) -> tracker.TrackerConfig:
    fields = {
        "state_dir": cfg.state_dir,
        "bridge_url": base,
        "bridge_token": "s3cr3t",
        "github_token": "github-secret",
        "default_repo": "agentculture/culture-nodes",
        "poll_seconds": 1.0,
        "github_request_budget": 10,
        "http_timeout_seconds": 5.0,
    }
    fields.update(overrides)
    return tracker.TrackerConfig(**fields)


def test_merged_pr_submits_once_with_observed_claim_and_is_idempotent(live):
    base, cfg, receiver = live
    status, accepted = _invoke(
        base,
        receiver,
        idem_key="att_merge_54",
        observe={"kind": "github_pr_merged", "repo": "agentculture/culture-nodes", "pr": 54},
    )
    assert status == 202
    assert receiver.wait_for_kind("accepted", timeout=10.0) is not None

    fixture = _fixture("github_pr_merged.json")
    submits = []

    def fetch(repo, pr, **kwargs):
        assert (repo, pr) == ("agentculture/culture-nodes", 54)
        assert kwargs["token"] == "github-secret"
        return fixture

    def submit(*args, **kwargs):
        submits.append((args, kwargs))
        return tracker.submit_observation(*args, **kwargs)

    poller = tracker.MergeTracker(
        _tracker_config(base, cfg), github_fetch=fetch, bridge_submit=submit
    )
    result = poller.run_cycle()
    assert result.github_requests == 1
    assert result.submissions == 1
    assert len(submits) == 1

    completed = receiver.wait_for_kind("completed", timeout=10.0)
    assert completed is not None
    record = completed["payload"]["ledger_delta"]["records"][0]
    assert completed["payload"]["outcome"] == "merged"
    assert record["authority"] == "proposed"
    assert record["origin"] == {"kind": "human", "actor_id": "company/human-ops"}
    assert record["data"]["kind"] == "observed-submission"
    assert record["data"]["collection_method"] == "github_pr_merged"
    assert record["data"]["merge_commit"] == fixture["merge_commit_sha"]
    assert fixture["merge_commit_sha"] in record["data"]["statement"]
    assert TaskStore(cfg.state_dir).get(accepted["invocation_id"]).status == STATUS_COMPLETED

    # A completed task is outside the next pending scan: no GitHub request
    # and, critically, no second submit.
    second = poller.run_cycle()
    assert second.github_requests == 0
    assert second.submissions == 0
    assert len(submits) == 1


def test_merged_pr_completes_without_github_token_in_environment(live):
    base, cfg, receiver = live
    _, accepted = _invoke(
        base,
        receiver,
        idem_key="att_merge_anonymous",
        observe={"kind": "github_pr_merged", "repo": "agentculture/culture-nodes", "pr": 54},
    )
    assert receiver.wait_for_kind("accepted", timeout=10.0) is not None

    tracker_cfg = tracker.TrackerConfig.from_env(
        {
            "HUMAN_INBOX_BRIDGE_STATE_DIR": cfg.state_dir,
            "HUMAN_INBOX_BRIDGE_AUTH_TOKEN": "s3cr3t",
            "HUMAN_INBOX_TRACKER_BRIDGE_URL": base,
        }
    )
    assert tracker_cfg.github_token == ""
    assert tracker_cfg.github_lane == "anonymous"

    fixture = _fixture("github_pr_merged.json")

    def fetch(repo, pr, **kwargs):
        assert (repo, pr) == ("agentculture/culture-nodes", 54)
        assert kwargs["token"] == ""
        return fixture

    result = tracker.MergeTracker(tracker_cfg, github_fetch=fetch).run_cycle()
    assert result.github_requests == 1
    assert result.submissions == 1
    completed = receiver.wait_for_kind("completed", timeout=10.0)
    assert completed is not None
    assert completed["payload"]["ledger_delta"]["records"][0]["authority"] == "proposed"
    assert TaskStore(cfg.state_dir).get(accepted["invocation_id"]).status == STATUS_COMPLETED


@pytest.mark.parametrize(
    ("token", "expected_authorization"),
    [("", None), ("github-secret", "Bearer github-secret")],
)
def test_github_request_authentication_follows_token_lane(
    monkeypatch, token, expected_authorization
):
    seen = []

    class Response(io.BytesIO):
        def __enter__(self):
            return self

        def __exit__(self, *args):
            self.close()

    def urlopen(request, **kwargs):
        seen.append(request)
        return Response(b'{"merged": false}')

    monkeypatch.setattr(tracker.urllib.request, "urlopen", urlopen)
    assert tracker.fetch_github_pull(
        "agentculture/culture-nodes", 64, token=token, timeout_seconds=5.0
    ) == {"merged": False}
    assert seen[0].get_header("Authorization") == expected_authorization


def test_closed_unmerged_pr_does_not_submit(live):
    base, cfg, receiver = live
    _, accepted = _invoke(
        base,
        receiver,
        idem_key="att_merge_55",
        observe={"kind": "github_pr_merged", "pr": 55},
    )
    assert receiver.wait_for_kind("accepted", timeout=10.0) is not None
    submits = []
    poller = tracker.MergeTracker(
        _tracker_config(base, cfg),
        github_fetch=lambda *args, **kwargs: _fixture("github_pr_closed_unmerged.json"),
        bridge_submit=lambda *args, **kwargs: submits.append((args, kwargs)),
    )

    result = poller.run_cycle()
    assert result.github_requests == 1
    assert result.submissions == 0
    assert submits == []
    assert TaskStore(cfg.state_dir).get(accepted["invocation_id"]).status == STATUS_PENDING
    assert receiver.wait_for_kind("completed", timeout=0.1) is None


def test_manual_submit_on_observed_task_is_unchanged(live):
    base, cfg, receiver = live
    _, accepted = _invoke(
        base,
        receiver,
        idem_key="att_manual_override",
        observe={"kind": "github_pr_merged", "pr": 54},
    )
    assert receiver.wait_for_kind("accepted", timeout=10.0) is not None

    status, _body = _request(
        base,
        f"/inbox/tasks/{accepted['invocation_id']}/submit",
        body={"outcome": "dropped", "note": "operator chose not to merge"},
        headers=AUTH,
    )
    assert status == 200
    completed = receiver.wait_for_kind("completed", timeout=10.0)
    record = completed["payload"]["ledger_delta"]["records"][0]
    assert record["data"] == {
        "statement": "operator chose not to merge",
        "kind": "human-submission",
        "outcome": "dropped",
    }
    stored = TaskStore(cfg.state_dir).get(accepted["invocation_id"])
    assert "observed" not in stored.submission


def test_undeclared_tasks_are_never_checked_or_submitted(live):
    base, cfg, receiver = live
    _, accepted = _invoke(base, receiver, idem_key="att_manual_only")
    assert receiver.wait_for_kind("accepted", timeout=10.0) is not None
    poller = tracker.MergeTracker(
        _tracker_config(base, cfg),
        github_fetch=lambda *args, **kwargs: pytest.fail("undeclared task reached GitHub"),
        bridge_submit=lambda *args, **kwargs: pytest.fail("undeclared task was submitted"),
    )
    result = poller.run_cycle()
    assert result.eligible_tasks == 0
    assert result.github_requests == 0
    assert TaskStore(cfg.state_dir).get(accepted["invocation_id"]).status == STATUS_PENDING


def test_cycle_honors_github_request_budget(tmp_path):
    store = TaskStore(tmp_path / "state")
    for number in (10, 11):
        store.save(
            HumanTask(
                invocation_id=f"hit_{number}",
                status=STATUS_PENDING,
                created_at=f"2026-08-13T00:00:{number}+00:00",
                extra_input={
                    "observe": {
                        "kind": "github_pr_merged",
                        "repo": "agentculture/culture-nodes",
                        "pr": number,
                    }
                },
            )
        )
    requests = []
    poller = tracker.MergeTracker(
        tracker.TrackerConfig(
            state_dir=str(tmp_path / "state"),
            github_token="github-secret",
            github_request_budget=1,
        ),
        github_fetch=lambda repo, pr, **kwargs: requests.append((repo, pr)) or {"merged": False},
    )
    result = poller.run_cycle()
    assert requests == [("agentculture/culture-nodes", 10)]
    assert result.github_requests == 1
    assert result.budget_exhausted is True

    poller.run_cycle()
    assert requests == [
        ("agentculture/culture-nodes", 10),
        ("agentculture/culture-nodes", 11),
    ]


def test_tracker_config_uses_bridge_state_and_authenticated_lane():
    cfg = tracker.TrackerConfig.from_env(
        {
            "GITHUB_TOKEN": "github-secret",
            "HUMAN_INBOX_BRIDGE_STATE_DIR": "/bridge/state",
            "HUMAN_INBOX_BRIDGE_AUTH_TOKEN": "bridge-secret",
            "HUMAN_INBOX_TRACKER_DEFAULT_REPO": "agentculture/culture-nodes",
            "HUMAN_INBOX_TRACKER_POLL_SECONDS": "12.5",
            "HUMAN_INBOX_TRACKER_GITHUB_REQUEST_BUDGET": "7",
        }
    )
    assert cfg.state_dir == "/bridge/state"
    assert cfg.bridge_token == "bridge-secret"
    assert cfg.github_lane == "authenticated"
    assert cfg.github_requests_per_hour == 5_000
    assert cfg.poll_seconds == 12.5
    assert cfg.github_request_budget == 7


def test_tracker_config_without_token_uses_anonymous_ceiling():
    cfg = tracker.TrackerConfig.from_env({})
    assert cfg.github_token == ""
    assert cfg.github_lane == "anonymous"
    assert cfg.github_requests_per_hour == 60
    # 120s, not 60s: the clamp targets half the ceiling by default so the
    # plan survives a co-tenant on the same IP and reset-window drift. See
    # DEFAULT_RATE_UTILIZATION and the rate-headroom tests at the end of
    # this file.
    assert cfg.poll_seconds == 120.0
    assert cfg.github_request_budget == 1

    faster = tracker.TrackerConfig(
        github_token="",
        poll_seconds=10.0,
        github_request_budget=50,
    )
    assert faster.poll_seconds == 120.0
    assert faster.github_request_budget == 1

    slower = tracker.TrackerConfig(
        github_token="",
        poll_seconds=300.0,
        github_request_budget=50,
    )
    # A slower cadence earns a bigger per-cycle budget: 300s at the planned
    # 30/hour (half of 60) is 2 whole requests, not 5 — the operator's
    # declared 50 is still the ceiling, never the floor.
    assert slower.github_request_budget == 2


def test_rate_limit_backs_off_until_reset_without_completing_task(tmp_path, caplog):
    store = TaskStore(tmp_path / "state")
    task = HumanTask(
        invocation_id="hit_rate_limited",
        status=STATUS_PENDING,
        extra_input={
            "observe": {
                "kind": "github_pr_merged",
                "repo": "agentculture/culture-nodes",
                "pr": 64,
            }
        },
    )
    store.save(task)
    now = [1_000.0]
    calls = []

    def fetch(*args, **kwargs):
        calls.append((args, kwargs))
        if len(calls) == 1:
            raise urllib.error.HTTPError(
                "https://api.github.com/repos/agentculture/culture-nodes/pulls/64",
                403,
                "rate limited",
                {"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1120"},
                io.BytesIO(b'{"message":"API rate limit exceeded"}'),
            )
        return {"merged": False}

    poller = tracker.MergeTracker(
        tracker.TrackerConfig(state_dir=str(tmp_path / "state")),
        github_fetch=fetch,
        clock=lambda: now[0],
    )
    caplog.set_level(logging.INFO, logger="human_inbox_bridge.tracker")

    limited = poller.run_cycle()
    assert limited.rate_limited is True
    assert limited.retry_after_seconds == 120.0
    assert limited.submissions == 0
    assert TaskStore(tmp_path / "state").get(task.invocation_id).status == STATUS_PENDING
    assert "rate limit exhausted in anonymous lane" in caplog.text
    assert "retrying next cycle" in caplog.text

    still_backing_off = poller.run_cycle()
    assert still_backing_off.rate_limited is True
    assert len(calls) == 1
    assert "rate-limit backoff active" in caplog.text

    now[0] = 1_120.0
    retried = poller.run_cycle()
    assert retried.rate_limited is False
    assert len(calls) == 2


def test_permission_denied_403_is_not_reported_as_rate_limit(tmp_path, caplog):
    store = TaskStore(tmp_path / "state")
    store.save(
        HumanTask(
            invocation_id="hit_private_repo",
            status=STATUS_PENDING,
            extra_input={
                "observe": {
                    "kind": "github_pr_merged",
                    "repo": "agentculture/private-nodes",
                    "pr": 1,
                }
            },
        )
    )

    def denied(*args, **kwargs):
        raise urllib.error.HTTPError(
            "https://api.github.com/repos/agentculture/private-nodes/pulls/1",
            403,
            "forbidden",
            {"X-RateLimit-Remaining": "59", "X-RateLimit-Reset": "1120"},
            io.BytesIO(b'{"message":"Resource not accessible"}'),
        )

    caplog.set_level(logging.WARNING, logger="human_inbox_bridge.tracker")
    result = tracker.MergeTracker(
        tracker.TrackerConfig(state_dir=str(tmp_path / "state")),
        github_fetch=denied,
    ).run_cycle()
    assert result.rate_limited is False
    assert result.submissions == 0
    assert "permission denied (not rate limited)" in caplog.text
    assert "rate limit exhausted" not in caplog.text


def test_tracker_config_reads_the_bridge_config_file(tmp_path):
    config_file = tmp_path / "bridge.json"
    config_file.write_text(
        json.dumps({"state_dir": "/configured/state", "port": 9191}),
        encoding="utf-8",
    )
    cfg = tracker.TrackerConfig.from_env(
        {
            "GITHUB_TOKEN": "github-secret",
            "HUMAN_INBOX_BRIDGE_CONFIG": str(config_file),
            "HUMAN_INBOX_BRIDGE_AUTH_TOKEN": "secret",
        }
    )
    assert cfg.state_dir == "/configured/state"
    assert cfg.bridge_url == "http://127.0.0.1:9191"
    assert cfg.bridge_token == "secret"


def test_observation_accepts_task_repo_and_declared_success_outcome():
    task = HumanTask(
        invocation_id="hit_1",
        extra_input={
            "repo": "agentculture/culture-nodes",
            "success_outcome": "shipped",
            "observe": {"kind": "github_pr_merged", "pr": 54},
        },
    )
    assert tracker.observation_for(task, None) == tracker.Observation(
        repo="agentculture/culture-nodes", pr=54, outcome="shipped"
    )


# --- reply-kind observations (issue #71) --------------------------------
#
# The pr-upkeep decision node posts a question to a PR, notifies Discord it
# is pending, and parks on `observe: {kind: github_pr_reply, ...}` — the
# SAME park/observe/auto-submit shape github_pr_merged already proves, with
# three exits instead of one: a qualifying human reply, the PR getting
# merged anyway, or the PR closing unmerged. See tracker.py's
# `run_cycle`/`_check_reply_group` docstrings for the shared-budget
# arithmetic these tests pin.


def test_qualifying_reply_completes_with_answered_outcome(live):
    base, cfg, receiver = live
    _, accepted = _invoke(
        base,
        receiver,
        idem_key="att_reply_54",
        observe={"kind": "github_pr_reply", "repo": "agentculture/culture-nodes", "pr": 54},
    )
    assert receiver.wait_for_kind("accepted", timeout=10.0) is not None

    open_pr = _fixture("github_pr_open.json")
    comments = [
        {
            "id": 1,
            "user": {"login": "human-reviewer"},
            "body": "changes look fine, proceed",
            "html_url": "https://github.com/agentculture/culture-nodes/pull/54#comment-1",
        }
    ]

    poller = tracker.MergeTracker(
        _tracker_config(base, cfg, poll_seconds=60.0),
        github_fetch=lambda *a, **k: open_pr,
        reply_fetch=lambda *a, **k: comments,
    )
    result = poller.run_cycle()
    assert result.github_requests == 2  # state check + comments check
    assert result.submissions == 1

    completed = receiver.wait_for_kind("completed", timeout=10.0)
    assert completed is not None
    assert completed["payload"]["outcome"] == "answered"
    record = completed["payload"]["ledger_delta"]["records"][0]
    assert record["authority"] == "proposed"
    assert record["data"]["kind"] == "observed-submission"
    assert record["data"]["collection_method"] == "github_pr_reply"
    assert record["data"]["reference"] == comments[0]["html_url"]
    assert TaskStore(cfg.state_dir).get(accepted["invocation_id"]).status == STATUS_COMPLETED


def test_bot_authored_comment_is_not_a_qualifying_reply(live):
    base, cfg, receiver = live
    _, accepted = _invoke(
        base,
        receiver,
        idem_key="att_reply_bot",
        observe={"kind": "github_pr_reply", "repo": "agentculture/culture-nodes", "pr": 54},
    )
    assert receiver.wait_for_kind("accepted", timeout=10.0) is not None

    comments = [
        {"id": 1, "user": {"login": "qodo-code-review[bot]"}, "body": "nice job overall"},
    ]
    poller = tracker.MergeTracker(
        _tracker_config(base, cfg, poll_seconds=60.0),
        github_fetch=lambda *a, **k: _fixture("github_pr_open.json"),
        reply_fetch=lambda *a, **k: comments,
    )
    result = poller.run_cycle()
    assert result.submissions == 0
    assert TaskStore(cfg.state_dir).get(accepted["invocation_id"]).status == STATUS_PENDING
    assert receiver.wait_for_kind("completed", timeout=0.1) is None


def test_reply_fetch_is_scoped_since_the_task_was_parked(live):
    """GitHub's own `since` filter is the freshness half of "which reply
    counts": a comment posted before the question went up can never
    qualify, because the tracker never even asks GitHub for it."""
    base, cfg, receiver = live
    _, accepted = _invoke(
        base,
        receiver,
        idem_key="att_reply_since",
        observe={"kind": "github_pr_reply", "repo": "agentculture/culture-nodes", "pr": 54},
    )
    assert receiver.wait_for_kind("accepted", timeout=10.0) is not None
    parked_created_at = TaskStore(cfg.state_dir).get(accepted["invocation_id"]).created_at
    assert parked_created_at

    seen_since = []

    def reply_fetch(repo, pr, *, since, **kwargs):
        seen_since.append(since)
        return []

    poller = tracker.MergeTracker(
        _tracker_config(base, cfg, poll_seconds=60.0),
        github_fetch=lambda *a, **k: _fixture("github_pr_open.json"),
        reply_fetch=reply_fetch,
    )
    poller.run_cycle()
    assert seen_since == [parked_created_at]


def test_merged_pr_releases_a_pending_reply_wait(live):
    base, cfg, receiver = live
    _, accepted = _invoke(
        base,
        receiver,
        idem_key="att_reply_merged",
        observe={"kind": "github_pr_reply", "repo": "agentculture/culture-nodes", "pr": 54},
    )
    assert receiver.wait_for_kind("accepted", timeout=10.0) is not None

    poller = tracker.MergeTracker(
        _tracker_config(base, cfg),
        github_fetch=lambda *a, **k: _fixture("github_pr_merged.json"),
        reply_fetch=lambda *a, **k: pytest.fail("comments fetched after a terminal merge state"),
    )
    result = poller.run_cycle()
    assert result.github_requests == 1  # terminal state short-circuits the comments call
    assert result.submissions == 1

    completed = receiver.wait_for_kind("completed", timeout=10.0)
    assert completed["payload"]["outcome"] == "merged"
    record = completed["payload"]["ledger_delta"]["records"][0]
    assert record["data"]["collection_method"] == "github_pr_merged"
    assert record["data"]["merge_commit"] == _fixture("github_pr_merged.json")["merge_commit_sha"]


def test_closed_unmerged_pr_releases_a_pending_reply_wait_as_dropped(live):
    base, cfg, receiver = live
    _, accepted = _invoke(
        base,
        receiver,
        idem_key="att_reply_closed",
        observe={"kind": "github_pr_reply", "repo": "agentculture/culture-nodes", "pr": 55},
    )
    assert receiver.wait_for_kind("accepted", timeout=10.0) is not None

    poller = tracker.MergeTracker(
        _tracker_config(base, cfg),
        github_fetch=lambda *a, **k: _fixture("github_pr_closed_unmerged.json"),
        reply_fetch=lambda *a, **k: pytest.fail("comments fetched after a terminal close state"),
    )
    result = poller.run_cycle()
    assert result.github_requests == 1
    assert result.submissions == 1

    completed = receiver.wait_for_kind("completed", timeout=10.0)
    assert completed["payload"]["outcome"] == "dropped"
    record = completed["payload"]["ledger_delta"]["records"][0]
    assert record["data"]["collection_method"] == "github_pr_closed"
    assert record["data"]["reference"]


def test_reply_group_does_not_spend_a_second_request_when_budget_is_one(tmp_path):
    """The shared-budget degrade: an open PR's comment check is skipped
    (not attempted, not partially charged) when only one unit remains."""
    store = TaskStore(tmp_path / "state")
    store.save(
        HumanTask(
            invocation_id="hit_reply_1",
            status=STATUS_PENDING,
            created_at="2026-08-13T00:00:00+00:00",
            extra_input={
                "observe": {
                    "kind": "github_pr_reply",
                    "repo": "agentculture/culture-nodes",
                    "pr": 54,
                }
            },
        )
    )
    poller = tracker.MergeTracker(
        tracker.TrackerConfig(
            state_dir=str(tmp_path / "state"), github_token="github-secret", github_request_budget=1
        ),
        github_fetch=lambda *a, **k: _fixture("github_pr_open.json"),
        reply_fetch=lambda *a, **k: pytest.fail("comments fetched despite exhausted budget"),
    )
    result = poller.run_cycle()
    assert result.github_requests == 1
    assert result.submissions == 0


def test_reply_groups_are_checked_before_merge_groups_share_the_same_budget(tmp_path):
    """Issue #71: a human is actively blocked on a reply-kind group, so it
    gets first claim on the cycle's shared budget over a merge-kind group —
    without the budget itself growing (still `github_request_budget` total
    GitHub requests this cycle, merge and reply combined)."""
    store = TaskStore(tmp_path / "state")
    store.save(
        HumanTask(
            invocation_id="hit_merge_1",
            status=STATUS_PENDING,
            created_at="2026-08-13T00:00:01+00:00",
            extra_input={
                "observe": {
                    "kind": "github_pr_merged",
                    "repo": "agentculture/culture-nodes",
                    "pr": 10,
                }
            },
        )
    )
    store.save(
        HumanTask(
            invocation_id="hit_reply_1",
            status=STATUS_PENDING,
            created_at="2026-08-13T00:00:00+00:00",
            extra_input={
                "observe": {
                    "kind": "github_pr_reply",
                    "repo": "agentculture/culture-nodes",
                    "pr": 54,
                }
            },
        )
    )
    merge_requests = []
    poller = tracker.MergeTracker(
        tracker.TrackerConfig(
            state_dir=str(tmp_path / "state"), github_token="github-secret", github_request_budget=1
        ),
        github_fetch=lambda repo, pr, **k: merge_requests.append(pr)
        or _fixture("github_pr_open.json"),
        reply_fetch=lambda *a, **k: [],
    )
    result = poller.run_cycle()
    # The whole budget (1) went to the reply group's terminal-state check;
    # the merge-kind group waits for the next cycle.
    assert merge_requests == [54]
    assert result.github_requests == 1
    assert result.budget_exhausted is True


def test_reply_observation_ignores_malformed_declarations():
    task = HumanTask(
        invocation_id="hit_1",
        created_at="2026-08-13T00:00:00+00:00",
        extra_input={"observe": {"kind": "github_pr_reply", "pr": "not-an-int"}},
    )
    assert tracker.reply_observation_for(task, "agentculture/culture-nodes") is None

    task2 = HumanTask(
        invocation_id="hit_2",
        created_at="2026-08-13T00:00:00+00:00",
        extra_input={"observe": {"kind": "something_else", "pr": 1}},
    )
    assert tracker.reply_observation_for(task2, "agentculture/culture-nodes") is None


def test_reply_observation_accepts_custom_outcome_names():
    task = HumanTask(
        invocation_id="hit_1",
        created_at="2026-08-13T00:00:00+00:00",
        extra_input={
            "observe": {
                "kind": "github_pr_reply",
                "repo": "agentculture/culture-nodes",
                "pr": 54,
                "answered_outcome": "resolved",
                "merged_outcome": "shipped",
                "dropped_outcome": "abandoned",
            }
        },
    )
    obs = tracker.reply_observation_for(task, None)
    assert obs == tracker.ReplyObservation(
        repo="agentculture/culture-nodes",
        pr=54,
        since="2026-08-13T00:00:00+00:00",
        answered_outcome="resolved",
        merged_outcome="shipped",
        dropped_outcome="abandoned",
    )


def test_qualifying_reply_helper_skips_ignored_authors_and_empty_bodies():
    comments = [
        {"user": {"login": "qodo-code-review[bot]"}, "body": "automated finding"},
        {"user": {"login": "human-reviewer"}, "body": "   "},
        {"user": {"login": "human-reviewer"}, "body": "looks good, ship it"},
    ]
    reply = tracker.qualifying_reply(comments, ignored_logins=tracker.DEFAULT_REPLY_IGNORED_LOGINS)
    assert reply is not None
    assert reply["body"] == "looks good, ship it"


def test_default_reply_ignored_logins_are_always_included_with_env_additions():
    cfg = tracker.TrackerConfig.from_env(
        {"HUMAN_INBOX_TRACKER_REPLY_IGNORED_LOGINS": "pr-upkeep-fix-bot"}
    )
    assert cfg.reply_ignored_logins == frozenset({"qodo-code-review[bot]", "pr-upkeep-fix-bot"})


def test_default_reply_ignored_logins_without_env_override():
    cfg = tracker.TrackerConfig.from_env({})
    assert cfg.reply_ignored_logins == tracker.DEFAULT_REPLY_IGNORED_LOGINS


# --- rate headroom -----------------------------------------------------
#
# The first cut of the lane clamp planned to spend GitHub's whole hourly
# ceiling: anonymous came out at exactly 60 requests/hour against a 60/hour
# limit. That is a plan to be refused. The quota is counted per source IP, so
# the tracker shares it with anything else on the host, and our cycle clock
# drifts against GitHub's reset window — at the boundary two cycles land in
# one window. These pin the headroom rather than the arithmetic that
# happened to produce it.


def _planned_requests_per_hour(cfg) -> float:
    return cfg.github_request_budget * 3600.0 / cfg.poll_seconds


def test_anonymous_lane_plans_below_the_anonymous_ceiling():
    cfg = tracker.TrackerConfig(github_token="")
    assert cfg.github_lane == "anonymous"
    planned = _planned_requests_per_hour(cfg)
    assert planned < tracker.ANONYMOUS_REQUESTS_PER_HOUR, (
        f"anonymous lane plans {planned}/hour against a "
        f"{tracker.ANONYMOUS_REQUESTS_PER_HOUR}/hour ceiling — no room for a "
        "co-tenant on the same IP or for reset-window drift"
    )


def test_authenticated_lane_plans_below_the_authenticated_ceiling():
    cfg = tracker.TrackerConfig(github_token="ghp_example")
    assert cfg.github_lane == "authenticated"
    assert _planned_requests_per_hour(cfg) < tracker.AUTHENTICATED_REQUESTS_PER_HOUR


def test_headroom_is_spent_on_cadence_not_on_dropping_below_one_request():
    """A cycle must still be able to check one PR — slowing down is the
    honest lever, never a zero budget that silently observes nothing."""
    cfg = tracker.TrackerConfig(github_token="")
    assert cfg.github_request_budget >= 1
    assert cfg.poll_seconds > 60.0


def test_sole_tenant_host_can_reclaim_the_full_ceiling():
    cfg = tracker.TrackerConfig(github_token="", rate_utilization=1.0)
    assert _planned_requests_per_hour(cfg) == tracker.ANONYMOUS_REQUESTS_PER_HOUR


def test_rate_utilization_is_read_from_the_environment(tmp_path):
    config_file = tmp_path / "bridge.json"
    config_file.write_text(json.dumps({"state_dir": str(tmp_path / "state"), "port": 9191}))
    cfg = tracker.TrackerConfig.from_env(
        {
            "HUMAN_INBOX_BRIDGE_CONFIG": str(config_file),
            "HUMAN_INBOX_BRIDGE_AUTH_TOKEN": "secret",
            "HUMAN_INBOX_TRACKER_RATE_UTILIZATION": "1.0",
        }
    )
    assert cfg.rate_utilization == 1.0
    assert _planned_requests_per_hour(cfg) == tracker.ANONYMOUS_REQUESTS_PER_HOUR


@pytest.mark.parametrize("bad", [0.0, -0.5, 1.5])
def test_out_of_range_utilization_is_refused_with_a_hint(bad):
    with pytest.raises(tracker.TrackerConfigError) as excinfo:
        tracker.TrackerConfig(rate_utilization=bad)
    message = str(excinfo.value)
    assert "rate_utilization" in message
    assert "HUMAN_INBOX_TRACKER_RATE_UTILIZATION" in message


# --- startup identity check (task t8, issue #72) -----------------------
#
# The tracker submits through ONE bridge's authenticated surface, and that
# bridge's idempotency store is per-bridge and file-based (one JSON file per
# key under Config.state_dir). Two bridges serving one actor therefore
# cannot deduplicate each other's submissions — deployment convention is the
# only thing keeping "one logical human inbox" true, and this startup check
# is the only mechanism that can notice when it stops being true. These
# tests pin the refusal, not the arithmetic of the comparison.

ACTOR_KEY = "company/human-ops"


def _actor_row(endpoint: str, *, revision: int = 1, actor_key: str = ACTOR_KEY) -> dict:
    return {
        "id": f"actor_register_{revision}",
        "actor_key": actor_key,
        "revision": revision,
        "kind": "human",
        "protocol": "http",
        "endpoint_ref": endpoint,
    }


def _identity_config(**overrides) -> tracker.TrackerConfig:
    fields = {
        "state_dir": "/unused/state",
        "bridge_url": "http://127.0.0.1:8087",
        "actor_id": ACTOR_KEY,
        "control_plane_url": "http://192.168.1.5:18080",
        "github_token": "github-secret",
    }
    fields.update(overrides)
    return tracker.TrackerConfig(**fields)


def _fetch(*rows):
    def fetch(control_plane_url, **kwargs):
        assert control_plane_url == "http://192.168.1.5:18080"
        return list(rows)

    return fetch


def _local(*addresses):
    """A locality oracle for a host that answers on *addresses*."""

    def is_local(host: str) -> bool:
        return host in set(addresses) | {"127.0.0.1", "::1", "localhost"}

    return is_local


def test_bridge_serving_a_different_actor_is_refused_naming_both_endpoints():
    """The production split this exists for: the tracker runs on thor with a
    loopback bridge URL while company/human-ops is registered on spark."""
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        tracker.verify_bridge_serves_actor(
            _identity_config(),
            actor_fetch=_fetch(_actor_row("http://192.168.1.157:8090")),
            is_local_address=_local("192.168.1.10"),  # this host is thor, not spark
        )
    message = str(excinfo.value)
    assert "http://192.168.1.157:8090" in message  # the actor's endpoint
    assert "http://127.0.0.1:8087" in message  # this tracker's own bridge
    assert ACTOR_KEY in message
    # The consequence, not just the mismatch: an operator reading this line
    # needs to know why it is fatal rather than cosmetic.
    assert "idempotenc" in message.lower()


def test_main_exits_non_zero_when_the_bridge_serves_a_different_actor(monkeypatch, capsys):
    """Acceptance: the refusal is an exit code, not only an exception."""
    monkeypatch.setattr(
        tracker, "fetch_actors", lambda url, **kwargs: [_actor_row("http://192.168.1.157:8090")]
    )
    monkeypatch.setattr(tracker, "_is_local_address", lambda host: host.startswith("127."))
    for name in ("HUMAN_INBOX_BRIDGE_CONFIG", "GITHUB_TOKEN"):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("HUMAN_INBOX_BRIDGE_ACTOR_ID", ACTOR_KEY)
    monkeypatch.setenv("HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL", "http://192.168.1.5:18080")

    rc = tracker.main(["--once"])

    assert rc != 0
    err = capsys.readouterr().err
    assert "http://192.168.1.157:8090" in err
    assert "http://127.0.0.1:8087" in err


def test_co_located_bridge_passes_though_the_urls_differ_textually():
    """The tracker on the actor's own host addresses it as loopback while
    the actor row names the LAN address — the same bridge, so no refusal."""
    resolved = tracker.verify_bridge_serves_actor(
        _identity_config(bridge_url="http://127.0.0.1:8090"),
        actor_fetch=_fetch(_actor_row("http://192.168.1.157:8090")),
        is_local_address=_local("192.168.1.157"),
    )
    assert resolved is not None
    assert resolved.endpoint_ref == "http://192.168.1.157:8090"


def test_identical_endpoints_need_no_locality_resolution():
    def never(host: str) -> bool:  # pragma: no cover - must not be called
        raise AssertionError(f"resolved locality for {host} on an exact match")

    resolved = tracker.verify_bridge_serves_actor(
        _identity_config(bridge_url="http://192.168.1.157:8090"),
        actor_fetch=_fetch(_actor_row("http://192.168.1.157:8090")),
        is_local_address=never,
    )
    assert resolved is not None


def test_two_spellings_of_one_address_are_one_bridge():
    """A remote-but-correct pairing: the tracker points straight at the
    actor's endpoint, written differently. Same address, so no locality
    question arises and none is asked."""

    def never(host: str) -> bool:  # pragma: no cover - must not be called
        raise AssertionError(f"resolved locality for {host} on equal addresses")

    resolved = tracker.verify_bridge_serves_actor(
        _identity_config(bridge_url="http://[0:0:0:0:0:0:0:1]:8090"),
        actor_fetch=_fetch(_actor_row("http://[::1]:8090")),
        is_local_address=never,
    )
    assert resolved is not None


def test_same_port_on_another_host_is_still_a_mismatch():
    """Matching ports must not be mistaken for the same bridge — the split
    deployment this guards against had both bridges on the same port."""
    with pytest.raises(tracker.BridgeIdentityError):
        tracker.verify_bridge_serves_actor(
            _identity_config(bridge_url="http://127.0.0.1:8087"),
            actor_fetch=_fetch(_actor_row("http://192.168.1.157:8087")),
            is_local_address=_local("192.168.1.10"),
        )


def test_second_bridge_on_the_same_host_is_a_mismatch():
    """Same machine, different port: two bridge processes, two idempotency
    stores, and only one of them is the actor's."""
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        tracker.verify_bridge_serves_actor(
            _identity_config(bridge_url="http://127.0.0.1:8087"),
            actor_fetch=_fetch(_actor_row("http://192.168.1.157:8090")),
            is_local_address=_local("192.168.1.157"),
        )
    assert "8090" in str(excinfo.value)


def test_newest_revision_decides_the_endpoint():
    """Actor identity is append-only: an endpoint move is a new revision, so
    an older matching row must not excuse a moved actor."""
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        tracker.verify_bridge_serves_actor(
            _identity_config(bridge_url="http://127.0.0.1:8087"),
            actor_fetch=_fetch(
                _actor_row("http://192.168.1.157:8087", revision=1),
                _actor_row("http://192.168.1.99:8087", revision=2),
            ),
            is_local_address=_local("192.168.1.157"),
        )
    message = str(excinfo.value)
    assert "http://192.168.1.99:8087" in message
    assert "revision 2" in message


def test_other_actors_rows_are_ignored():
    resolved = tracker.verify_bridge_serves_actor(
        _identity_config(bridge_url="http://127.0.0.1:8090"),
        actor_fetch=_fetch(
            _actor_row("http://192.168.1.99:8086", revision=7, actor_key="company/codex-thor"),
            _actor_row("http://192.168.1.157:8090"),
        ),
        is_local_address=_local("192.168.1.157"),
    )
    assert resolved is not None and resolved.actor_key == ACTOR_KEY


def test_unregistered_actor_is_refused():
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        tracker.verify_bridge_serves_actor(
            _identity_config(),
            actor_fetch=_fetch(
                _actor_row("http://192.168.1.99:8086", revision=1, actor_key="company/developer")
            ),
            is_local_address=_local(),
        )
    message = str(excinfo.value)
    assert ACTOR_KEY in message
    assert "http://192.168.1.5:18080" in message
    assert "HUMAN_INBOX_BRIDGE_ACTOR_ID" in message


def test_unreachable_control_plane_fails_closed():
    """An unverifiable identity is not a verified one. The unit restarts, so
    a control plane that is merely restarting costs a retry, not a silent
    window in which a split deployment could double-submit."""

    def fetch(control_plane_url, **kwargs):
        raise urllib.error.URLError("connection refused")

    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        tracker.verify_bridge_serves_actor(
            _identity_config(), actor_fetch=fetch, is_local_address=_local()
        )
    assert "http://192.168.1.5:18080" in str(excinfo.value)


def test_unconfigured_control_plane_warns_that_the_guard_is_inactive(caplog):
    with caplog.at_level(logging.WARNING, logger="human_inbox_bridge.tracker"):
        resolved = tracker.verify_bridge_serves_actor(
            _identity_config(control_plane_url=""),
            actor_fetch=_fetch(),
            is_local_address=_local(),
        )
    assert resolved is None
    assert "HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL" in caplog.text
    assert "idempotenc" in caplog.text.lower()


def test_tracker_config_carries_the_actor_id_and_control_plane_url():
    cfg = tracker.TrackerConfig.from_env(
        {
            "HUMAN_INBOX_BRIDGE_ACTOR_ID": ACTOR_KEY,
            "HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL": "http://192.168.1.5:18080/",
        }
    )
    assert cfg.actor_id == ACTOR_KEY
    # Trailing slash stripped, exactly as bridge_url is.
    assert cfg.control_plane_url == "http://192.168.1.5:18080"


def test_tracker_config_control_plane_url_is_unset_by_default():
    cfg = tracker.TrackerConfig.from_env({})
    assert cfg.control_plane_url == ""
    assert cfg.actor_id == "human-inbox-bridge"


@pytest.mark.parametrize("bad", ["", "not a url", "192.168.1.157:8090", "http://"])
def test_unusable_endpoint_ref_is_refused(bad):
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        tracker.verify_bridge_serves_actor(
            _identity_config(),
            actor_fetch=_fetch(_actor_row(bad)),
            is_local_address=_local(),
        )
    assert ACTOR_KEY in str(excinfo.value)
