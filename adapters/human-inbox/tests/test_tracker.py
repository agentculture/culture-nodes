"""Recorded-fixture tests for the merge-as-action tracker (plan t16)."""

from __future__ import annotations

import json
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


def test_tracker_config_uses_bridge_state_and_requires_github_token():
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
    assert cfg.poll_seconds == 12.5
    assert cfg.github_request_budget == 7
    with pytest.raises(tracker.TrackerConfigError):
        tracker.TrackerConfig.from_env({})


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
