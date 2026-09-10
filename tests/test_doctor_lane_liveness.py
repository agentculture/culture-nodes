"""Tests for `nodes doctor`'s fifth check, ``lane_liveness`` (issue #308,
plan loop-closure-claude-codex task t11, spec claims c4/c12/c26, h13).

Both codex lanes answered ``/healthz`` 200 with a spent refresh token on
2026-09-07, and ``codex login status`` printed "Logged in using ChatGPT"
minutes before every dispatch failed with "refresh token was revoked". The
bridge's ``liveness`` fact (t9) is the measurement that tells those apart;
this check is how an operator reads it BEFORE a fan-out, off the actors
listing the control plane exposes (t10 attaches each actor row's
``liveness`` — ``{session_ok, reason, mode, checked_at, locked}``).

The check is warning severity: it can never flip ``healthy`` or the exit
code (only ``prompt_file_present`` does). Four states are asserted here — a
measured-dead lane, a lane whose fact is present but ``session_ok=null``, an
unreachable API, and an API that carries no liveness field yet — because the
honest non-answer (``unmeasured``) must be distinguishable from a verdict
(c26: a stale or unmeasured lane is never refused, only a measured-dead one)
*and* from a live lane (code-review finding 8: the all-clear line counted a
null fact as measured and then claimed ``session_ok=true`` over it).
"""

from __future__ import annotations

import json

from culture_nodes.cli import main

_LIVE = {
    "session_ok": True,
    "reason": "ok",
    "mode": "CHECK",
    "checked_at": "2026-09-07T10:00:00+00:00",
    "locked": False,
}
_DEAD = {
    "session_ok": False,
    "reason": "refresh_token_spent",
    "mode": "LOCK",
    "checked_at": "2026-09-07T10:05:00+00:00",
    "locked": True,
}
#: Code-review finding 8: a lane whose probe could not classify what it saw.
#: `session_ok` is null, so it is neither live nor dead — and doctor used to
#: skip it in the tally and print "all live" over it. A never-logged-in codex
#: lands here (its 401 is now classified, but a probe that times out or dies
#: still does).
_UNMEASURED = {
    "session_ok": None,
    "reason": "probe_failed",
    "mode": "CHECK",
    "checked_at": "2026-09-07T10:07:00+00:00",
    "locked": False,
}


def _find_check(payload: dict, check_id: str) -> dict:
    for check in payload["checks"]:
        if check["id"] == check_id:
            return check
    raise AssertionError(f"no check named {check_id!r} in {payload['checks']!r}")


def _serve(fake_api, items: list[dict]) -> None:
    fake_api.route(
        "GET", r"/v1alpha1/healthz", lambda h, m, q, b: h.send_json(200, {"status": "ok"})
    )
    fake_api.route(
        "GET", r"/v1alpha1/actors", lambda h, m, q, b: h.send_json(200, {"items": items})
    )
    fake_api.start()


def _doctor(capsys, api_url: str) -> tuple[int, dict]:
    rc = main(["doctor", "--json", "--api-url", api_url])
    return rc, json.loads(capsys.readouterr().out)


def test_doctor_reports_exactly_five_checks(fake_api, capsys) -> None:
    """CLAUDE.md's "Mesh identity" section and the teken rubric both name
    the count; this pins it in code rather than prose."""
    _serve(fake_api, [])
    _, payload = _doctor(capsys, fake_api.base_url)
    ids = [c["id"] for c in payload["checks"]]
    assert len(ids) == 5, ids
    assert ids[-1] == "lane_liveness"


def test_measured_dead_lane_is_named_with_its_reason_but_doctor_stays_healthy(
    fake_api, capsys
) -> None:
    _serve(
        fake_api,
        [
            {"actor_key": "company/codex-thor", "revision": 1, "kind": "agent", "liveness": _LIVE},
            {"actor_key": "company/codex-orin", "revision": 2, "kind": "agent", "liveness": _DEAD},
        ],
    )
    rc, payload = _doctor(capsys, fake_api.base_url)
    check = _find_check(payload, "lane_liveness")
    assert rc == 0
    assert payload["healthy"] is True
    assert check["severity"] == "warning"
    assert check["passed"] is False
    assert "company/codex-orin" in check["message"]
    assert "refresh_token_spent" in check["message"]
    assert "LOCK" in check["message"]
    # The live lane is not accused.
    assert "company/codex-thor" not in check["message"]
    # The remediation says what restores a lane — an interactive re-login on
    # the bridge host — and what routes around it meanwhile.
    assert "fallback_actor" in check["remediation"]
    assert "login" in check["remediation"]


def test_locked_lane_counts_even_when_session_ok_is_unmeasured(fake_api, capsys) -> None:
    """LOCK mode latches on a run's output; the latch is the fact, whatever
    the probe last said."""
    latched = {
        "session_ok": None,
        "reason": "unmeasured",
        "mode": "LOCK",
        "checked_at": "2026-09-07T10:05:00+00:00",
        "locked": True,
    }
    _serve(fake_api, [{"actor_key": "company/codex-thor", "revision": 3, "liveness": latched}])
    _, payload = _doctor(capsys, fake_api.base_url)
    check = _find_check(payload, "lane_liveness")
    assert check["passed"] is False
    assert "company/codex-thor" in check["message"]
    assert "locked" in check["message"]


def test_only_the_newest_revision_of_an_actor_is_read(fake_api, capsys) -> None:
    """The actors listing carries every append-only revision; a dead fact on
    a superseded row must not condemn the lane its current row says is live."""
    _serve(
        fake_api,
        [
            {"actor_key": "company/codex-thor", "revision": 1, "liveness": _DEAD},
            {"actor_key": "company/codex-thor", "revision": 2, "liveness": _LIVE},
        ],
    )
    _, payload = _doctor(capsys, fake_api.base_url)
    check = _find_check(payload, "lane_liveness")
    assert check["passed"] is True
    assert "1 lane" in check["message"]
    assert "company/codex-thor" not in check["message"]


def test_no_liveness_field_is_reported_as_unmeasured_and_passes(fake_api, capsys) -> None:
    """Until t10 lands, the API exposes actor rows with no liveness at all.
    That is not a verdict on any lane, so the check passes and says
    ``unmeasured`` — never silently green, never red."""
    _serve(
        fake_api,
        [
            {"actor_key": "company/codex-thor", "revision": 1, "kind": "agent"},
            {"actor_key": "company/human-ops", "revision": 1, "kind": "human"},
        ],
    )
    rc, payload = _doctor(capsys, fake_api.base_url)
    check = _find_check(payload, "lane_liveness")
    assert rc == 0
    assert check["passed"] is True
    assert check["severity"] == "warning"
    assert "unmeasured" in check["message"]
    assert "no liveness" in check["message"]


def test_unreachable_api_is_unmeasured_never_an_error(capsys) -> None:
    # Port 1 is unbound; connecting there refuses immediately.
    rc, payload = _doctor(capsys, "http://127.0.0.1:1")
    check = _find_check(payload, "lane_liveness")
    assert rc == 0
    assert payload["healthy"] is True
    assert check["severity"] == "warning"
    assert check["passed"] is False
    assert "unmeasured" in check["message"]
    assert "not reachable" in check["message"] or "cannot reach" in check["message"]
    assert "nodes_api_reachable" in check["remediation"]


def test_text_mode_names_the_dead_lane_without_failing_overall(fake_api, capsys) -> None:
    _serve(fake_api, [{"actor_key": "company/codex-orin", "revision": 1, "liveness": _DEAD}])
    rc = main(["doctor", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "culture-nodes doctor: healthy" in out
    assert "[FAIL] lane_liveness" in out
    assert "company/codex-orin" in out


def test_malformed_actors_payload_is_unmeasured(fake_api, capsys) -> None:
    """A control plane answering something that is not the actors listing is
    reported as a non-answer, not as five healthy lanes and not as a crash."""
    fake_api.route(
        "GET", r"/v1alpha1/healthz", lambda h, m, q, b: h.send_json(200, {"status": "ok"})
    )
    fake_api.route("GET", r"/v1alpha1/actors", lambda h, m, q, b: h.send_json(200, ["nope"]))
    fake_api.start()
    rc, payload = _doctor(capsys, fake_api.base_url)
    check = _find_check(payload, "lane_liveness")
    assert rc == 0
    assert check["passed"] is False
    assert "unmeasured" in check["message"]


def test_a_null_fact_is_counted_unmeasured_and_never_read_as_all_live(fake_api, capsys) -> None:
    """Code-review finding 8: `session_ok=null` is not `true`. Doctor tallied
    only measured-vs-dead, so one lane whose probe returned no verdict was
    reported as "all live (session_ok=true, none locked)" — the exact claim
    the fact exists to avoid making."""
    _serve(
        fake_api,
        [
            {"actor_key": "company/codex-thor", "revision": 1, "liveness": _LIVE},
            {"actor_key": "company/codex-orin", "revision": 1, "liveness": _UNMEASURED},
        ],
    )
    rc, payload = _doctor(capsys, fake_api.base_url)
    check = _find_check(payload, "lane_liveness")
    assert rc == 0
    assert payload["healthy"] is True
    assert check["severity"] == "warning"
    assert "all live" not in check["message"]
    assert "session_ok=true" not in check["message"]
    # The tally separates the three states, and the unmeasured lane is named
    # with the reason its probe gave.
    assert "1 live" in check["message"]
    assert "1 unmeasured" in check["message"]
    assert "0 dead" in check["message"]
    assert "company/codex-orin" in check["message"]
    assert "probe_failed" in check["message"]
    # The live lane is not accused of being unmeasured.
    assert "company/codex-thor" not in check["message"]


def test_an_unmeasured_lane_alongside_a_dead_one_is_named_in_both_tallies(fake_api, capsys) -> None:
    _serve(
        fake_api,
        [
            {"actor_key": "company/codex-thor", "revision": 1, "liveness": _DEAD},
            {"actor_key": "company/codex-orin", "revision": 1, "liveness": _UNMEASURED},
        ],
    )
    rc, payload = _doctor(capsys, fake_api.base_url)
    check = _find_check(payload, "lane_liveness")
    assert rc == 0
    assert check["passed"] is False
    assert "0 live" in check["message"]
    assert "1 unmeasured" in check["message"]
    assert "1 dead" in check["message"]
    assert "company/codex-thor" in check["message"]
    assert "company/codex-orin" in check["message"]


def test_text_mode_surfaces_an_unmeasured_lane_rather_than_calling_it_live(
    fake_api, capsys
) -> None:
    """The tally has to be readable without `--json`: an operator reading the
    text report before a fan-out must see that a lane was not measured."""
    _serve(
        fake_api,
        [{"actor_key": "company/codex-orin", "revision": 1, "liveness": _UNMEASURED}],
    )
    rc = main(["doctor", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "culture-nodes doctor: healthy" in out
    assert "all live" not in out
    assert "1 unmeasured" in out
    assert "company/codex-orin" in out
    assert "probe_failed" in out


def test_a_not_logged_in_lane_is_dead_and_named_with_that_reason(fake_api, capsys) -> None:
    """The bridge-side classifier (finding 8, part a) turns a never-logged-in
    codex into `session_ok=false reason=not_logged_in`; doctor must carry that
    word through to the operator, since the remediation is `codex login` on
    the bridge host rather than a re-copied credential."""
    never = {
        "session_ok": False,
        "reason": "not_logged_in",
        "mode": "CHECK",
        "checked_at": "2026-09-07T10:08:00+00:00",
        "locked": False,
    }
    _serve(fake_api, [{"actor_key": "company/codex-orin", "revision": 1, "liveness": never}])
    rc, payload = _doctor(capsys, fake_api.base_url)
    check = _find_check(payload, "lane_liveness")
    assert rc == 0
    assert check["passed"] is False
    assert "1 dead" in check["message"]
    assert "not_logged_in" in check["message"]
    assert "login" in check["remediation"]
