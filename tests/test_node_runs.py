"""Tests for `nodes node-runs` — the cross-run node-runs listing against a fake API."""

from __future__ import annotations

import json

from culture_nodes.cli import main


def test_node_runs_list_text_and_query(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["query"] = q
        h.send_json(
            200,
            {
                "items": [
                    {
                        "id": "nr-1",
                        "run_id": "run-1",
                        "node_id": "intake",
                        "state": "completed",
                        "updated_at": "t",
                    }
                ],
                "next_cursor": "cur-2",
            },
        )

    fake_api.route("GET", r"/v1alpha1/node-runs", handler)
    fake_api.start()
    rc = main(
        [
            "node-runs",
            "list",
            "--cursor",
            "cur-1",
            "--limit",
            "10",
            "--updated-since",
            "2026-01-01T00:00:00Z",
            "--updated-until",
            "2026-01-02T00:00:00Z",
            "--api-url",
            fake_api.base_url,
        ]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert "nr-1" in out
    assert "next_cursor: cur-2" in out
    assert seen["query"] == {
        "cursor": ["cur-1"],
        "limit": ["10"],
        "updated_since": ["2026-01-01T00:00:00Z"],
        "updated_until": ["2026-01-02T00:00:00Z"],
    }


def test_node_runs_list_no_items(fake_api, capsys) -> None:
    fake_api.route(
        "GET", r"/v1alpha1/node-runs", lambda h, m, q, b: h.send_json(200, {"items": []})
    )
    fake_api.start()
    rc = main(["node-runs", "list", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "no node runs" in out


def test_node_runs_list_no_next_cursor_omitted(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/node-runs",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "items": [
                    {
                        "id": "nr-1",
                        "run_id": "run-1",
                        "node_id": "intake",
                        "state": "completed",
                        "updated_at": "t",
                    }
                ]
            },
        ),
    )
    fake_api.start()
    rc = main(["node-runs", "list", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "next_cursor" not in out


def test_node_runs_list_json_passthrough_byte_exact(fake_api, capsys) -> None:
    payload = {"items": []}
    fake_api.route(
        "GET", r"/v1alpha1/node-runs", lambda h, m, q, b: _write_compact(h, 200, payload)
    )
    fake_api.start()
    rc = main(["node-runs", "list", "--json", "--api-url", fake_api.base_url])
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


def test_node_runs_400_bad_cursor(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/node-runs",
        lambda h, m, q, b: h.send_json(
            400,
            {
                "code": 1,
                "message": "cursor is malformed",
                "remediation": "pass an opaque cursor from a previous response's next_cursor",
            },
        ),
    )
    fake_api.start()
    rc = main(["node-runs", "list", "--cursor", "bogus", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "cursor is malformed" in captured.err


def test_node_runs_bare_noun(capsys) -> None:
    rc = main(["node-runs"])
    assert rc == 0
    assert "usage: nodes node-runs" in capsys.readouterr().out
