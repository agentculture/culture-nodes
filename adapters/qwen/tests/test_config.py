from __future__ import annotations

import json
import os
import shutil
import subprocess
from pathlib import Path

import pytest

from qwen_bridge.config import (
    AgentIdentity,
    Config,
    ConfigError,
    ConfigLegError,
    load_agent_config_source,
)


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


# --- the #114 configured-repo load (plan task t4, issue #114) -------------
#
# The image carries no agent identity: the configured repository IS the
# config. At startup the bridge loads the culture.yaml + AGENTS.md +
# .qwen/skills triple from it, fails closed with a DISTINCT message per
# missing leg, and records which repo + revision (the clone HEAD) it loaded
# on the Config the capability layer already receives (plan tasks t3/t5 wire
# that record into the capability document).
#
# "No invoke served" is a property of WHERE the refusal lands: a leg
# refusal is a ConfigError raised by the startup load, and the entrypoint
# catches ConfigError at boot (print + exit 2) before dialin/serve — so the
# bridge that refuses a leg never opens its port.
#
# The fixture clones are materialized from the tracked content under
# tests/fixtures/config-repo-*/ with a pinned branch, message,
# author/committer ident and date, so their HEAD is a deterministic, known
# revision on every host that runs the suite.

_FIXTURES = Path(__file__).parent / "fixtures"

#: The pinned git recipe the fixture materialization commits with. The date
#: is the 2026-08-23 vendoring date of the .qwen/skills surface this fixture
#: models.
_FIXTURE_GIT_ENV = {
    "GIT_AUTHOR_NAME": "qwen-bridge fixture",
    "GIT_AUTHOR_EMAIL": "qwen-bridge-fixture@example.com",
    "GIT_AUTHOR_DATE": "2026-08-23T00:00:00Z",
    "GIT_COMMITTER_NAME": "qwen-bridge fixture",
    "GIT_COMMITTER_EMAIL": "qwen-bridge-fixture@example.com",
    "GIT_COMMITTER_DATE": "2026-08-23T00:00:00Z",
}

#: The complete fixture's deterministic clone HEAD, measured twice from the
#: pinned recipe (two materializations, one value).
KNOWN_COMPLETE_REVISION = "79e599b9fcb9a14240894d75d0d4a49fce0ad81b"


def _materialize_clone(root: Path, name: str, *, dest: str | None = None, git: bool = True) -> Path:
    """Copy the tracked fixture content *name* into *root* and, when *git*,
    commit it with the pinned recipe so HEAD is deterministic."""
    clone = root / (dest or name)
    shutil.copytree(_FIXTURES / name, clone)
    if not git:
        return clone
    env = {**os.environ, **_FIXTURE_GIT_ENV}
    for args in (
        ("init", "-q", "-b", "main"),
        ("add", "-A"),
        ("commit", "-q", "-m", "config-repo fixture"),
    ):
        subprocess.run(["git", *args], cwd=clone, env=env, check=True, capture_output=True)
    return clone


def test_the_loaded_triple_reports_the_clone_head(tmp_path):
    """AC2: the loaded config repo + revision (the clone HEAD) flow out of
    the config module, asserted against a fixture clone at a known
    revision."""
    clone = _materialize_clone(tmp_path, "config-repo-complete")
    cfg = Config(config_repo=str(clone))
    assert cfg.agent_config is None  # nothing is loaded before the startup load
    source = load_agent_config_source(cfg)
    # The record rides on the Config the capability layer receives — this is
    # the exact field t3's builder and t5 consume (AgentConfigSource.repo /
    # .revision via cfg.agent_config).
    assert source is cfg.agent_config
    assert source.repo == str(clone.resolve())
    assert source.revision == KNOWN_COMPLETE_REVISION
    assert source.agent == AgentIdentity(
        suffix="culture-nodes",
        backend="colleague",
        model="sakamakismile/Qwen3.6-27B-Text-NVFP4-MTP",
    )
    assert source.culture_path == str((clone / "culture.yaml").resolve())
    assert source.prompt_file == str((clone / "AGENTS.md").resolve())
    assert source.skills_dir == str((clone / ".qwen" / "skills").resolve())
    assert source.skill_count == 1


def test_the_single_allowlisted_repo_is_the_configured_repository(tmp_path):
    """No explicit config_repo: the image deployment clones exactly one
    repository (the single allowlist entry), and that one is the identity
    source."""
    clone = _materialize_clone(tmp_path, "config-repo-complete")
    source = load_agent_config_source(Config(repo_allowlist=(str(clone),)))
    assert source.repo == str(clone.resolve())
    assert source.revision == KNOWN_COMPLETE_REVISION


def test_an_explicit_config_repo_wins_over_the_allowlist(tmp_path):
    clone = _materialize_clone(tmp_path, "config-repo-complete")
    other = _materialize_clone(tmp_path, "config-repo-complete", dest="other")
    cfg = Config(config_repo=str(clone), repo_allowlist=(str(other),))
    source = load_agent_config_source(cfg)
    assert source.repo == str(clone.resolve())
    assert source.revision == KNOWN_COMPLETE_REVISION


def test_a_missing_culture_yaml_refuses_with_the_identity_leg_message(tmp_path):
    """AC1, leg 1: no culture.yaml. The refusal is DISTINCT (names the
    culture.yaml leg, not the others), and it is a boot-time ConfigError, so
    the entrypoint exits before serving — no invoke is ever reached."""
    clone = _materialize_clone(tmp_path, "config-repo-no-culture-yaml")
    cfg = Config(config_repo=str(clone))
    with pytest.raises(ConfigLegError) as excinfo:
        load_agent_config_source(cfg)
    message = str(excinfo.value)
    assert "culture.yaml" in message
    assert "AGENTS.md" not in message
    assert ".qwen/skills" not in message
    assert cfg.agent_config is None  # no partial record lands on the Config


def test_a_missing_agents_md_refuses_with_the_prompt_file_leg_message(tmp_path):
    """AC1, leg 2: no AGENTS.md — the qwen prompt-file leg."""
    clone = _materialize_clone(tmp_path, "config-repo-no-agents-md")
    cfg = Config(config_repo=str(clone))
    with pytest.raises(ConfigLegError) as excinfo:
        load_agent_config_source(cfg)
    message = str(excinfo.value)
    assert "AGENTS.md" in message
    assert "culture.yaml" not in message
    assert ".qwen/skills" not in message
    assert cfg.agent_config is None


def test_a_missing_skills_leg_refuses_with_the_skills_leg_message(tmp_path):
    """AC1, leg 3: no .qwen/skills directory at all."""
    clone = _materialize_clone(tmp_path, "config-repo-no-skills")
    cfg = Config(config_repo=str(clone))
    with pytest.raises(ConfigLegError) as excinfo:
        load_agent_config_source(cfg)
    message = str(excinfo.value)
    assert ".qwen/skills" in message
    assert "culture.yaml" not in message
    assert "AGENTS.md" not in message
    assert cfg.agent_config is None


def test_an_empty_skills_directory_refuses_like_a_missing_one(tmp_path):
    """The skills leg is the DIRECTORY existing with at least one skill: an
    empty directory is the same refusal, not a thinner one."""
    clone = _materialize_clone(tmp_path, "config-repo-complete")
    shutil.rmtree(clone / ".qwen" / "skills" / "sample")
    with pytest.raises(ConfigLegError) as excinfo:
        load_agent_config_source(Config(config_repo=str(clone)))
    assert ".qwen/skills" in str(excinfo.value)


def test_the_three_leg_refusals_are_pairwise_distinct(tmp_path):
    """AC1: three missing legs assert three DISTINCT refusal messages."""
    messages = {}
    for name in (
        "config-repo-no-culture-yaml",
        "config-repo-no-agents-md",
        "config-repo-no-skills",
    ):
        clone = _materialize_clone(tmp_path, name)
        cfg = Config(config_repo=str(clone))
        with pytest.raises(ConfigLegError) as excinfo:
            load_agent_config_source(cfg)
        messages[name] = str(excinfo.value)
    assert len(set(messages.values())) == 3


def test_a_culture_yaml_that_declares_no_agent_refuses(tmp_path):
    """The identity leg is culture.yaml WITH backend + identity: a file that
    declares no agent block is an empty identity — refused with its own
    message (distinct from the missing-file one)."""
    clone = _materialize_clone(tmp_path, "config-repo-complete")
    (clone / "culture.yaml").write_text("agents: []\n", encoding="utf-8")
    cfg = Config(config_repo=str(clone))
    with pytest.raises(ConfigLegError) as excinfo:
        load_agent_config_source(cfg)
    message = str(excinfo.value)
    assert "culture.yaml" in message
    assert "declares no agent" in message
    assert cfg.agent_config is None


def test_no_configured_repository_refuses_to_start():
    """A bridge that cannot name its identity repository has no identity:
    fail closed, with a message that names the configured-repo resolution,
    not any file leg."""
    with pytest.raises(ConfigLegError) as excinfo:
        load_agent_config_source(Config())
    message = str(excinfo.value)
    assert "configured repository" in message
    for leg in ("culture.yaml", "AGENTS.md", ".qwen/skills"):
        assert leg not in message


def test_an_ambiguous_allowlist_refuses_without_an_explicit_config_repo(tmp_path):
    a = _materialize_clone(tmp_path, "config-repo-complete", dest="repo-a")
    b = _materialize_clone(tmp_path, "config-repo-complete", dest="repo-b")
    with pytest.raises(ConfigLegError) as excinfo:
        load_agent_config_source(Config(repo_allowlist=(str(a), str(b))))
    assert "configured repository" in str(excinfo.value)


def test_a_configured_repository_that_is_not_a_directory_refuses(tmp_path):
    cfg = Config(config_repo=str(tmp_path / "no-such-dir"))
    with pytest.raises(ConfigLegError) as excinfo:
        load_agent_config_source(cfg)
    assert "not a directory" in str(excinfo.value)


def test_a_triple_without_a_git_head_refuses_on_the_revision_leg(tmp_path):
    """The three file legs pass but the revision is unmeasurable (a plain
    directory, not a clone): the capability surface must report a clone
    HEAD, so this fails closed with its own distinct message."""
    clone = _materialize_clone(tmp_path, "config-repo-complete", git=False)
    with pytest.raises(ConfigLegError) as excinfo:
        load_agent_config_source(Config(config_repo=str(clone)))
    message = str(excinfo.value)
    assert "revision" in message
    for leg in ("culture.yaml", "AGENTS.md", ".qwen/skills"):
        assert leg not in message


def test_a_reload_after_a_new_commit_reports_the_new_head(tmp_path):
    """The revision is MEASURED at each startup load, never cached: a fresh
    commit in the clone is the revision the next boot reports."""
    clone = _materialize_clone(tmp_path, "config-repo-complete")
    first = load_agent_config_source(Config(config_repo=str(clone)))
    (clone / "AGENTS.md").write_text("# AGENTS\nsecond boot\n", encoding="utf-8")
    env = {**os.environ, **_FIXTURE_GIT_ENV}
    subprocess.run(["git", "add", "-A"], cwd=clone, env=env, check=True, capture_output=True)
    subprocess.run(
        ["git", "commit", "-q", "-m", "config-repo fixture second"],
        cwd=clone,
        env=env,
        check=True,
        capture_output=True,
    )
    second = load_agent_config_source(Config(config_repo=str(clone)))
    assert second.revision != first.revision
    head = subprocess.run(
        ["git", "rev-parse", "HEAD"], cwd=clone, check=True, capture_output=True, text=True
    ).stdout.strip()
    assert second.revision == head


def test_config_repo_from_env():
    cfg = Config.load(env={"QWEN_BRIDGE_CONFIG_REPO": "/srv/identity"})
    assert cfg.config_repo == "/srv/identity"
    assert cfg.agent_config is None  # the knob alone does not load; the startup load does


def test_config_repo_file_baseline_with_env_override(tmp_path):
    path = tmp_path / "bridge.json"
    path.write_text(json.dumps({"config_repo": "/srv/from-file"}))
    cfg = Config.load(str(path), env={"QWEN_BRIDGE_CONFIG_REPO": "/srv/from-env"})
    assert cfg.config_repo == "/srv/from-env"
    assert cfg.agent_config is None


def test_a_leg_refusal_is_a_boot_time_config_error():
    """ConfigLegError IS a ConfigError: the ported entrypoint catches it at
    boot (print + exit 2, before dialin/serve), so a refused leg serves no
    invoke. Plan task t5 keeps that order when it wires the startup load."""
    assert issubclass(ConfigLegError, ConfigError)
