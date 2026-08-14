"""Tests for `nodes doctor`'s unprivileged_userns check.

The check exists because an actor dispatched onto a host that forbids
unprivileged user namespaces cannot be sandboxed by bubblewrap — and codex
does not fail loudly when that happens. It keeps running shell commands
unconfined while every `apply_patch` dies, so the session reads fine, writes
nothing, and burns its budget retrying. Two lanes were lost that way before
this check existed.

The probe list is injected rather than mocked, so these assert the logic on
both kinds of kernel instead of on whichever one runs the suite.
"""

from __future__ import annotations

import json

from culture_nodes.cli import main
from culture_nodes.cli._commands.doctor import _USERNS_SYSCTLS, _userns_check


def _find_check(payload: dict, check_id: str) -> dict:
    for check in payload["checks"]:
        if check["id"] == check_id:
            return check
    raise AssertionError(f"no check named {check_id!r} in {payload['checks']!r}")


def _probes(tmp_path, files: dict[str, str]) -> tuple[tuple[str, str], ...]:
    """Build a probe list pointing at temp files, preserving each real
    sysctl's blocking value so the test exercises the shipped mapping."""
    blocking = {path.rsplit("/", 1)[-1]: value for path, value in _USERNS_SYSCTLS}
    out = []
    for name, contents in files.items():
        p = tmp_path / name
        p.write_text(contents)
        out.append((str(p), blocking[name]))
    return tuple(out)


def test_absent_knobs_mean_unrestricted(tmp_path):
    """A kernel with neither sysctl present does not restrict here."""
    missing = ((str(tmp_path / "nope"), "1"),)
    result = _userns_check(probes=missing)
    assert result["passed"] is True
    assert result["remediation"] == ""


def test_apparmor_restriction_is_named_with_its_consequence(tmp_path):
    result = _userns_check(
        probes=_probes(tmp_path, {"apparmor_restrict_unprivileged_userns": "1\n"})
    )
    assert result["passed"] is False
    assert "apparmor_restrict_unprivileged_userns=1" in result["message"]
    # The message must state what actually goes wrong, because the failure is
    # silent: writes vanish while shell commands keep working.
    assert "workspace-write" in result["message"]
    assert "danger-full-access" in result["remediation"]


def test_debian_family_knob_is_detected(tmp_path):
    result = _userns_check(probes=_probes(tmp_path, {"unprivileged_userns_clone": "0\n"}))
    assert result["passed"] is False
    assert "unprivileged_userns_clone=0" in result["message"]


def test_permitting_values_pass(tmp_path):
    result = _userns_check(
        probes=_probes(
            tmp_path,
            {"apparmor_restrict_unprivileged_userns": "0\n", "unprivileged_userns_clone": "1\n"},
        )
    )
    assert result["passed"] is True
    assert result["remediation"] == ""


def test_both_blockers_are_reported_together(tmp_path):
    result = _userns_check(
        probes=_probes(
            tmp_path,
            {"apparmor_restrict_unprivileged_userns": "1\n", "unprivileged_userns_clone": "0\n"},
        )
    )
    assert result["passed"] is False
    assert "apparmor_restrict_unprivileged_userns=1" in result["message"]
    assert "unprivileged_userns_clone=0" in result["message"]


def test_doctor_surfaces_the_check_as_a_warning(capsys):
    """severity=warning by design: the CLI's own verbs need no sandbox, so a
    restricted host is reported to the operator, never fatal."""
    # An unbound port keeps the API probe from reaching anything real.
    rc = main(["doctor", "--json", "--api-url", "http://127.0.0.1:1"])
    payload = json.loads(capsys.readouterr().out)
    check = _find_check(payload, "unprivileged_userns")
    assert check["severity"] == "warning"
    # Whatever this host's kernel reports, the check must not decide health.
    assert payload["healthy"] == all(
        c["passed"] for c in payload["checks"] if c["severity"] == "error"
    )
    assert rc == (0 if payload["healthy"] else 1)
