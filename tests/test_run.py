"""Tests for `nodes run` — create/list/get/cancel/events against a fake API."""

from __future__ import annotations

import json
from pathlib import Path

from culture_nodes.cli import main

# --- create ------------------------------------------------------------------


def test_run_create_text(fake_api, tmp_path: Path, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/runs",
        lambda h, m, q, b: h.send_json(
            201,
            {
                "id": "run-1",
                "workflow_digest": "sha256:abc",
                "state": "running",
                "created_at": "2026-01-01T00:00:00Z",
                "updated_at": "2026-01-01T00:00:00Z",
            },
        ),
    )
    fake_api.start()
    rc = main(["run", "create", "--workflow", "sha256:abc", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "id: run-1" in out
    assert "state: running" in out


def test_run_create_reads_input_from_file(fake_api, tmp_path: Path) -> None:
    input_file = tmp_path / "input.json"
    input_file.write_text('{"foo": "bar"}', encoding="utf-8")
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(
            201, {"id": "run-1", "workflow_digest": "d", "state": "running", "created_at": "t"}
        )

    fake_api.route("POST", r"/v1alpha1/runs", handler)
    fake_api.start()
    main(
        [
            "run",
            "create",
            "--workflow",
            "sha256:abc",
            "--input",
            str(input_file),
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert seen["body"] == {"workflow_digest": "sha256:abc", "input": {"foo": "bar"}}


def test_run_create_reads_input_from_stdin(fake_api, monkeypatch) -> None:
    import io

    monkeypatch.setattr("sys.stdin", io.StringIO('{"x": 1}'))
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(
            201, {"id": "run-1", "workflow_digest": "d", "state": "running", "created_at": "t"}
        )

    fake_api.route("POST", r"/v1alpha1/runs", handler)
    fake_api.start()
    main(
        [
            "run",
            "create",
            "--workflow",
            "sha256:abc",
            "--input",
            "-",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert seen["body"]["input"] == {"x": 1}


def test_run_create_bad_input_json_exit_1(fake_api, tmp_path: Path, capsys) -> None:
    bad = tmp_path / "bad.json"
    bad.write_text("{not json", encoding="utf-8")
    fake_api.start()
    rc = main(
        [
            "run",
            "create",
            "--workflow",
            "sha256:abc",
            "--input",
            str(bad),
            "--api-url",
            fake_api.base_url,
        ]
    )
    captured = capsys.readouterr()
    assert rc == 1
    assert "not valid JSON" in captured.err


def test_run_create_404_unknown_digest(fake_api, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/runs",
        lambda h, m, q, b: h.send_json(
            404,
            {
                "code": 1,
                "message": "no workflow version with digest sha256:missing",
                "remediation": "publish the workflow first via POST /v1alpha1/workflows",
            },
        ),
    )
    fake_api.start()
    rc = main(["run", "create", "--workflow", "sha256:missing", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "no workflow version with digest sha256:missing" in captured.err
    assert "hint: publish the workflow first" in captured.err


def test_run_create_json_passthrough_byte_exact(fake_api, capsys) -> None:
    payload = {
        "id": "run-1",
        "workflow_digest": "sha256:abc",
        "state": "running",
        "created_at": "t",
    }
    fake_api.route(
        "POST",
        r"/v1alpha1/runs",
        lambda h, m, q, b: _write_compact(h, 201, payload),
    )
    fake_api.start()
    rc = main(
        ["run", "create", "--workflow", "sha256:abc", "--json", "--api-url", fake_api.base_url]
    )
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


# --- list / get / cancel ------------------------------------------------------


def test_run_list_text_and_query(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["query"] = q
        h.send_json(
            200,
            {
                "items": [
                    {
                        "id": "run-1",
                        "state": "completed",
                        "workflow_digest": "sha256:abc",
                        "created_at": "t",
                    }
                ]
            },
        )

    fake_api.route("GET", r"/v1alpha1/runs", handler)
    fake_api.start()
    rc = main(["run", "list", "--state", "completed", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "run-1" in out
    assert "completed" in out
    assert seen["query"] == {"state": ["completed"]}


def test_run_get_text(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/runs/(?P<id>[^/]+)",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "run": {"id": "run-1", "state": "running", "workflow_digest": "sha256:abc"},
                "tokens": [{"id": "tok-1"}],
                "node_runs": [
                    {"id": "nr-1", "node_id": "intake", "state": "completed", "visit_count": 1}
                ],
            },
        ),
    )
    fake_api.start()
    rc = main(["run", "get", "run-1", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "id: run-1" in out
    assert "tokens: 1" in out
    assert "node_runs: 1" in out
    assert "intake: completed" in out


def test_run_cancel_text(fake_api, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/runs/(?P<id>[^/]+)/cancel",
        lambda h, m, q, b: h.send_json(200, {"id": "run-1", "state": "cancelled"}),
    )
    fake_api.start()
    rc = main(["run", "cancel", "run-1", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "run run-1 is now cancelled" in out


def test_run_cancel_409_terminal(fake_api, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/runs/(?P<id>[^/]+)/cancel",
        lambda h, m, q, b: h.send_json(
            409,
            {
                "code": 1,
                "message": "run run-1 is already completed",
                "remediation": "the run has already reached a terminal state",
            },
        ),
    )
    fake_api.start()
    rc = main(["run", "cancel", "run-1", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "run run-1 is already completed" in captured.err
    assert "hint: the run has already reached a terminal state" in captured.err


# --- events (SSE) --------------------------------------------------------------


def _sse_route(frames):
    def handler(h, m, q, b):
        h.start_sse()
        for seq, event_type, data in frames:
            h.send_sse_frame(seq, event_type, data)
        h.close_after_response()

    return handler


def test_run_events_text_mode(fake_api, capsys) -> None:
    frames = [
        (1, "run.started", {"run_id": "run-1"}),
        (2, "run.completed", {"run_id": "run-1", "state": "completed"}),
    ]
    fake_api.route("GET", r"/v1alpha1/runs/(?P<id>[^/]+)/events", _sse_route(frames))
    fake_api.start()
    rc = main(["run", "events", "run-1", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    lines = out.strip("\n").split("\n")
    assert rc == 0
    assert lines[0] == '[1] run.started: {"run_id":"run-1"}'
    assert lines[1] == '[2] run.completed: {"run_id":"run-1","state":"completed"}'


def test_run_events_json_mode(fake_api, capsys) -> None:
    frames = [
        (1, "run.started", {"run_id": "run-1"}),
        (2, "run.completed", {"run_id": "run-1", "state": "completed"}),
    ]
    fake_api.route("GET", r"/v1alpha1/runs/(?P<id>[^/]+)/events", _sse_route(frames))
    fake_api.start()
    rc = main(["run", "events", "run-1", "--json", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    lines = [json.loads(line) for line in out.strip("\n").split("\n")]
    assert rc == 0
    assert lines[0] == {"id": "1", "event": "run.started", "data": {"run_id": "run-1"}}
    assert lines[1] == {
        "id": "2",
        "event": "run.completed",
        "data": {"run_id": "run-1", "state": "completed"},
    }


def test_run_events_stops_at_terminal_event_even_with_more_frames(fake_api, capsys) -> None:
    # A server that (incorrectly) kept writing past a terminal event should
    # not make the client hang or over-print: the client itself breaks on
    # the first terminal event type it sees.
    frames = [
        (1, "run.completed", {"run_id": "run-1"}),
        (2, "run.started", {"run_id": "run-1", "unexpected": True}),
    ]
    fake_api.route("GET", r"/v1alpha1/runs/(?P<id>[^/]+)/events", _sse_route(frames))
    fake_api.start()
    rc = main(["run", "events", "run-1", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    lines = out.strip("\n").split("\n")
    assert rc == 0
    assert len(lines) == 1
    assert lines[0] == '[1] run.completed: {"run_id":"run-1"}'


def test_run_events_follow_flag_accepted(fake_api, capsys) -> None:
    frames = [(1, "run.completed", {"run_id": "run-1"})]
    fake_api.route("GET", r"/v1alpha1/runs/(?P<id>[^/]+)/events", _sse_route(frames))
    fake_api.start()
    rc = main(["run", "events", "run-1", "--follow", "--api-url", fake_api.base_url])
    assert rc == 0
    assert "run.completed" in capsys.readouterr().out


def test_run_events_404_unknown_run(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/runs/(?P<id>[^/]+)/events",
        lambda h, m, q, b: h.send_json(
            404, {"code": 1, "message": "no run with id bogus", "remediation": "check the run id"}
        ),
    )
    fake_api.start()
    rc = main(["run", "events", "bogus", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "no run with id bogus" in captured.err
    assert "hint: check the run id" in captured.err


# --- bare noun + connection errors -----------------------------------------------


def test_run_bare_noun(capsys) -> None:
    rc = main(["run"])
    assert rc == 0
    assert "usage: nodes run" in capsys.readouterr().out


def test_run_connection_refused_exit_2(capsys) -> None:
    rc = main(["run", "list", "--api-url", "http://127.0.0.1:1"])
    captured = capsys.readouterr()
    assert rc == 2
    assert captured.err.startswith("error: cannot reach the nodes API at http://127.0.0.1:1\n")
