"""Tests for `nodes ledger` and `nodes review` against a fake API."""

from __future__ import annotations

import json

from culture_nodes.cli import main


def _write_compact(handler, status: int, payload: object) -> None:
    body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    handler.send_response(status)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)


# --- ledger records ------------------------------------------------------------


def test_ledger_records_text(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/runs/(?P<id>[^/]+)/ledger",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "items": [
                    {
                        "id": "rec-1",
                        "record_type": "claim",
                        "authority": "proposed",
                        "created_at": "2026-01-01T00:00:00Z",
                    }
                ],
                "ledger_version": 3,
            },
        ),
    )
    fake_api.start()
    rc = main(["ledger", "records", "run-1", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "ledger_version: 3" in out
    assert "rec-1" in out
    assert "claim" in out
    assert "proposed" in out


def test_ledger_records_json_passthrough_byte_exact(fake_api, capsys) -> None:
    payload = {"items": [], "ledger_version": 0}
    fake_api.route(
        "GET",
        r"/v1alpha1/runs/(?P<id>[^/]+)/ledger",
        lambda h, m, q, b: _write_compact(h, 200, payload),
    )
    fake_api.start()
    rc = main(["ledger", "records", "run-1", "--json", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert out == json.dumps(payload, separators=(",", ":")) + "\n"


def test_ledger_records_404(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/runs/(?P<id>[^/]+)/ledger",
        lambda h, m, q, b: h.send_json(
            404, {"code": 1, "message": "no run with id bogus", "remediation": "check the run id"}
        ),
    )
    fake_api.start()
    rc = main(["ledger", "records", "bogus", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "no run with id bogus" in captured.err


# --- ledger projection -----------------------------------------------------------


def test_ledger_projection_text_and_subject_query(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["query"] = q
        h.send_json(
            200,
            {
                "kind": "evidence_for_subject",
                "subject": "task-1",
                "items": [{"id": "r1"}],
                "digest": "d1",
            },
        )

    fake_api.route(
        "GET", r"/v1alpha1/runs/(?P<id>[^/]+)/ledger/projections/(?P<name>[^/]+)", handler
    )
    fake_api.start()
    rc = main(
        [
            "ledger",
            "projection",
            "run-1",
            "evidence_for_subject",
            "--subject",
            "task-1",
            "--api-url",
            fake_api.base_url,
        ]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert "kind: evidence_for_subject" in out
    assert "subject: task-1" in out
    assert "digest: d1" in out
    assert "items: 1" in out
    assert seen["query"] == {"subject": ["task-1"]}


def test_ledger_projection_unknown_name_400(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/runs/(?P<id>[^/]+)/ledger/projections/(?P<name>[^/]+)",
        lambda h, m, q, b: h.send_json(
            400,
            {
                "code": 1,
                "message": 'unknown projection "bogus"',
                "remediation": "use one of: current_scope, confirmed_claims",
            },
        ),
    )
    fake_api.start()
    rc = main(["ledger", "projection", "run-1", "bogus", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "unknown projection" in captured.err
    assert "hint: use one of" in captured.err


# --- review create --------------------------------------------------------------


def test_review_create_text(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(
            201,
            {
                "id": "rev-1",
                "run_id": "run-1",
                "status": "requested",
                "ledger_version": 4,
                "frame_checksum": "sha256:frame",
                "record_ids": ["rec-1", "rec-2"],
                "created_at": "t",
            },
        )

    fake_api.route("POST", r"/v1alpha1/runs/(?P<id>[^/]+)/reviews", handler)
    fake_api.start()
    rc = main(
        [
            "review",
            "create",
            "run-1",
            "--records",
            "rec-1,rec-2",
            "--ledger-version",
            "4",
            "--api-url",
            fake_api.base_url,
        ]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert "id: rev-1" in out
    assert "status: requested" in out
    assert "records: 2" in out
    assert seen["body"] == {"record_ids": ["rec-1", "rec-2"], "ledger_version": 4}


def test_review_create_requires_records(fake_api, capsys) -> None:
    fake_api.start()
    rc = main(
        [
            "review",
            "create",
            "run-1",
            "--records",
            "",
            "--ledger-version",
            "1",
            "--api-url",
            fake_api.base_url,
        ]
    )
    captured = capsys.readouterr()
    assert rc == 1
    assert "at least one ledger record id" in captured.err


def test_review_create_409_wrong_ledger_version(fake_api, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/runs/(?P<id>[^/]+)/reviews",
        lambda h, m, q, b: h.send_json(
            409,
            {
                "code": 1,
                "message": "ledger version 4 does not match current version 5",
                "remediation": "re-read the current ledger version and try again",
            },
        ),
    )
    fake_api.start()
    rc = main(
        [
            "review",
            "create",
            "run-1",
            "--records",
            "rec-1",
            "--ledger-version",
            "4",
            "--api-url",
            fake_api.base_url,
        ]
    )
    captured = capsys.readouterr()
    assert rc == 1
    assert "does not match current version" in captured.err


# --- review commit ---------------------------------------------------------------


def test_review_commit_text(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(
            200,
            {
                "review_id": "rev-1",
                "records": [{"id": "rec-1"}, {"id": "rec-2"}],
                "ledger_version": 5,
            },
        )

    fake_api.route("POST", r"/v1alpha1/reviews/(?P<id>[^/]+)/commit", handler)
    fake_api.start()
    rc = main(
        [
            "review",
            "commit",
            "rev-1",
            "--confirm",
            "rec-1",
            "--reject",
            "rec-2",
            "--ledger-version",
            "4",
            "--api-url",
            fake_api.base_url,
        ]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert "review_id: rev-1" in out
    assert "ledger_version: 5" in out
    assert "records: 2" in out
    assert seen["body"] == {
        "decisions": {"rec-1": "confirm", "rec-2": "reject"},
        "expected_ledger_version": 4,
    }


def test_review_commit_requires_confirm_or_reject(fake_api, capsys) -> None:
    fake_api.start()
    rc = main(
        ["review", "commit", "rev-1", "--ledger-version", "4", "--api-url", fake_api.base_url]
    )
    captured = capsys.readouterr()
    assert rc == 1
    assert "at least one of --confirm or --reject" in captured.err


def test_review_commit_rejects_overlapping_ids(fake_api, capsys) -> None:
    fake_api.start()
    rc = main(
        [
            "review",
            "commit",
            "rev-1",
            "--confirm",
            "rec-1",
            "--reject",
            "rec-1",
            "--ledger-version",
            "4",
            "--api-url",
            fake_api.base_url,
        ]
    )
    captured = capsys.readouterr()
    assert rc == 1
    assert "named in both --confirm and --reject" in captured.err


def test_review_commit_409_stale(fake_api, capsys) -> None:
    """The task's explicit acceptance case: a 409 from commit maps to
    CliError code 1 ("stale") carrying the API's own remediation — no
    special-casing needed beyond the generic error-body mapping."""
    fake_api.route(
        "POST",
        r"/v1alpha1/reviews/(?P<id>[^/]+)/commit",
        lambda h, m, q, b: h.send_json(
            409,
            {
                "code": 1,
                "message": "review rev-1 is stale: the ledger has moved",
                "remediation": (
                    "re-read the current ledger version and, if still needed, "
                    "submit a new review request"
                ),
            },
        ),
    )
    fake_api.start()
    rc = main(
        [
            "review",
            "commit",
            "rev-1",
            "--confirm",
            "rec-1",
            "--ledger-version",
            "4",
            "--api-url",
            fake_api.base_url,
        ]
    )
    captured = capsys.readouterr()
    assert rc == 1
    assert captured.err == (
        "error: review rev-1 is stale: the ledger has moved\n"
        "hint: re-read the current ledger version and, if still needed, "
        "submit a new review request\n"
    )


def test_review_commit_json_passthrough_byte_exact(fake_api, capsys) -> None:
    payload = {"review_id": "rev-1", "records": [], "ledger_version": 9}
    fake_api.route(
        "POST",
        r"/v1alpha1/reviews/(?P<id>[^/]+)/commit",
        lambda h, m, q, b: _write_compact(h, 200, payload),
    )
    fake_api.start()
    rc = main(
        [
            "review",
            "commit",
            "rev-1",
            "--confirm",
            "rec-1",
            "--ledger-version",
            "4",
            "--json",
            "--api-url",
            fake_api.base_url,
        ]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert out == json.dumps(payload, separators=(",", ":")) + "\n"


# --- bare nouns ------------------------------------------------------------------


def test_ledger_bare_noun(capsys) -> None:
    rc = main(["ledger"])
    assert rc == 0
    assert "usage: nodes ledger" in capsys.readouterr().out


def test_review_bare_noun(capsys) -> None:
    rc = main(["review"])
    assert rc == 0
    assert "usage: nodes review" in capsys.readouterr().out
