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
