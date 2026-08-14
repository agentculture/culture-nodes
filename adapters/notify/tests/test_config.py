"""Config precedence and validation, mirroring the sibling bridges'
test_config.py: defaults, file baseline, NOTIFY_BRIDGE_* env overrides on
top, and loud errors for values the bridge cannot use.

Also proves the webhook URL has NO surface here at all: it is not a
`Config` field, not a recognised env var, and not an accepted file key --
`webhook.resolve_webhook()` is the only path to it."""

from __future__ import annotations

import dataclasses
import json

import pytest

from notify_bridge.config import _ENV_STRING_FIELDS, _FILE_FIELDS, Config, ConfigError


def test_defaults():
    cfg = Config.load(env={})
    assert cfg.actor_id == "notify-bridge"
    assert cfg.host == "127.0.0.1"
    assert cfg.port == 8088
    assert cfg.auth_token is None
    assert cfg.state_dir == ".notify-bridge-state"


def test_file_baseline(tmp_path):
    path = tmp_path / "cfg.json"
    path.write_text(
        json.dumps({"port": 9000, "auth_token": "tok", "actor_id": "company/notify-discord"}),
        encoding="utf-8",
    )
    cfg = Config.load(str(path), env={})
    assert cfg.port == 9000
    assert cfg.auth_token == "tok"
    assert cfg.actor_id == "company/notify-discord"


def test_env_overrides_file(tmp_path):
    path = tmp_path / "cfg.json"
    path.write_text(json.dumps({"port": 9000, "state_dir": "from-file"}), encoding="utf-8")
    env = {
        "NOTIFY_BRIDGE_PORT": "9100",
        "NOTIFY_BRIDGE_STATE_DIR": "from-env",
        "NOTIFY_BRIDGE_AUTH_TOKEN": "envtok",
    }
    cfg = Config.load(str(path), env=env)
    assert cfg.port == 9100
    assert cfg.state_dir == "from-env"
    assert cfg.auth_token == "envtok"


def test_config_file_via_env(tmp_path):
    path = tmp_path / "cfg.json"
    path.write_text(json.dumps({"port": 9001}), encoding="utf-8")
    cfg = Config.load(env={"NOTIFY_BRIDGE_CONFIG": str(path)})
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
        Config.load(env={"NOTIFY_BRIDGE_PORT": "not-a-number"})


# -- the webhook URL has no config surface ------------------------------------


def test_config_has_no_webhook_url_field():
    field_names = {f.name for f in dataclasses.fields(Config)}
    assert not any("webhook" in name.lower() or "url" in name.lower() for name in field_names)


def test_no_env_var_maps_to_a_webhook_field():
    for env_name in _ENV_STRING_FIELDS:
        assert "webhook" not in env_name.lower()


def test_webhook_url_is_not_an_accepted_file_key(tmp_path):
    path = tmp_path / "cfg.json"
    path.write_text(json.dumps({"webhook_url": "https://discord.com/x"}), encoding="utf-8")
    with pytest.raises(ConfigError):
        Config.load(str(path), env={})
    assert "webhook_url" not in _FILE_FIELDS
