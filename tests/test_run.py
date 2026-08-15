"""Tests for `nodes run` — create/list/get/cancel/events against a fake API."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from culture_nodes.cli import main
from tests.run_test_helpers import write_compact

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
        lambda h, m, q, b: write_compact(h, 201, payload),
    )
    fake_api.start()
    rc = main(
        ["run", "create", "--workflow", "sha256:abc", "--json", "--api-url", fake_api.base_url]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert out == json.dumps(payload, separators=(",", ":")) + "\n"


def test_run_create_sends_name_description_category(fake_api) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(
            201,
            {
                "id": "run-1",
                "workflow_digest": "sha256:abc",
                "state": "running",
                "created_at": "t",
                "name": "nightly audit",
                "category": "audit",
            },
        )

    fake_api.route("POST", r"/v1alpha1/runs", handler)
    fake_api.start()
    rc = main(
        [
            "run",
            "create",
            "--workflow",
            "sha256:abc",
            "--name",
            "nightly audit",
            "--description",
            "runs every night",
            "--category",
            "audit",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert rc == 0
    assert seen["body"] == {
        "workflow_digest": "sha256:abc",
        "name": "nightly audit",
        "description": "runs every night",
        "category": "audit",
    }


def test_run_create_text_renders_name_and_category(fake_api, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/runs",
        lambda h, m, q, b: h.send_json(
            201,
            {
                "id": "run-1",
                "workflow_digest": "sha256:abc",
                "state": "running",
                "created_at": "t",
                "name": "nightly audit",
                "category": "audit",
            },
        ),
    )
    fake_api.start()
    rc = main(["run", "create", "--workflow", "sha256:abc", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "name: nightly audit" in out
    assert "category: audit" in out


def test_run_create_text_renders_display_hint_as_derived(fake_api, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/runs",
        lambda h, m, q, b: h.send_json(
            201,
            {
                "id": "run-1",
                "workflow_digest": "sha256:abc",
                "state": "running",
                "created_at": "t",
                "display_hint": "review the pending PR",
            },
        ),
    )
    fake_api.start()
    rc = main(["run", "create", "--workflow", "sha256:abc", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "name: review the pending PR (derived)" in out


def test_run_create_text_renders_usage_when_present(fake_api, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/runs",
        lambda h, m, q, b: h.send_json(
            201,
            {
                "id": "run-1",
                "workflow_digest": "sha256:abc",
                "state": "running",
                "created_at": "t",
                "usage": {
                    "input_tokens": 0,
                    "output_tokens": 0,
                    "attempts_reported": 0,
                    "attempts_not_reported": 0,
                },
            },
        ),
    )
    fake_api.start()
    rc = main(["run", "create", "--workflow", "sha256:abc", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "usage.input_tokens: 0" in out
    assert "usage.attempts_reported: 0" in out
    assert "usage.attempts_not_reported: 0" in out
    # no attempt reported anything, so cost must never be fabricated
    assert "usage.cost" not in out


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


def test_run_list_time_window_and_sort_query(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["query"] = q
        h.send_json(200, {"items": []})

    fake_api.route("GET", r"/v1alpha1/runs", handler)
    fake_api.start()
    rc = main(
        [
            "run",
            "list",
            "--updated-since",
            "2026-01-01T00:00:00Z",
            "--updated-until",
            "2026-01-02T00:00:00Z",
            "--sort",
            "updated_at",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert rc == 0
    assert seen["query"] == {
        "updated_since": ["2026-01-01T00:00:00Z"],
        "updated_until": ["2026-01-02T00:00:00Z"],
        "sort": ["updated_at"],
    }


def test_run_list_renders_name_or_derived_hint(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/runs",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "items": [
                    {
                        "id": "run-1",
                        "state": "completed",
                        "workflow_digest": "sha256:abc",
                        "created_at": "t",
                        "name": "nightly audit",
                    },
                    {
                        "id": "run-2",
                        "state": "running",
                        "workflow_digest": "sha256:abc",
                        "created_at": "t",
                        "display_hint": "review the pending PR",
                    },
                    {
                        "id": "run-3",
                        "state": "running",
                        "workflow_digest": "sha256:abc",
                        "created_at": "t",
                    },
                ]
            },
        ),
    )
    fake_api.start()
    rc = main(["run", "list", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    lines = out.strip("\n").split("\n")
    assert "nightly audit" in lines[0]
    assert "review the pending PR (derived)" in lines[1]
    # run-3 has neither name nor display_hint: no trailing name segment at all.
    assert lines[2].rstrip().endswith("t")


def test_run_list_sort_rejects_unknown_value(capsys) -> None:
    with pytest.raises(SystemExit) as exc:
        main(["run", "list", "--sort", "bogus"])
    assert exc.value.code == 1
    err = capsys.readouterr().err
    assert err.startswith("error:")
    assert "hint:" in err


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
    # no usage/name/category keys in the fake response: none of those lines
    # get fabricated.
    assert "usage." not in out
    assert "name:" not in out
    assert "category:" not in out


def test_run_get_renders_name_category_and_full_usage(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/runs/(?P<id>[^/]+)",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "run": {
                    "id": "run-1",
                    "state": "completed",
                    "workflow_digest": "sha256:abc",
                    "name": "nightly audit",
                    "category": "audit",
                    "usage": {
                        "input_tokens": 1200,
                        "output_tokens": 340,
                        "cost": 0.42,
                        "currency": "USD",
                        "attempts_reported": 3,
                        "attempts_not_reported": 1,
                    },
                },
                "tokens": [],
                "node_runs": [],
            },
        ),
    )
    fake_api.start()
    rc = main(["run", "get", "run-1", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "name: nightly audit" in out
    assert "category: audit" in out
    assert "usage.input_tokens: 1200" in out
    assert "usage.output_tokens: 340" in out
    assert "usage.cost: 0.42 USD" in out
    assert "usage.attempts_reported: 3" in out
    assert "usage.attempts_not_reported: 1" in out


def test_run_get_renders_cost_by_currency_never_summed(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/runs/(?P<id>[^/]+)",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "run": {
                    "id": "run-1",
                    "state": "completed",
                    "workflow_digest": "sha256:abc",
                    "usage": {
                        "input_tokens": 100,
                        "output_tokens": 50,
                        "cost_by_currency": [
                            {"currency": "USD", "cost": 0.1},
                            {"currency": "EUR", "cost": 0.2},
                        ],
                        "attempts_reported": 2,
                        "attempts_not_reported": 0,
                    },
                },
                "tokens": [],
                "node_runs": [],
            },
        ),
    )
    fake_api.start()
    rc = main(["run", "get", "run-1", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "usage.cost_by_currency: 0.1 USD" in out
    assert "usage.cost_by_currency: 0.2 EUR" in out
    # no single summed cost across currencies
    assert "usage.cost:" not in out


def test_run_get_usage_absent_reports_no_attempt_never_zero_fabricated(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/runs/(?P<id>[^/]+)",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "run": {
                    "id": "run-1",
                    "state": "running",
                    "workflow_digest": "sha256:abc",
                    "usage": {
                        "input_tokens": 0,
                        "output_tokens": 0,
                        "attempts_reported": 0,
                        "attempts_not_reported": 2,
                    },
                },
                "tokens": [],
                "node_runs": [],
            },
        ),
    )
    fake_api.start()
    rc = main(["run", "get", "run-1", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "usage.attempts_reported: 0" in out
    assert "usage.attempts_not_reported: 2" in out
    assert "usage.cost" not in out


# --- retag ---------------------------------------------------------------


def test_run_retag_text(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(200, {"id": "run-1", "state": "running", "category": "review"})

    fake_api.route("PATCH", r"/v1alpha1/runs/(?P<id>[^/]+)", handler)
    fake_api.start()
    rc = main(["run", "retag", "run-1", "--category", "review", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert seen["body"] == {"category": "review"}
    assert "id: run-1" in out
    assert "category: review" in out


def test_run_retag_json_passthrough_byte_exact(fake_api, capsys) -> None:
    payload = {"id": "run-1", "state": "running", "category": "review"}
    fake_api.route(
        "PATCH", r"/v1alpha1/runs/(?P<id>[^/]+)", lambda h, m, q, b: write_compact(h, 200, payload)
    )
    fake_api.start()
    rc = main(
        [
            "run",
            "retag",
            "run-1",
            "--category",
            "review",
            "--json",
            "--api-url",
            fake_api.base_url,
        ]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert out == json.dumps(payload, separators=(",", ":")) + "\n"


def test_run_retag_empty_string_clears_category(fake_api) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(200, {"id": "run-1", "state": "running"})

    fake_api.route("PATCH", r"/v1alpha1/runs/(?P<id>[^/]+)", handler)
    fake_api.start()
    rc = main(["run", "retag", "run-1", "--category", "", "--api-url", fake_api.base_url])
    assert rc == 0
    assert seen["body"] == {"category": ""}


def test_run_retag_400_immutable_field_refused(fake_api, capsys) -> None:
    fake_api.route(
        "PATCH",
        r"/v1alpha1/runs/(?P<id>[^/]+)",
        lambda h, m, q, b: h.send_json(
            400,
            {
                "code": 1,
                "message": "PATCH only accepts category",
                "remediation": "name/description are immutable after creation",
            },
        ),
    )
    fake_api.start()
    rc = main(["run", "retag", "run-1", "--category", "review", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "PATCH only accepts category" in captured.err
    assert "hint: name/description are immutable after creation" in captured.err


def test_run_retag_404_unknown_run(fake_api, capsys) -> None:
    fake_api.route(
        "PATCH",
        r"/v1alpha1/runs/(?P<id>[^/]+)",
        lambda h, m, q, b: h.send_json(
            404, {"code": 1, "message": "no run with id bogus", "remediation": "check the run id"}
        ),
    )
    fake_api.start()
    rc = main(["run", "retag", "bogus", "--category", "review", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "no run with id bogus" in captured.err
