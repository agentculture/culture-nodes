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

The fixture answers ``/v1/capabilities`` with the REAL document shape every
bridge serves — ``preflight.capability_block`` wrapped as
``{"preflight": {"protocol_version": "1.0", "host": {..., "liveness":
{...}}}}`` — and derives it from the adapter's own ``preflight.py``
(stdlib-only, no package imports, loaded straight off the file) so the flat
``{"host": ...}`` document no bridge emits cannot drift back in.

Same harness style as tests/test_deploy_two_host.py (its ``Harness`` is
reused): the lane file's marked block is executed under ``set -euo
pipefail`` with an ``ssh`` shim that runs the "remote" command locally
against a per-host HOME, and a ``curl`` shim that answers
``/v1/capabilities`` from ``FAKE_CAPABILITIES``.
"""

from __future__ import annotations

import importlib.util
import json
import os
import subprocess
from pathlib import Path
from types import ModuleType

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


# The shared preflight module, loaded off the file: it is stdlib-only and
# imports nothing from its own package (that is what makes the
# byte-identical rule cheap to hold), so the detector's fixture can derive
# the wire shape from the same source the bridge serves it from.
def _load_preflight() -> ModuleType:
    spec = importlib.util.spec_from_file_location(
        "liveness_detector_preflight", ROOT / "adapters/codex/src/codex_bridge/preflight.py"
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


_PREFLIGHT = _load_preflight()


def _capabilities_doc(hostname: str, liveness: str | None) -> str:
    """GET /v1/capabilities exactly as a bridge serves it: the
    ``capability_block`` the actor registration carries, ``liveness`` present
    only when a fact is being advertised (None models a bridge predating t9).
    """
    facts: dict[str, object] = {"hostname": hostname, "commit_policy": "none"}
    if liveness is not None:
        facts["liveness"] = json.loads(liveness)
    return json.dumps(_PREFLIGHT.capability_block(_PREFLIGHT.host_block(**facts)))


# Answers /v1/capabilities with the derived document and records the argv so
# a test can assert the bearer header was sent and the token was not printed
# anywhere.
_CURL_SHIM = """#!/usr/bin/env bash
printf 'curl[%s] %s\\n' "$FAKE_HOST" "$*" >> "$FAKE_LOG"
[ "${FAKE_CURL_EXIT:-0}" = 0 ] || exit "$FAKE_CURL_EXIT"
case "$*" in
  */v1/capabilities*)
    printf '%s\\n' "$FAKE_CAPABILITIES"
    ;;
esac
exit 0
"""


def _account(h: Harness, host: str, token: str = "fake-bridge-bearer") -> Path:
    home = h.account(host, "clean")
    (home / ".culture-nodes").mkdir(parents=True, exist_ok=True)
    (home / ".culture-nodes/codex-bridge.env").write_text(f"CODEX_BRIDGE_AUTH_TOKEN={token}\n")
    return home


def _run(
    h: Harness, host: str, *, liveness: str | None = None, **fake_env: str
) -> subprocess.CompletedProcess:
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
        "FAKE_CAPABILITIES": _capabilities_doc(host, liveness),
        **fake_env,
    }
    return subprocess.run(  # nosec B603 - fixed bash over extracted repository script
        ["bash", "-c", script],
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )


def test_the_fixture_derives_the_wrapped_shape_from_the_adapter():
    """The detector reads ``preflight.host.liveness``; the fixture must be
    the document a bridge actually serves, or a shape drift on either side
    turns a live lane into 'predates t9' silently. Deriving it from
    ``preflight.capability_block`` is what keeps the two in step."""
    doc = json.loads(_capabilities_doc(THOR, _LIVE))
    assert doc["preflight"]["protocol_version"] == _PREFLIGHT.PROTOCOL_VERSION
    host = doc["preflight"]["host"]
    assert host["liveness"]["session_ok"] is True
    # No flat `host` at the top level: that is the shape no bridge emits.
    assert "host" not in doc


def test_deploy_sources_the_lane_and_calls_it_after_doctor_on_both_hosts():
    script = DEPLOY.read_text()
    assert 'source "$SCRIPT_DIR/lanes/liveness-detector.sh"' in script
    thor_case = script[script.index("thor*)") : script.index("orin*)")]
    orin_case = script[script.index("orin*)") : script.index("spark*)")]
    for case in (thor_case, orin_case):
        doctor_at = case.index("nodes doctor")
        detector_at = case.index('lane_liveness_detector "$HOST"')
        assert doctor_at < detector_at, "the liveness line follows the doctor detector"
        # A detector, not a gate: the call is joined neither to an exit nor to a `||`.
        call_line = [ln for ln in case.splitlines() if 'lane_liveness_detector "$HOST"' in ln][0]
        assert "exit" not in call_line
        assert "||" not in call_line


def test_live_lane_prints_one_line_with_the_fact(tmp_path):
    h = Harness(tmp_path)
    _account(h, THOR)
    result = _run(h, THOR, liveness=_LIVE)
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
    result = _run(h, ORIN, liveness=_DEAD)
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
    result = _run(h, THOR)  # liveness=None: no liveness key in the host block
    assert result.returncode == 0, result.stderr
    (line,) = [ln for ln in result.stdout.splitlines() if "liveness[" in ln]
    assert "session_ok=unmeasured" in line
    assert "no liveness fact" in line


def test_unreadable_bridge_prints_unmeasured_and_does_not_fail(tmp_path):
    h = Harness(tmp_path)
    _account(h, THOR)
    result = _run(h, THOR, liveness=_LIVE, FAKE_CURL_EXIT="7")
    assert result.returncode == 0, result.stderr
    assert "exit-reached" in result.stdout
    (line,) = [ln for ln in result.stdout.splitlines() if "liveness[" in ln]
    assert "session_ok=unmeasured" in line
    assert "unreadable" in line


def test_missing_account_prints_unmeasured_and_does_not_fail(tmp_path):
    """An account that was never bootstrapped answers 255 like a real ssh;
    the detector says so and the deploy continues."""
    h = Harness(tmp_path)
    result = _run(h, THOR, liveness=_LIVE)
    assert result.returncode == 0, result.stderr
    (line,) = [ln for ln in result.stdout.splitlines() if "liveness[" in ln]
    assert "session_ok=unmeasured" in line


def test_the_probe_reads_capabilities_as_the_account_with_a_bearer_and_never_prints_it(
    tmp_path,
):
    h = Harness(tmp_path)
    _account(h, THOR, token="s3cret-bearer-value")
    result = _run(h, THOR, liveness=_LIVE)
    assert result.returncode == 0, result.stderr
    log = h.log.read_text()
    ssh_lines = [ln for ln in log.splitlines() if ln.startswith("ssh[")]
    assert ssh_lines, log
    assert all(f"culture-codex@{THOR}" in ln for ln in ssh_lines), log
    curl_lines = [ln for ln in log.splitlines() if ln.startswith("curl[")]
    assert len(curl_lines) == 1, log
    assert "127.0.0.1:8086/v1/capabilities" in curl_lines[0]
    assert "Authorization: Bearer s3cret-bearer-value" in curl_lines[0]
    # Names travel; values do not: the operator's terminal never sees the token.
    assert "s3cret-bearer-value" not in result.stdout
    assert "s3cret-bearer-value" not in result.stderr
