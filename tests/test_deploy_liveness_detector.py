"""Fake-host coverage for deploy.sh's lane-liveness detector tail (issue
#308, plan loop-closure-claude-codex task t11, spec c4/h13).

After ``nodes doctor`` runs on thor and orin, the deploy prints ONE line per
lane with the bridge's own ``liveness`` fact, read off ``/v1/capabilities``
(codex, port 8086) as the engine account — the same ssh/curl shape the codex
preflight step already uses. It is a detector, never a gate: a dead lane, an
unreadable bridge, and a bridge that predates the fact all print a line and
leave the exit code alone. The 2026-09-07 incident is why the line exists —
``codex login status`` said "Logged in" while every dispatch failed on a
revoked refresh token; the bridge's CHECK probe is the fact that would have
said otherwise, and a deploy is the moment an operator is already looking.

Same harness style as tests/test_deploy_two_host.py (its ``Harness`` is
reused): the lane file's marked block is executed under ``set -euo
pipefail`` with an ``ssh`` shim that runs the "remote" command locally
against a per-host HOME, and a ``curl`` shim that answers
``/v1/capabilities`` from ``FAKE_LIVENESS``.
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

from tests.test_deploy_two_host import ORIN, THOR, Harness, _block, _write_exec

ROOT = Path(__file__).resolve().parents[1]
DEPLOY = ROOT / "deploy/prod/deploy.sh"
LANE = ROOT / "deploy/prod/lanes/liveness-detector.sh"
MARKERS = ("# LIVENESS_DETECTOR_START", "# LIVENESS_DETECTOR_END")

_LIVE = (
    '{"session_ok": true, "reason": "ok", '
    '"checked_at": "2026-09-07T10:00:00+00:00", "mode": "CHECK"}'
)
_DEAD = (
    '{"session_ok": false, "reason": "refresh_token_spent", '
    '"checked_at": "2026-09-07T10:05:00+00:00", "mode": "LOCK"}'
)

# Answers /v1/capabilities with a host block carrying FAKE_LIVENESS (or no
# liveness key at all when it is unset — a bridge older than t9), and records
# the argv so a test can assert the bearer header was sent and the token was
# not printed anywhere.
_CURL_SHIM = """#!/usr/bin/env bash
printf 'curl[%s] %s\\n' "$FAKE_HOST" "$*" >> "$FAKE_LOG"
[ "${FAKE_CURL_EXIT:-0}" = 0 ] || exit "$FAKE_CURL_EXIT"
case "$*" in
  */v1/capabilities*)
    if [ -n "${FAKE_LIVENESS:-}" ]; then
      printf '{"host": {"hostname": "%s", "liveness": %s}}' "$FAKE_HOST" "$FAKE_LIVENESS"
    else
      printf '{"host": {"hostname": "%s"}}' "$FAKE_HOST"
    fi
    ;;
esac
exit 0
"""


def _account(h: Harness, host: str, token: str = "fake-bridge-bearer") -> Path:
    home = h.account(host, "clean")
    (home / ".culture-nodes").mkdir(parents=True, exist_ok=True)
    (home / ".culture-nodes/codex-bridge.env").write_text(f"CODEX_BRIDGE_AUTH_TOKEN={token}\n")
    return home


def _run(h: Harness, host: str, **fake_env: str) -> subprocess.CompletedProcess:
    _write_exec(h.bin / "curl", _CURL_SHIM)
    script = (
        "set -euo pipefail\n"
        "say() { printf '==> %s\\n' \"$*\"; }\n"
        'unix_user_target() { case "$1" in spark*) printf \'culture-%s@localhost\' "$2";; '
        '*) printf \'culture-%s@%s\' "$2" "$1";; esac; }\n'
        + _block(LANE, MARKERS)
        + f'\nlane_liveness_detector "{host}"\necho "exit-reached"\n'
    )
    env = {
        "PATH": f"{h.bin}{os.pathsep}{os.environ['PATH']}",
        "HOME": str(h.tmp / "operator"),
        "FAKE_LOG": str(h.log),
        "FAKE_HOSTS": str(h.hosts),
        **fake_env,
    }
    return subprocess.run(  # nosec B603 - fixed bash over extracted repository script
        ["bash", "-c", script],
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )


def test_deploy_sources_the_lane_and_calls_it_after_doctor_on_both_hosts():
    script = DEPLOY.read_text()
    assert 'source "$SCRIPT_DIR/lanes/liveness-detector.sh"' in script
    thor_case = script[script.index("thor*)") : script.index("orin*)")]
    orin_case = script[script.index("orin*)") : script.index("spark*)")]
    for case in (thor_case, orin_case):
        doctor_at = case.index("nodes doctor")
        detector_at = case.index('lane_liveness_detector "$HOST"')
        assert doctor_at < detector_at, "the liveness line follows the doctor detector"
        # A detector, not a gate: the call is never joined to an exit.
        call_line = [ln for ln in case.splitlines() if 'lane_liveness_detector "$HOST"' in ln][0]
        assert "exit" not in call_line and "||" not in call_line


def test_live_lane_prints_one_line_with_the_fact(tmp_path):
    h = Harness(tmp_path)
    _account(h, THOR)
    result = _run(h, THOR, FAKE_LIVENESS=_LIVE)
    assert result.returncode == 0, result.stderr
    lines = [ln for ln in result.stdout.splitlines() if "liveness[" in ln]
    assert len(lines) == 1, result.stdout
    (line,) = lines
    assert f"liveness[codex-thor@{THOR}]" in line
    assert "session_ok=true" in line
    assert "reason=ok" in line
    assert "mode=CHECK" in line
    assert "checked_at=2026-09-07T10:00:00+00:00" in line


def test_dead_lane_prints_the_reason_and_does_not_change_the_exit_code(tmp_path):
    h = Harness(tmp_path)
    _account(h, ORIN)
    result = _run(h, ORIN, FAKE_LIVENESS=_DEAD)
    assert result.returncode == 0, result.stderr
    assert "exit-reached" in result.stdout
    (line,) = [ln for ln in result.stdout.splitlines() if "liveness[" in ln]
    assert f"liveness[codex-orin@{ORIN}]" in line
    assert "session_ok=false" in line
    assert "reason=refresh_token_spent" in line
    assert "mode=LOCK" in line
    # The line says what a dead lane means for the next dispatch.
    assert "re-login" in line or "login" in line


def test_bridge_without_the_fact_prints_unmeasured(tmp_path):
    h = Harness(tmp_path)
    _account(h, THOR)
    result = _run(h, THOR)  # FAKE_LIVENESS unset: no liveness key in the host block
    assert result.returncode == 0, result.stderr
    (line,) = [ln for ln in result.stdout.splitlines() if "liveness[" in ln]
    assert "session_ok=unmeasured" in line
    assert "no liveness fact" in line


def test_unreadable_bridge_prints_unmeasured_and_does_not_fail(tmp_path):
    h = Harness(tmp_path)
    _account(h, THOR)
    result = _run(h, THOR, FAKE_LIVENESS=_LIVE, FAKE_CURL_EXIT="7")
    assert result.returncode == 0, result.stderr
    assert "exit-reached" in result.stdout
    (line,) = [ln for ln in result.stdout.splitlines() if "liveness[" in ln]
    assert "session_ok=unmeasured" in line
    assert "unreadable" in line


def test_missing_account_prints_unmeasured_and_does_not_fail(tmp_path):
    """An account that was never bootstrapped answers 255 like a real ssh;
    the detector says so and the deploy continues."""
    h = Harness(tmp_path)
    result = _run(h, THOR, FAKE_LIVENESS=_LIVE)
    assert result.returncode == 0, result.stderr
    (line,) = [ln for ln in result.stdout.splitlines() if "liveness[" in ln]
    assert "session_ok=unmeasured" in line


def test_the_probe_reads_capabilities_as_the_account_with_a_bearer_and_never_prints_it(
    tmp_path,
):
    h = Harness(tmp_path)
    _account(h, THOR, token="s3cret-bearer-value")
    result = _run(h, THOR, FAKE_LIVENESS=_LIVE)
    assert result.returncode == 0, result.stderr
    log = h.log.read_text()
    ssh_lines = [ln for ln in log.splitlines() if ln.startswith("ssh[")]
    assert ssh_lines and all(f"culture-codex@{THOR}" in ln for ln in ssh_lines), log
    curl_lines = [ln for ln in log.splitlines() if ln.startswith("curl[")]
    assert len(curl_lines) == 1, log
    assert "127.0.0.1:8086/v1/capabilities" in curl_lines[0]
    assert "Authorization: Bearer s3cret-bearer-value" in curl_lines[0]
    # Names travel; values do not: the operator's terminal never sees the token.
    assert "s3cret-bearer-value" not in result.stdout
    assert "s3cret-bearer-value" not in result.stderr
