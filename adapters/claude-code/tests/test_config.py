from __future__ import annotations

import json
import os

import pytest

from claude_code_bridge.config import Config, ConfigError


def test_defaults_are_safe_when_unconfigured():
    cfg = Config.load(env={})
    assert cfg.repo_allowlist == ()
    assert cfg.repo_allowed("/anywhere") is False
    assert cfg.auth_token is None
    assert cfg.claude_bin == "claude"
    assert cfg.permission_mode == "bypassPermissions"
    assert cfg.min_claude_version == "2.1.220"
    # t6 (c44/h37): sane defaults for the session-key concurrency guard —
    # serialized on, exactly one in-flight invocation per session_key.
    assert cfg.session_concurrency_enabled is True
    assert cfg.max_inflight_per_session_key == 1


def test_session_concurrency_env_parsing():
    cfg = Config.load(
        env={
            "CLAUDE_CODE_BRIDGE_SESSION_CONCURRENCY_ENABLED": "false",
            "CLAUDE_CODE_BRIDGE_MAX_INFLIGHT_PER_SESSION_KEY": "3",
        }
    )
    assert cfg.session_concurrency_enabled is False
    assert cfg.max_inflight_per_session_key == 3


def test_max_inflight_per_session_key_env_rejects_garbage():
    with pytest.raises(ConfigError):
        Config.load(env={"CLAUDE_CODE_BRIDGE_MAX_INFLIGHT_PER_SESSION_KEY": "many"})


def test_repo_allowlist_from_env_is_pathsep_joined(tmp_path):
    a = tmp_path / "a"
    b = tmp_path / "b"
    a.mkdir()
    b.mkdir()
    env = {"CLAUDE_CODE_BRIDGE_REPO_ALLOWLIST": f"{a}{os.pathsep}{b}"}
    cfg = Config.load(env=env)
    assert cfg.repo_allowed(str(a)) is True
    assert cfg.repo_allowed(str(b)) is True
    assert cfg.repo_allowed(str(tmp_path / "c")) is False


def test_repo_allowlist_normalizes_relative_and_symlinked_paths(tmp_path):
    real = tmp_path / "real"
    real.mkdir()
    link = tmp_path / "link"
    link.symlink_to(real)
    env = {"CLAUDE_CODE_BRIDGE_REPO_ALLOWLIST": str(link)}
    cfg = Config.load(env=env)
    assert cfg.repo_allowed(str(real)) is True


def test_config_file_sets_baseline_and_env_overrides_win(tmp_path):
    path = tmp_path / "bridge.json"
    path.write_text(json.dumps({"port": 9000, "actor_id": "from-file", "sync_max_steps": 3}))
    env = {"CLAUDE_CODE_BRIDGE_PORT": "9100"}
    cfg = Config.load(str(path), env=env)
    assert cfg.actor_id == "from-file"
    assert cfg.sync_max_steps == 3
    assert cfg.port == 9100  # env wins over file


def test_config_file_via_env_var(tmp_path):
    path = tmp_path / "bridge.json"
    path.write_text(json.dumps({"actor_id": "via-env-path"}))
    env = {"CLAUDE_CODE_BRIDGE_CONFIG": str(path)}
    cfg = Config.load(env=env)
    assert cfg.actor_id == "via-env-path"


def test_unknown_config_file_key_is_rejected(tmp_path):
    path = tmp_path / "bridge.json"
    path.write_text(json.dumps({"not_a_real_field": 1}))
    with pytest.raises(ConfigError):
        Config.load(str(path), env={})


def test_config_file_must_be_a_json_object(tmp_path):
    path = tmp_path / "bridge.json"
    path.write_text(json.dumps([1, 2, 3]))
    with pytest.raises(ConfigError):
        Config.load(str(path), env={})


def test_config_file_must_be_valid_json(tmp_path):
    path = tmp_path / "bridge.json"
    path.write_text("{not json")
    with pytest.raises(ConfigError):
        Config.load(str(path), env={})


def test_missing_config_file_raises(tmp_path):
    with pytest.raises(ConfigError):
        Config.load(str(tmp_path / "nope.json"), env={})


@pytest.mark.parametrize("raw,expected", [("1", True), ("true", True), ("0", False), ("no", False)])
def test_bool_env_parsing(raw, expected):
    cfg = Config.load(env={"CLAUDE_CODE_BRIDGE_ALWAYS_ASYNC": raw})
    assert cfg.always_async is expected


def test_bool_env_parsing_rejects_garbage():
    with pytest.raises(ConfigError):
        Config.load(env={"CLAUDE_CODE_BRIDGE_ALWAYS_ASYNC": "maybe"})


def test_int_and_float_env_parsing_reject_garbage():
    with pytest.raises(ConfigError):
        Config.load(env={"CLAUDE_CODE_BRIDGE_PORT": "not-a-number"})
    with pytest.raises(ConfigError):
        Config.load(env={"CLAUDE_CODE_BRIDGE_SYNC_TIMEOUT_SECONDS": "not-a-number"})


def test_claude_env_passthrough_from_file(tmp_path):
    path = tmp_path / "bridge.json"
    path.write_text(json.dumps({"claude_env": {"ANTHROPIC_API_KEY": "sk-test"}}))
    cfg = Config.load(str(path), env={})
    assert cfg.claude_env == {"ANTHROPIC_API_KEY": "sk-test"}


def test_min_claude_version_overridable_via_env():
    cfg = Config.load(env={"CLAUDE_CODE_BRIDGE_MIN_CLAUDE_VERSION": "2.1.221"})
    assert cfg.min_claude_version == "2.1.221"


def test_permission_mode_overridable_via_file(tmp_path):
    path = tmp_path / "bridge.json"
    path.write_text(json.dumps({"permission_mode": "acceptEdits"}))
    cfg = Config.load(str(path), env={})
    assert cfg.permission_mode == "acceptEdits"
