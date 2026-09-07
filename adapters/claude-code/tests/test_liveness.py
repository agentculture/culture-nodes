"""Bridge-side lane liveness (issue #308 detector, plan
loop-closure-claude-codex task t9) — claude-code's half.

claude-code has no dry-refresh probe to spend: its credential file already
states when the OAuth access token expires (`~/.claude/.credentials.json`,
`claudeAiOauth.expiresAt`, a millisecond epoch — never read by anything
before this task). So the fact here is a file read, and the two acceptance
cases are: an `expiresAt` in the past yields `session_ok=false
reason=credential_expired`; a missing file yields `reason=unmeasured` and
never a crash.
"""

from __future__ import annotations

import json

import pytest

from claude_code_bridge import capabilities, liveness, preflight
from claude_code_bridge.config import Config, ConfigError


def _credentials(tmp_path, expires_at_ms):
    path = tmp_path / ".credentials.json"
    path.write_text(
        json.dumps({"claudeAiOauth": {"accessToken": "x", "expiresAt": expires_at_ms}}),
        encoding="utf-8",
    )
    return path


NOW = 1_800_000_000.0


def test_a_past_expires_at_is_an_expired_credential(tmp_path):
    cfg = Config(credentials_path=str(_credentials(tmp_path, int((NOW - 60) * 1000))))
    fact = capabilities.liveness_fact(cfg, now=NOW)
    assert fact["session_ok"] is False
    assert fact["reason"] == liveness.REASON_CREDENTIAL_EXPIRED
    assert fact["mode"] == "LOCK"


def test_a_future_expires_at_is_a_live_session(tmp_path):
    cfg = Config(credentials_path=str(_credentials(tmp_path, int((NOW + 3600) * 1000))))
    fact = capabilities.liveness_fact(cfg, now=NOW)
    assert fact["session_ok"] is True
    assert fact["reason"] == liveness.REASON_OK


def test_a_missing_credentials_file_is_unmeasured_never_a_crash(tmp_path):
    cfg = Config(credentials_path=str(tmp_path / "absent" / ".credentials.json"))
    fact = capabilities.liveness_fact(cfg, now=NOW)
    assert fact["session_ok"] is None
    assert fact["reason"] == liveness.REASON_UNMEASURED


def test_a_malformed_credentials_file_is_unmeasured_never_a_crash(tmp_path):
    path = tmp_path / ".credentials.json"
    path.write_text("{oops", encoding="utf-8")
    assert capabilities.liveness_fact(Config(credentials_path=str(path)), now=NOW)["reason"] == (
        liveness.REASON_UNMEASURED
    )
    path.write_text(json.dumps({"claudeAiOauth": {"expiresAt": "soon"}}), encoding="utf-8")
    assert capabilities.liveness_fact(Config(credentials_path=str(path)), now=NOW)["reason"] == (
        liveness.REASON_UNMEASURED
    )


def test_the_credentials_path_expands_the_home_directory(tmp_path, monkeypatch):
    monkeypatch.setenv("HOME", str(tmp_path))
    _credentials(tmp_path, int((NOW - 1) * 1000))
    cfg = Config(credentials_path="~/.credentials.json")
    assert capabilities.liveness_fact(cfg, now=NOW)["reason"] == liveness.REASON_CREDENTIAL_EXPIRED


def test_the_default_path_is_the_one_claude_writes():
    assert Config().credentials_path == "~/.claude/.credentials.json"


def test_the_host_surface_carries_the_fact_and_still_validates(tmp_path):
    cfg = Config(
        repo_allowlist=(str(tmp_path),),
        credentials_path=str(_credentials(tmp_path, int((NOW - 60) * 1000))),
    )
    host = capabilities.host_facts(
        cfg, probes=((str(tmp_path / "absent-knob"), "1"),), git_probe=None, now=NOW
    )
    assert host["liveness"]["session_ok"] is False
    assert host["liveness"]["reason"] == "credential_expired"
    preflight.validate_block(preflight.capability_block(host))


def test_the_mode_is_read_from_the_lane_configuration(tmp_path):
    cfg = Config.load(
        env={
            "CLAUDE_CODE_BRIDGE_LIVENESS_MODE": "check",
            "CLAUDE_CODE_BRIDGE_CREDENTIALS_PATH": str(tmp_path / "c.json"),
        }
    )
    assert cfg.liveness_mode == "CHECK"
    assert cfg.credentials_path == str(tmp_path / "c.json")
    assert capabilities.liveness_fact(cfg, now=NOW)["mode"] == "CHECK"

    path = tmp_path / "bridge.json"
    path.write_text(json.dumps({"liveness_mode": "LOCK"}), encoding="utf-8")
    assert Config.load(str(path), env={}).liveness_mode == "LOCK"


def test_an_unknown_liveness_mode_is_a_config_error_at_load():
    with pytest.raises(ConfigError):
        Config.load(env={"CLAUDE_CODE_BRIDGE_LIVENESS_MODE": "maybe"})


def test_liveness_is_an_agreed_host_key_here_too():
    assert "liveness" in preflight.HOST_KEYS
    assert liveness.LIVENESS_KEYS == ("session_ok", "reason", "checked_at", "mode")
