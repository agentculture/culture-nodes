"""Tests for `nodes workflow` — validate/publish/list/get against a fake API."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from culture_nodes.cli import main


def _send_compact_json(handler, status: int, payload: object) -> None:
    """Write a response whose bytes are NOT what Python's json.dump default
    separators would produce (no spaces) — proves --json passthrough is a
    true byte-for-byte relay, not a re-encode that happens to look similar.
    """
    body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    handler.send_response(status)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)


@pytest.fixture
def workflow_file(tmp_path: Path) -> Path:
    p = tmp_path / "wf.yaml"
    p.write_text("name: demo\nversion: 1\n", encoding="utf-8")
    return p


# --- validate ----------------------------------------------------------------


def test_workflow_validate_valid_text(fake_api, workflow_file, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/workflows/validate",
        lambda h, m, q, b: h.send_json(
            200, {"valid": True, "digest": "sha256:abc123", "diagnostics": []}
        ),
    )
    fake_api.start()
    rc = main(["workflow", "validate", str(workflow_file), "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "valid: 0 errors, 0 warnings" in out
    assert "digest: sha256:abc123" in out


def test_workflow_validate_sends_yaml_format(fake_api, workflow_file) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(200, {"valid": True, "digest": "d", "diagnostics": []})

    fake_api.route("POST", r"/v1alpha1/workflows/validate", handler)
    fake_api.start()
    main(["workflow", "validate", str(workflow_file), "--api-url", fake_api.base_url])
    assert seen["body"]["format"] == "yaml"
    assert "name: demo" in seen["body"]["source"]


def test_workflow_validate_sends_json_format(fake_api, tmp_path: Path) -> None:
    wf = tmp_path / "wf.json"
    wf.write_text('{"name": "demo"}', encoding="utf-8")
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(200, {"valid": True, "digest": "d", "diagnostics": []})

    fake_api.route("POST", r"/v1alpha1/workflows/validate", handler)
    fake_api.start()
    main(["workflow", "validate", str(wf), "--api-url", fake_api.base_url])
    assert seen["body"]["format"] == "json"


def test_workflow_validate_invalid_is_domain_outcome_not_error(
    fake_api, workflow_file, capsys
) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/workflows/validate",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "valid": False,
                "digest": "",
                "diagnostics": [
                    {
                        "level": "error",
                        "path": "/nodes/0",
                        "code": "E001",
                        "message": "missing owner",
                        "hint": "add an ownerRef",
                    }
                ],
            },
        ),
    )
    fake_api.start()
    rc = main(["workflow", "validate", str(workflow_file), "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert captured.err == ""  # domain outcome: stdout only, never error:/hint: on stderr
    assert "error /nodes/0 E001: missing owner | hint: add an ownerRef" in captured.out
    assert "invalid: 1 error, 0 warnings" in captured.out


def test_workflow_validate_json_passthrough_byte_exact(fake_api, workflow_file, capsys) -> None:
    payload = {"valid": True, "digest": "sha256:xyz", "diagnostics": []}
    fake_api.route(
        "POST",
        r"/v1alpha1/workflows/validate",
        lambda h, m, q, b: _send_compact_json(h, 200, payload),
    )
    fake_api.start()
    rc = main(
        ["workflow", "validate", str(workflow_file), "--json", "--api-url", fake_api.base_url]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert out == json.dumps(payload, separators=(",", ":")) + "\n"


def test_workflow_validate_unreadable_file_exit_2(fake_api, capsys) -> None:
    fake_api.start()
    rc = main(
        [
            "workflow",
            "validate",
            "/no/such/file/here.yaml",
            "--api-url",
            fake_api.base_url,
        ]
    )
    err = capsys.readouterr().err
    assert rc == 2
    assert err.startswith("error:")
    assert "hint:" in err


# --- publish -------------------------------------------------------------------


def test_workflow_publish_new_text(fake_api, workflow_file, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/workflows",
        lambda h, m, q, b: h.send_json(
            201,
            {
                "id": "wv-1",
                "workflow_key": "demo",
                "version": 1,
                "digest": "sha256:new",
                "created_at": "2026-01-01T00:00:00Z",
            },
        ),
    )
    fake_api.start()
    rc = main(["workflow", "publish", str(workflow_file), "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "published: new" in out
    assert "digest: sha256:new" in out


def test_workflow_publish_idempotent_existing(fake_api, workflow_file, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/workflows",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "id": "wv-1",
                "workflow_key": "demo",
                "version": 1,
                "digest": "sha256:same",
                "created_at": "2026-01-01T00:00:00Z",
            },
        ),
    )
    fake_api.start()
    rc = main(["workflow", "publish", str(workflow_file), "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "published: already published" in out


def test_workflow_publish_422_does_not_compile(fake_api, workflow_file, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/workflows",
        lambda h, m, q, b: h.send_json(
            422,
            {
                "code": 1,
                "message": "workflow does not compile: 2 error diagnostic(s)",
                "remediation": (
                    "call POST /v1alpha1/workflows/validate for the full diagnostic list"
                ),
            },
        ),
    )
    fake_api.start()
    rc = main(["workflow", "publish", str(workflow_file), "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert captured.err == (
        "error: workflow does not compile: 2 error diagnostic(s)\n"
        "hint: call POST /v1alpha1/workflows/validate for the full diagnostic list\n"
    )


# --- list ------------------------------------------------------------------------


def test_workflow_list_text(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/workflows",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "items": [
                    {
                        "digest": "sha256:one",
                        "workflow_key": "demo",
                        "version": 1,
                        "created_at": "2026-01-01T00:00:00Z",
                    }
                ]
            },
        ),
    )
    fake_api.start()
    rc = main(["workflow", "list", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "sha256:one" in out
    assert "demo" in out
    assert "v1" in out


def test_workflow_list_empty(fake_api, capsys) -> None:
    fake_api.route(
        "GET", r"/v1alpha1/workflows", lambda h, m, q, b: h.send_json(200, {"items": []})
    )
    fake_api.start()
    rc = main(["workflow", "list", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "no published workflows" in out


def test_workflow_list_passes_query_params(fake_api) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["query"] = q
        h.send_json(200, {"items": []})

    fake_api.route("GET", r"/v1alpha1/workflows", handler)
    fake_api.start()
    main(
        [
            "workflow",
            "list",
            "--workflow-key",
            "demo",
            "--limit",
            "7",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert seen["query"] == {"workflow_key": ["demo"], "limit": ["7"]}


# --- get -------------------------------------------------------------------------


def test_workflow_get_404(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/workflows/(?P<digest>[^/]+)",
        lambda h, m, q, b: h.send_json(
            404,
            {
                "code": 1,
                "message": "no workflow version with digest bogus",
                "remediation": "check the digest, or POST /v1alpha1/workflows to publish it",
            },
        ),
    )
    fake_api.start()
    rc = main(["workflow", "get", "bogus", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert captured.err.startswith("error: no workflow version with digest bogus\n")
    assert "hint: check the digest" in captured.err


def test_workflow_get_json_passthrough_byte_exact(fake_api, capsys) -> None:
    payload = {
        "id": "wv-1",
        "workflow_key": "demo",
        "version": 1,
        "source_format": "yaml",
        "source": "name: demo",
        "normalized_ir": {"nodes": []},
        "digest": "sha256:one",
        "created_at": "2026-01-01T00:00:00Z",
    }
    fake_api.route(
        "GET",
        r"/v1alpha1/workflows/(?P<digest>[^/]+)",
        lambda h, m, q, b: _send_compact_json(h, 200, payload),
    )
    fake_api.start()
    rc = main(["workflow", "get", "sha256:one", "--json", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert out == json.dumps(payload, separators=(",", ":")) + "\n"


# --- bare noun + connection errors -----------------------------------------------


def test_workflow_bare_noun(capsys) -> None:
    rc = main(["workflow"])
    assert rc == 0
    assert "usage: nodes workflow" in capsys.readouterr().out


def test_workflow_connection_refused_exit_2(capsys) -> None:
    rc = main(["workflow", "list", "--api-url", "http://127.0.0.1:1"])
    captured = capsys.readouterr()
    assert rc == 2
    assert captured.err.startswith("error: cannot reach the nodes API at http://127.0.0.1:1\n")
    assert "hint: start it with 'nodes serve'" in captured.err
