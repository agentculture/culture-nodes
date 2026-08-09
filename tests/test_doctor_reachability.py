"""Tests for `nodes doctor`'s nodes_api_reachable check.

Uses a controlled --api-url (the fake server, or an unreachable port) so
these tests do not depend on whether anything happens to be listening on
the default 127.0.0.1:8080 in the environment running the suite.
"""

from __future__ import annotations

import json

from culture_nodes.cli import main


def _find_check(payload: dict, check_id: str) -> dict:
    for check in payload["checks"]:
        if check["id"] == check_id:
            return check
    raise AssertionError(f"no check named {check_id!r} in {payload['checks']!r}")


def test_doctor_reports_reachable_when_healthz_ok(fake_api, capsys) -> None:
    fake_api.route(
        "GET", r"/v1alpha1/healthz", lambda h, m, q, b: h.send_json(200, {"status": "ok"})
    )
    fake_api.start()
    rc = main(["doctor", "--json", "--api-url", fake_api.base_url])
    payload = json.loads(capsys.readouterr().out)
    check = _find_check(payload, "nodes_api_reachable")
    assert rc == 0
    assert payload["healthy"] is True
    assert check["passed"] is True
    assert check["severity"] == "warning"
    assert fake_api.base_url in check["message"]


def test_doctor_warns_but_stays_healthy_when_api_unreachable(capsys) -> None:
    # Port 1 is unbound; connecting there refuses immediately.
    rc = main(["doctor", "--json", "--api-url", "http://127.0.0.1:1"])
    payload = json.loads(capsys.readouterr().out)
    check = _find_check(payload, "nodes_api_reachable")

    # The whole point of this check being severity="warning": doctor stays
    # healthy (exit 0) even though the API is unreachable, because the
    # identity verbs (whoami/learn/explain/overview) work with no API at all.
    assert rc == 0
    assert payload["healthy"] is True
    assert check["passed"] is False
    assert check["severity"] == "warning"
    assert "not reachable" in check["message"]
    assert "nodes serve" in check["remediation"]


def test_doctor_text_mode_shows_fail_marker_without_failing_overall(capsys) -> None:
    rc = main(["doctor", "--api-url", "http://127.0.0.1:1"])
    out = capsys.readouterr().out
    assert rc == 0
    assert "culture-nodes doctor: healthy" in out
    assert "[FAIL] nodes_api_reachable" in out
