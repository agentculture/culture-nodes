"""Tests for run grading, cancellation, event streaming, and failures.

These commands act on an existing run, unlike test_run.py's creation and
query concerns. Grouping the mutation and live-stream surfaces here keeps
their authority, terminal-state, and transport-error cases together.
"""

from __future__ import annotations

import json

import pytest

from culture_nodes.cli import main
from tests.run_test_helpers import write_compact

# --- grade -----------------------------------------------------------------


def test_run_grade_text(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(
            201,
            {
                "id": "ledger_1",
                "record_type": "grade",
                "authority": "proposed",
                "origin": {"kind": "agent", "actor_id": "actor-grader"},
                "data": {
                    "rating": 4,
                    "rationale": "solid",
                    "evaluated_actor_id": "actor-evaluated",
                },
            },
        )

    fake_api.route("POST", r"/v1alpha1/runs/(?P<id>[^/]+)/grades", handler)
    fake_api.start()
    rc = main(
        [
            "run",
            "grade",
            "run-1",
            "--rating",
            "4",
            "--notes",
            "solid",
            "--actor",
            "actor-evaluated",
            "--as",
            "actor-grader",
            "--api-url",
            fake_api.base_url,
        ]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert seen["body"] == {
        "rating": 4,
        "rationale": "solid",
        "evaluated_actor_id": "actor-evaluated",
        "grading_actor_id": "actor-grader",
    }
    assert "id: ledger_1" in out
    assert "authority: proposed" in out
    assert "origin: agent (actor-grader)" in out
    assert "rating: 4" in out
    assert "evaluated_actor_id: actor-evaluated" in out


def test_run_grade_optional_fields_included_only_when_given(fake_api) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(
            201,
            {
                "id": "ledger_1",
                "authority": "confirmed",
                "origin": {"kind": "human", "actor_id": "actor-human"},
                "data": {
                    "rating": 5,
                    "rationale": "clean",
                    "evaluated_actor_id": "actor-evaluated",
                },
            },
        )

    fake_api.route("POST", r"/v1alpha1/runs/(?P<id>[^/]+)/grades", handler)
    fake_api.start()
    rc = main(
        [
            "run",
            "grade",
            "run-1",
            "--rating",
            "5",
            "--notes",
            "clean",
            "--actor",
            "actor-evaluated",
            "--as",
            "actor-human",
            "--node-run-ref",
            "nr-1",
            "--attempt-ref",
            "att-1",
            "--category",
            "review",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert rc == 0
    assert seen["body"] == {
        "rating": 5,
        "rationale": "clean",
        "evaluated_actor_id": "actor-evaluated",
        "grading_actor_id": "actor-human",
        "node_run_ref": "nr-1",
        "attempt_ref": "att-1",
        "category": "review",
    }


def test_run_grade_json_passthrough_byte_exact(fake_api, capsys) -> None:
    payload = {
        "id": "ledger_1",
        "authority": "proposed",
        "origin": {"kind": "agent", "actor_id": "actor-grader"},
        "data": {"rating": 4, "rationale": "solid", "evaluated_actor_id": "actor-evaluated"},
    }
    fake_api.route(
        "POST",
        r"/v1alpha1/runs/(?P<id>[^/]+)/grades",
        lambda h, m, q, b: write_compact(h, 201, payload),
    )
    fake_api.start()
    rc = main(
        [
            "run",
            "grade",
            "run-1",
            "--rating",
            "4",
            "--notes",
            "solid",
            "--actor",
            "actor-evaluated",
            "--as",
            "actor-grader",
            "--json",
            "--api-url",
            fake_api.base_url,
        ]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert out == json.dumps(payload, separators=(",", ":")) + "\n"


def test_run_grade_rating_out_of_bounds_refused_by_argparse(capsys) -> None:
    with pytest.raises(SystemExit) as exc:
        main(
            [
                "run",
                "grade",
                "run-1",
                "--rating",
                "6",
                "--notes",
                "solid",
                "--actor",
                "actor-evaluated",
                "--as",
                "actor-grader",
            ]
        )
    assert exc.value.code == 1
    err = capsys.readouterr().err
    assert err.startswith("error:")
    assert "hint:" in err


def test_run_grade_400_self_grade_refused(fake_api, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/runs/(?P<id>[^/]+)/grades",
        lambda h, m, q, b: h.send_json(
            400,
            {
                "code": 1,
                "message": "ledger: authority refused [no_self_grade]: origin agent (actor-1) ...",
                "remediation": "the producer named in origin may not write this record's authority",
            },
        ),
    )
    fake_api.start()
    rc = main(
        [
            "run",
            "grade",
            "run-1",
            "--rating",
            "3",
            "--notes",
            "solid",
            "--actor",
            "actor-1",
            "--as",
            "actor-1",
            "--api-url",
            fake_api.base_url,
        ]
    )
    captured = capsys.readouterr()
    assert rc == 1
    assert "no_self_grade" in captured.err


def test_run_grade_404_unknown_actor(fake_api, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/runs/(?P<id>[^/]+)/grades",
        lambda h, m, q, b: h.send_json(
            404,
            {
                "code": 1,
                "message": "no actor with id does-not-exist",
                "remediation": "check the id and try again",
            },
        ),
    )
    fake_api.start()
    rc = main(
        [
            "run",
            "grade",
            "run-1",
            "--rating",
            "3",
            "--notes",
            "solid",
            "--actor",
            "does-not-exist",
            "--as",
            "actor-grader",
            "--api-url",
            fake_api.base_url,
        ]
    )
    captured = capsys.readouterr()
    assert rc == 1
    assert "no actor with id does-not-exist" in captured.err


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
    out = capsys.readouterr().out
    assert "usage: nodes run" in out
    assert "retag" in out
    assert "grade" in out


def test_run_connection_refused_exit_2(capsys) -> None:
    rc = main(["run", "list", "--api-url", "http://127.0.0.1:1"])
    captured = capsys.readouterr()
    assert rc == 2
    assert captured.err.startswith("error: cannot reach the nodes API at http://127.0.0.1:1\n")
