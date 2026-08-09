"""Tests for `nodes human-tasks` — list/get/decide against a fake API."""

from __future__ import annotations

import json

from culture_nodes.cli import main

# --- list --------------------------------------------------------------------


def test_human_tasks_list_text_and_query(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["query"] = q
        h.send_json(
            200,
            {
                "items": [
                    {
                        "id": "task-1",
                        "run_id": "run-1",
                        "status": "pending",
                        "kind": "approval",
                        "created_at": "t",
                    }
                ]
            },
        )

    fake_api.route("GET", r"/v1alpha1/human-tasks", handler)
    fake_api.start()
    rc = main(["human-tasks", "list", "--status", "pending", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "task-1" in out
    assert "pending" in out
    assert seen["query"] == {"status": ["pending"]}


def test_human_tasks_list_no_items(fake_api, capsys) -> None:
    fake_api.route(
        "GET", r"/v1alpha1/human-tasks", lambda h, m, q, b: h.send_json(200, {"items": []})
    )
    fake_api.start()
    rc = main(["human-tasks", "list", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "no human tasks" in out


def test_human_tasks_list_json_passthrough_byte_exact(fake_api, capsys) -> None:
    payload = {"items": []}
    fake_api.route(
        "GET", r"/v1alpha1/human-tasks", lambda h, m, q, b: _write_compact(h, 200, payload)
    )
    fake_api.start()
    rc = main(["human-tasks", "list", "--json", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert out == json.dumps(payload, separators=(",", ":")) + "\n"


def _write_compact(handler, status: int, payload: object) -> None:
    body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    handler.send_response(status)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)


# --- get -----------------------------------------------------------------------


def test_human_tasks_get_text(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/human-tasks/(?P<id>[^/]+)",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "id": "task-1",
                "run_id": "run-1",
                "status": "pending",
                "kind": "approval",
                "request": {},
                "created_at": "t",
            },
        ),
    )
    fake_api.start()
    rc = main(["human-tasks", "get", "task-1", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "id: task-1" in out
    assert "status: pending" in out


def test_human_tasks_get_404(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/human-tasks/(?P<id>[^/]+)",
        lambda h, m, q, b: h.send_json(
            404,
            {"code": 1, "message": "no human task with id bogus", "remediation": "check the id"},
        ),
    )
    fake_api.start()
    rc = main(["human-tasks", "get", "bogus", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "no human task with id bogus" in captured.err
    assert "hint: check the id" in captured.err


def test_human_tasks_get_json_passthrough_byte_exact(fake_api, capsys) -> None:
    payload = {
        "id": "task-1",
        "run_id": "run-1",
        "status": "pending",
        "kind": "approval",
        "request": {},
        "created_at": "t",
    }
    fake_api.route(
        "GET",
        r"/v1alpha1/human-tasks/(?P<id>[^/]+)",
        lambda h, m, q, b: _write_compact(h, 200, payload),
    )
    fake_api.start()
    rc = main(["human-tasks", "get", "task-1", "--json", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert out == json.dumps(payload, separators=(",", ":")) + "\n"


# --- decide --------------------------------------------------------------------


def test_human_tasks_decide_sends_bearer_token_from_flag(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["auth"] = h.headers.get("Authorization")
        seen["body"] = json.loads(b)
        h.send_json(
            200,
            {
                "human_task_id": "task-1",
                "run_id": "run-1",
                "node_run_id": "nr-1",
                "outcome": "approved",
                "ledger_records": [],
                "run_state": "running",
            },
        )

    fake_api.route("POST", r"/v1alpha1/human-tasks/(?P<id>[^/]+)/decision", handler)
    fake_api.start()
    rc = main(
        [
            "human-tasks",
            "decide",
            "task-1",
            "--outcome",
            "approved",
            "--decider-actor-id",
            "human:alice",
            "--expected-ledger-version",
            "3",
            "--token",
            "s3cr3t-token",
            "--api-url",
            fake_api.base_url,
        ]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert seen["auth"] == "Bearer s3cr3t-token"
    assert seen["body"] == {
        "outcome": "approved",
        "decider_actor_id": "human:alice",
        "expected_ledger_version": 3,
    }
    assert "outcome: approved" in out
    assert "s3cr3t-token" not in out


def test_human_tasks_decide_sends_bearer_token_from_env(fake_api, capsys, monkeypatch) -> None:
    monkeypatch.setenv("NODES_HUMAN_DECISION_TOKEN", "env-token")
    seen = {}

    def handler(h, m, q, b):
        seen["auth"] = h.headers.get("Authorization")
        h.send_json(
            200,
            {
                "human_task_id": "task-1",
                "run_id": "run-1",
                "outcome": "approved",
                "run_state": "running",
            },
        )

    fake_api.route("POST", r"/v1alpha1/human-tasks/(?P<id>[^/]+)/decision", handler)
    fake_api.start()
    rc = main(
        [
            "human-tasks",
            "decide",
            "task-1",
            "--outcome",
            "approved",
            "--decider-actor-id",
            "human:alice",
            "--expected-ledger-version",
            "3",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert rc == 0
    assert seen["auth"] == "Bearer env-token"
    assert "env-token" not in capsys.readouterr().out


def test_human_tasks_decide_flag_token_overrides_env(fake_api, monkeypatch) -> None:
    monkeypatch.setenv("NODES_HUMAN_DECISION_TOKEN", "env-token")
    seen = {}

    def handler(h, m, q, b):
        seen["auth"] = h.headers.get("Authorization")
        h.send_json(
            200,
            {
                "human_task_id": "task-1",
                "run_id": "run-1",
                "outcome": "x",
                "run_state": "running",
            },
        )

    fake_api.route("POST", r"/v1alpha1/human-tasks/(?P<id>[^/]+)/decision", handler)
    fake_api.start()
    main(
        [
            "human-tasks",
            "decide",
            "task-1",
            "--outcome",
            "x",
            "--decider-actor-id",
            "human:alice",
            "--expected-ledger-version",
            "1",
            "--token",
            "flag-token",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert seen["auth"] == "Bearer flag-token"


def test_human_tasks_decide_note_and_record_ids(fake_api) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(
            200,
            {
                "human_task_id": "task-1",
                "run_id": "run-1",
                "outcome": "approved",
                "run_state": "running",
            },
        )

    fake_api.route("POST", r"/v1alpha1/human-tasks/(?P<id>[^/]+)/decision", handler)
    fake_api.start()
    main(
        [
            "human-tasks",
            "decide",
            "task-1",
            "--outcome",
            "approved",
            "--decider-actor-id",
            "human:alice",
            "--expected-ledger-version",
            "3",
            "--note",
            "looks good",
            "--record-ids",
            "rec-1,rec-2",
            "--token",
            "tok",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert seen["body"]["response"] == {"note": "looks good"}
    assert seen["body"]["record_ids"] == ["rec-1", "rec-2"]


def test_human_tasks_decide_missing_token_exit_1(fake_api, capsys, monkeypatch) -> None:
    monkeypatch.delenv("NODES_HUMAN_DECISION_TOKEN", raising=False)
    fake_api.start()
    rc = main(
        [
            "human-tasks",
            "decide",
            "task-1",
            "--outcome",
            "approved",
            "--decider-actor-id",
            "human:alice",
            "--expected-ledger-version",
            "3",
            "--api-url",
            fake_api.base_url,
        ]
    )
    captured = capsys.readouterr()
    assert rc == 1
    assert captured.err.startswith("error:")
    assert "hint:" in captured.err
    assert "NODES_HUMAN_DECISION_TOKEN" in captured.err


def test_human_tasks_decide_401_wrong_token_never_logged(fake_api, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/human-tasks/(?P<id>[^/]+)/decision",
        lambda h, m, q, b: h.send_json(
            401,
            {
                "code": 1,
                "message": "invalid or missing decision bearer token",
                "remediation": "pass a valid Authorization: Bearer token",
            },
        ),
    )
    fake_api.start()
    rc = main(
        [
            "human-tasks",
            "decide",
            "task-1",
            "--outcome",
            "approved",
            "--decider-actor-id",
            "human:alice",
            "--expected-ledger-version",
            "3",
            "--token",
            "wrong-token",
            "--api-url",
            fake_api.base_url,
        ]
    )
    captured = capsys.readouterr()
    assert rc == 1
    assert "invalid or missing decision bearer token" in captured.err
    assert "wrong-token" not in captured.err
    assert "wrong-token" not in captured.out


def test_human_tasks_decide_409_already_decided(fake_api, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/human-tasks/(?P<id>[^/]+)/decision",
        lambda h, m, q, b: h.send_json(
            409,
            {
                "code": 1,
                "message": "human task task-1 is already decided",
                "remediation": "a task decides exactly once",
            },
        ),
    )
    fake_api.start()
    rc = main(
        [
            "human-tasks",
            "decide",
            "task-1",
            "--outcome",
            "approved",
            "--decider-actor-id",
            "human:alice",
            "--expected-ledger-version",
            "3",
            "--token",
            "tok",
            "--api-url",
            fake_api.base_url,
        ]
    )
    captured = capsys.readouterr()
    assert rc == 1
    assert "already decided" in captured.err


def test_human_tasks_decide_json_passthrough_byte_exact(fake_api, capsys) -> None:
    payload = {
        "human_task_id": "task-1",
        "run_id": "run-1",
        "outcome": "approved",
        "run_state": "running",
    }
    fake_api.route(
        "POST",
        r"/v1alpha1/human-tasks/(?P<id>[^/]+)/decision",
        lambda h, m, q, b: _write_compact(h, 200, payload),
    )
    fake_api.start()
    rc = main(
        [
            "human-tasks",
            "decide",
            "task-1",
            "--outcome",
            "approved",
            "--decider-actor-id",
            "human:alice",
            "--expected-ledger-version",
            "3",
            "--token",
            "tok",
            "--json",
            "--api-url",
            fake_api.base_url,
        ]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert out == json.dumps(payload, separators=(",", ":")) + "\n"


# --- bare noun -------------------------------------------------------------


def test_human_tasks_bare_noun(capsys) -> None:
    rc = main(["human-tasks"])
    assert rc == 0
    assert "usage: nodes human-tasks" in capsys.readouterr().out
