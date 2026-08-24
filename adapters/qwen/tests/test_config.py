from __future__ import annotations

import json
import os

import pytest
from qwen_bridge.config import Config, ConfigError


def test_defaults_are_safe_when_unconfigured():
    cfg = Config.load(env={})
    assert cfg.repo_allowlist == ()
    assert cfg.repo_allowed("/anywhere") is False
    assert cfg.auth_token is None
    assert cfg.qwen_bin == "qwen"
    assert cfg.default_sandbox == "workspace-write"
    # t6 (c44/h37): sane defaults for the session-key concurrency guard —
    # serialized on, exactly one in-flight invocation per session_key.
    assert cfg.session_concurrency_enabled is True
    assert cfg.max_inflight_per_session_key == 1


def test_session_concurrency_env_parsing():
    cfg = Config.load(
        env={
            "QWEN_BRIDGE_SESSION_CONCURRENCY_ENABLED": "false",
            "QWEN_BRIDGE_MAX_INFLIGHT_PER_SESSION_KEY": "3",
        }
    )
    assert cfg.session_concurrency_enabled is False
    assert cfg.max_inflight_per_session_key == 3


def test_max_inflight_per_session_key_env_rejects_garbage():
    with pytest.raises(ConfigError):
        Config.load(env={"QWEN_BRIDGE_MAX_INFLIGHT_PER_SESSION_KEY": "many"})


def test_repo_allowlist_from_env_is_pathsep_joined(tmp_path):
    a = tmp_path / "a"
    b = tmp_path / "b"
    a.mkdir()
    b.mkdir()
    env = {"QWEN_BRIDGE_REPO_ALLOWLIST": f"{a}{os.pathsep}{b}"}
    cfg = Config.load(env=env)
    assert cfg.repo_allowed(str(a)) is True
    assert cfg.repo_allowed(str(b)) is True
    assert cfg.repo_allowed(str(tmp_path / "c")) is False


def test_repo_allowlist_normalizes_relative_and_symlinked_paths(tmp_path):
    real = tmp_path / "real"
    real.mkdir()
    link = tmp_path / "link"
    link.symlink_to(real)
    env = {"QWEN_BRIDGE_REPO_ALLOWLIST": str(link)}
    cfg = Config.load(env=env)
    # A caller naming the resolved real path is recognised too, since the
    # allowlist stores canonical (symlink-resolved) paths.
    assert cfg.repo_allowed(str(real)) is True


def test_repo_allowlist_scoped_prefix_allows_children_but_not_siblings(tmp_path):
    root = tmp_path / ".worktrees.culture-nodes"
    root.mkdir()
    cfg = Config(repo_allowlist_prefixes=(str(root),))
    assert cfg.repo_allowed(str(root / "writer-a")) is True
    assert cfg.repo_allowed(str(tmp_path / ".worktrees.other" / "writer-a")) is False


def test_exact_allowlist_does_not_accidentally_contain_nested_worktrees(tmp_path):
    repo = tmp_path / "culture-nodes"
    nested = repo / ".claude" / "worktrees" / "web-ux-quick-wins"
    nested.mkdir(parents=True)
    cfg = Config(repo_allowlist=(str(repo),))
    assert cfg.repo_allowed(str(nested)) is False


def test_config_file_sets_baseline_and_env_overrides_win(tmp_path):
    path = tmp_path / "bridge.json"
    path.write_text(json.dumps({"port": 9000, "actor_id": "from-file", "sync_max_steps": 3}))
    env = {"QWEN_BRIDGE_PORT": "9100"}
    cfg = Config.load(str(path), env=env)
    assert cfg.actor_id == "from-file"  # file value kept where env doesn't override
    assert cfg.sync_max_steps == 3
    assert cfg.port == 9100  # env wins over file


def test_config_file_via_env_var(tmp_path):
    path = tmp_path / "bridge.json"
    path.write_text(json.dumps({"actor_id": "via-env-path"}))
    env = {"QWEN_BRIDGE_CONFIG": str(path)}
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
    cfg = Config.load(env={"QWEN_BRIDGE_ALWAYS_ASYNC": raw})
    assert cfg.always_async is expected


def test_bool_env_parsing_rejects_garbage():
    with pytest.raises(ConfigError):
        Config.load(env={"QWEN_BRIDGE_ALWAYS_ASYNC": "maybe"})


def test_int_and_float_env_parsing_reject_garbage():
    with pytest.raises(ConfigError):
        Config.load(env={"QWEN_BRIDGE_PORT": "not-a-number"})
    with pytest.raises(ConfigError):
        Config.load(env={"QWEN_BRIDGE_SYNC_TIMEOUT_SECONDS": "not-a-number"})


def test_qwen_env_passthrough_from_file(tmp_path):
    path = tmp_path / "bridge.json"
    path.write_text(json.dumps({"qwen_env": {"QWEN_HOME": "/srv/qwen-bridge/.qwen"}}))
    cfg = Config.load(str(path), env={})
    assert cfg.qwen_env == {"QWEN_HOME": "/srv/qwen-bridge/.qwen"}


def test_default_sandbox_overridable_from_env():
    cfg = Config.load(env={"QWEN_BRIDGE_DEFAULT_SANDBOX": "read-only"})
    assert cfg.default_sandbox == "read-only"


def test_default_port_differs_from_colleague_bridge():
    # colleague-bridge defaults to 8085; this bridge picks a different
    # default so both can run on one host without colliding.
    cfg = Config.load(env={})
    assert cfg.port == 8086


def test_only_allowed_repo_infers_one_and_refuses_ambiguity():
    """Issue #125: a trigger-created run's input IS the event payload.

    There is nowhere in a deployment-neutral workflow to put a checkout path
    — a literal in the graph would make it deployment-specific, and the pure
    emitter that raises the event knows nothing about checkouts. So a bridge
    naming exactly one repository infers it; the caller restating it would add
    no safety, because the allowlist check would reject anything else anyway.

    Ambiguity fails closed. With two entries, or with any prefix rule, the
    choice is real and guessing it would silently pick a workspace the caller
    did not name.
    """
    cfg = Config(repo_allowlist=("/srv/only",))
    assert cfg.only_allowed_repo() == "/srv/only"

    assert Config(repo_allowlist=()).only_allowed_repo() is None
    assert Config(repo_allowlist=("/srv/a", "/srv/b")).only_allowed_repo() is None

    # A prefix rule means the allowlist admits repos it does not enumerate,
    # so "exactly one entry" no longer means "exactly one possible repo".
    assert (
        Config(
            repo_allowlist=("/srv/only",), repo_allowlist_prefixes=("/srv/scoped",)
        ).only_allowed_repo()
        is None
    )
