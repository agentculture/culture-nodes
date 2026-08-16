"""Tests for `nodes actors` — the actors listing and the capacity-pause clear.

The breaker itself lives in the Go control plane; what this suite owns is the
operator's view of it (task t9, honesty condition h38): does a pause render
with its reason and until-when, and can it be cleared without touching the
database — including the bearer-token discipline `resume` shares with
`human-tasks decide`.
"""

from __future__ import annotations

import json

from culture_nodes.cli import main
from culture_nodes.cli._commands.actors import ENV_REGISTRATION_TOKEN

PAUSED_ACTOR = {
    "id": "act-1",
    "actor_key": "company/analyzer",
    "revision": 2,
    "kind": "agent",
    "protocol": "http",
    "created_at": "t",
    "availability": {
        "paused": True,
        "paused_until": "2026-08-13T18:00:00Z",
        "reason": "capacity_exhausted",
        "retry_after_seconds": 120,
        "detail": "weekly session limit reached",
        "tripped_at": "2026-08-13T17:58:00Z",
        "tripped_by_run_id": "run-9",
        "tripped_by_attempt_id": "att-9",
    },
}

AVAILABLE_ACTOR = {
    "id": "act-2",
    "actor_key": "company/verifier",
    "revision": 1,
    "kind": "agent",
    "protocol": "http",
    "created_at": "t",
}


def test_actors_list_renders_the_pause(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/actors$",
        lambda h, m, q, b: h.send_json(200, {"items": [PAUSED_ACTOR, AVAILABLE_ACTOR]}),
    )
    fake_api.start()
    rc = main(["actors", "list", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "PAUSED until 2026-08-13T18:00:00Z (capacity_exhausted)" in out
    assert "company/verifier" in out
    assert "available" in out


def test_actors_list_paused_only_filters(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/actors$",
        lambda h, m, q, b: h.send_json(200, {"items": [PAUSED_ACTOR, AVAILABLE_ACTOR]}),
    )
    fake_api.start()
    rc = main(["actors", "list", "--paused-only", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "company/analyzer" in out
    assert "company/verifier" not in out


def test_actors_list_no_actors(fake_api, capsys) -> None:
    fake_api.route("GET", r"/v1alpha1/actors$", lambda h, m, q, b: h.send_json(200, {"items": []}))
    fake_api.start()
    rc = main(["actors", "list", "--api-url", fake_api.base_url])
    assert rc == 0
    assert "no actors" in capsys.readouterr().out


def test_actors_get_renders_reason_and_until_when(fake_api, capsys) -> None:
    fake_api.route(
        "GET", r"/v1alpha1/actors/act-1$", lambda h, m, q, b: h.send_json(200, PAUSED_ACTOR)
    )
    fake_api.start()
    rc = main(["actors", "get", "act-1", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "availability.paused: yes" in out
    assert "availability.reason: capacity_exhausted" in out
    assert "availability.paused_until: 2026-08-13T18:00:00Z" in out
    assert "availability.retry_after_seconds: 120" in out
    assert "availability.tripped_by_attempt_id: att-9" in out


PACED_ACTOR = {
    "id": "act-3",
    "actor_key": "company/paced",
    "revision": 1,
    "kind": "agent",
    "protocol": "http",
    "created_at": "t",
    "dispatch_rate": {
        "scope": "actor",
        "scope_key": "company/paced",
        "limit_per_window": 4,
        "window_seconds": 18000,
        "window_anchor": "2026-08-13T00:00:00Z",
        "window_started_at": "2026-08-13T15:00:00Z",
        "window_ends_at": "2026-08-13T20:00:00Z",
        "dispatched": 3,
        "remaining": 1,
        "next_dispatch_at": "2026-08-13T18:30:00Z",
        "updated_at": "2026-08-13T17:00:00Z",
    },
}


def test_actors_get_renders_the_declared_rate_and_its_consumption(fake_api, capsys) -> None:
    """Task t10: the rate being enforced and how much of the window is left."""
    fake_api.route(
        "GET", r"/v1alpha1/actors/act-3$", lambda h, m, q, b: h.send_json(200, PACED_ACTOR)
    )
    fake_api.start()
    rc = main(["actors", "get", "act-3", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "dispatch_rate.limit_per_window: 4" in out
    assert "dispatch_rate.dispatched: 3" in out
    assert "dispatch_rate.remaining: 1" in out
    assert "dispatch_rate.window_ends_at: 2026-08-13T20:00:00Z" in out
    assert "dispatch_rate.next_dispatch_at: 2026-08-13T18:30:00Z" in out


def test_actors_get_without_a_rate_renders_no_dispatch_rate_block(fake_api, capsys) -> None:
    """No declared rate is not a rate with nothing consumed, and must not print like one."""
    fake_api.route(
        "GET", r"/v1alpha1/actors/act-2$", lambda h, m, q, b: h.send_json(200, AVAILABLE_ACTOR)
    )
    fake_api.start()
    rc = main(["actors", "get", "act-2", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "dispatch_rate" not in out


def test_actors_get_without_a_pause_renders_no_availability_block(fake_api, capsys) -> None:
    """Never paused is not the same fact as `paused: no`, and must not print like it."""
    fake_api.route(
        "GET", r"/v1alpha1/actors/act-2$", lambda h, m, q, b: h.send_json(200, AVAILABLE_ACTOR)
    )
    fake_api.start()
    rc = main(["actors", "get", "act-2", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "availability" not in out


def test_actors_resume_sends_the_token_and_renders_the_cleared_pause(
    fake_api, capsys, monkeypatch
) -> None:
    seen: dict = {}

    def handler(h, m, q, b):
        seen["auth"] = h.headers.get("Authorization")
        seen["body"] = json.loads(b) if b else None
        cleared = dict(PAUSED_ACTOR)
        cleared["availability"] = dict(PAUSED_ACTOR["availability"])
        cleared["availability"].update(
            {"paused": False, "cleared_at": "2026-08-13T17:59:00Z", "cleared_by": "ori"}
        )
        h.send_json(200, cleared)

    fake_api.route("POST", r"/v1alpha1/actors/act-1/resume$", handler)
    fake_api.start()
    monkeypatch.delenv(ENV_REGISTRATION_TOKEN, raising=False)
    rc = main(
        [
            "actors",
            "resume",
            "act-1",
            "--cleared-by",
            "ori",
            "--token",
            "secret-token",
            "--api-url",
            fake_api.base_url,
        ]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert seen["auth"] == "Bearer secret-token"
    assert seen["body"] == {"cleared_by": "ori"}
    assert "availability.paused: no" in out
    assert "availability.cleared_by: ori" in out
    # The token is never echoed anywhere in the rendered result.
    assert "secret-token" not in out


def test_actors_resume_reads_the_token_from_the_environment(fake_api, capsys, monkeypatch) -> None:
    seen: dict = {}

    def handler(h, m, q, b):
        seen["auth"] = h.headers.get("Authorization")
        h.send_json(200, AVAILABLE_ACTOR)

    fake_api.route("POST", r"/v1alpha1/actors/act-2/resume$", handler)
    fake_api.start()
    monkeypatch.setenv(ENV_REGISTRATION_TOKEN, "env-token")
    rc = main(["actors", "resume", "act-2", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert seen["auth"] == "Bearer env-token"
    # An actor that was not paused says so rather than printing an empty block.
    assert "not paused" in out


def test_actors_resume_without_a_token_is_a_structured_error(capsys, monkeypatch) -> None:
    monkeypatch.delenv(ENV_REGISTRATION_TOKEN, raising=False)
    rc = main(["actors", "resume", "act-1"])
    captured = capsys.readouterr()
    assert rc == 1
    assert "error:" in captured.err
    assert "hint:" in captured.err
    assert ENV_REGISTRATION_TOKEN in captured.err


def test_actors_json_is_byte_exact_passthrough(fake_api, capsys) -> None:
    fake_api.route(
        "GET", r"/v1alpha1/actors/act-1$", lambda h, m, q, b: h.send_json(200, PAUSED_ACTOR)
    )
    fake_api.start()
    rc = main(["actors", "get", "act-1", "--json", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert json.loads(out)["availability"]["reason"] == "capacity_exhausted"


def test_actors_bare_noun_points_at_explain(capsys) -> None:
    rc = main(["actors"])
    out = capsys.readouterr().out
    assert rc == 0
    assert "nodes explain actors" in out


# --- nodes actors dial-in (task t6, issues #136 / #121) ---------------------
#
# Retiring the stored participant address does not retire the question the
# address answered. This is the operator's read-only view of it: which
# bridges are dialled in right now, answered without dispatching anything.

DIAL_IN_PRESENCE = {
    "observed_at": "2026-08-16T12:00:00Z",
    "window_seconds": 30,
    "connected": 1,
    "disconnected": 1,
    "never_dialled": 2,
    "items": [
        {
            "actor_key": "company/developer",
            "actor_id": "act-dev",
            "revision": 3,
            "kind": "agent",
            "presence": "connected",
            "last_seen_at": "2026-08-16T11:59:57Z",
            "seconds_since_last_seen": 3.0,
            "credential": {
                "issued": True,
                "issued_at": "2026-08-15T09:00:00Z",
                "issuance_count": 1,
                "revoked": False,
                "locked_out": False,
                "failure_count": 0,
            },
        },
        {
            "actor_key": "company/notify",
            "actor_id": "act-notify",
            "revision": 1,
            "kind": "agent",
            "presence": "disconnected",
            "last_seen_at": "2026-08-16T10:00:00Z",
            "seconds_since_last_seen": 7200.0,
            "credential": {
                "issued": True,
                "issuance_count": 1,
                "revoked": False,
                "locked_out": False,
                "failure_count": 0,
            },
        },
        {
            "actor_key": "company/codex-thor",
            "actor_id": "act-thor",
            "revision": 2,
            "kind": "agent",
            "presence": "never_dialled",
        },
        {
            "actor_key": "company/revoked-bridge",
            "actor_id": "act-revoked",
            "revision": 1,
            "kind": "agent",
            "presence": "never_dialled",
            "credential": {
                "issued": True,
                "issuance_count": 2,
                "revoked": True,
                "revoked_at": "2026-08-16T09:00:00Z",
                "locked_out": False,
                "failure_count": 0,
            },
        },
    ],
}


def _serve_presence(fake_api, payload=None):
    fake_api.route(
        "GET",
        r"/v1alpha1/dial-in-presence$",
        lambda h, m, q, b: h.send_json(200, payload or DIAL_IN_PRESENCE),
    )
    fake_api.start()


def test_dial_in_lists_presence_without_dispatching(fake_api, capsys) -> None:
    """Criterion 1: one GET answers 'who is connected', and it is the only call."""
    _serve_presence(fake_api)
    rc = main(["actors", "dial-in", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    # Exactly one request, and it is a read.
    assert [(method, path) for method, path, _ in fake_api.requests] == [
        ("GET", "/v1alpha1/dial-in-presence")
    ]
    assert "company/developer" in out
    assert "connected" in out
    # The window the claim is made against is stated, not implied.
    assert "30" in out


def test_dial_in_distinguishes_never_dialled_from_dropped(fake_api, capsys) -> None:
    """Criterion 2: absence has two shapes, and the dropped one names when."""
    _serve_presence(fake_api)
    rc = main(["actors", "dial-in", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    lines = {line.split()[0]: line for line in out.splitlines() if line.startswith("company/")}

    dropped = lines["company/notify"]
    assert "DISCONNECTED" in dropped
    assert "2026-08-16T10:00:00Z" in dropped

    never = lines["company/codex-thor"]
    assert "NEVER DIALLED" in never
    # A bridge that never dialled in has no last-seen instant to report, and
    # must not borrow one that reads like "just now".
    assert "2026-08-16T10:00:00Z" not in never
    assert "ago" not in never


def test_dial_in_names_revocation_as_its_own_reason(fake_api, capsys) -> None:
    """A revoked credential looks absent for a reason an outage cannot fix."""
    _serve_presence(fake_api)
    rc = main(["actors", "dial-in", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    revoked = next(line for line in out.splitlines() if "company/revoked-bridge" in line)
    assert "REVOKED" in revoked


def test_dial_in_absent_only_filters_to_the_problem(fake_api, capsys) -> None:
    _serve_presence(fake_api)
    rc = main(["actors", "dial-in", "--absent-only", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "company/developer" not in out
    assert "company/notify" in out
    assert "company/codex-thor" in out


def test_dial_in_json_is_passthrough(fake_api, capsys) -> None:
    _serve_presence(fake_api)
    rc = main(["actors", "dial-in", "--json", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    payload = json.loads(out)
    assert payload["connected"] == 1
    assert payload["items"][2]["presence"] == "never_dialled"


def test_dial_in_reports_an_empty_fleet_honestly(fake_api, capsys) -> None:
    _serve_presence(
        fake_api,
        {
            "observed_at": "2026-08-16T12:00:00Z",
            "window_seconds": 30,
            "connected": 0,
            "disconnected": 0,
            "never_dialled": 0,
            "items": [],
        },
    )
    rc = main(["actors", "dial-in", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "no registered actors" in out
