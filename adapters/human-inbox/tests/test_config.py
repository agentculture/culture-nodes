"""Config precedence and validation, mirroring the sibling bridges'
test_config.py: defaults, file baseline, HUMAN_INBOX_BRIDGE_* env overrides
on top, and loud errors for values the bridge cannot use."""

from __future__ import annotations

import json

import pytest

from human_inbox_bridge.config import Config, ConfigError


def test_defaults():
    cfg = Config.load(env={})
    assert cfg.actor_id == "human-inbox-bridge"
    assert cfg.host == "127.0.0.1"
    assert cfg.port == 8087
    assert cfg.auth_token is None
    # A human actor makes no liveness promise: 0 means the control plane's
    # wait is genuinely open-ended (internal/worker/dispatch.go asyncDeadline).
    assert cfg.heartbeat_after_seconds == 0
    assert cfg.state_dir == ".human-inbox-bridge-state"
    assert cfg.default_success_outcome == "completed"
    assert cfg.callback_max_retries == 5


def test_file_baseline(tmp_path):
    path = tmp_path / "cfg.json"
    path.write_text(
        json.dumps({"port": 9000, "auth_token": "tok", "actor_id": "ops/humans"}),
        encoding="utf-8",
    )
    cfg = Config.load(str(path), env={})
    assert cfg.port == 9000
    assert cfg.auth_token == "tok"
    assert cfg.actor_id == "ops/humans"


def test_env_overrides_file(tmp_path):
    path = tmp_path / "cfg.json"
    path.write_text(json.dumps({"port": 9000, "state_dir": "from-file"}), encoding="utf-8")
    env = {
        "HUMAN_INBOX_BRIDGE_PORT": "9100",
        "HUMAN_INBOX_BRIDGE_STATE_DIR": "from-env",
        "HUMAN_INBOX_BRIDGE_AUTH_TOKEN": "envtok",
        "HUMAN_INBOX_BRIDGE_HEARTBEAT_AFTER_SECONDS": "30",
        "HUMAN_INBOX_BRIDGE_CALLBACK_TIMEOUT_SECONDS": "2.5",
    }
    cfg = Config.load(str(path), env=env)
    assert cfg.port == 9100
    assert cfg.state_dir == "from-env"
    assert cfg.auth_token == "envtok"
    assert cfg.heartbeat_after_seconds == 30
    assert cfg.callback_timeout_seconds == 2.5


def test_config_file_via_env(tmp_path):
    path = tmp_path / "cfg.json"
    path.write_text(json.dumps({"port": 9001}), encoding="utf-8")
    cfg = Config.load(env={"HUMAN_INBOX_BRIDGE_CONFIG": str(path)})
    assert cfg.port == 9001


def test_unknown_file_key_is_an_error(tmp_path):
    path = tmp_path / "cfg.json"
    path.write_text(json.dumps({"no_such_key": 1}), encoding="utf-8")
    with pytest.raises(ConfigError):
        Config.load(str(path), env={})


def test_invalid_json_is_an_error(tmp_path):
    path = tmp_path / "cfg.json"
    path.write_text("{not json", encoding="utf-8")
    with pytest.raises(ConfigError):
        Config.load(str(path), env={})


def test_non_object_json_is_an_error(tmp_path):
    path = tmp_path / "cfg.json"
    path.write_text("[1, 2]", encoding="utf-8")
    with pytest.raises(ConfigError):
        Config.load(str(path), env={})


def test_missing_file_is_an_error():
    with pytest.raises(ConfigError):
        Config.load("/no/such/config.json", env={})


def test_bad_int_env_is_an_error():
    with pytest.raises(ConfigError):
        Config.load(env={"HUMAN_INBOX_BRIDGE_PORT": "not-a-number"})


def test_bad_float_env_is_an_error():
    with pytest.raises(ConfigError):
        Config.load(env={"HUMAN_INBOX_BRIDGE_CALLBACK_TIMEOUT_SECONDS": "abc"})
