"""Bridge configuration: env vars, with an optional small JSON file underneath.

Mirrors `colleague_bridge/config.py` field for field — same precedence rule,
same safe-by-default allowlist stance — with the colleague-specific knobs
(`open_pr`, `allow_dirty`, `colleague_bin`) swapped for pi's own
(`pi_bin`, `default_sandbox`), since the ACP seam has no PR-opening or
dirty-worktree concept to configure.

The bridge runs on the machine that hosts a `pi` install — it is a
deployment-time reference adapter, not part of the culture-nodes control
plane. Its configuration is deliberately small and readable in one place:

* a **repo path allowlist** — the bridge only dispatches into repos it was
  explicitly configured for; any other `input.repo` is refused with 403
  (never a silent `git clone` of whatever a caller names);
* the **pi binary** to spawn, and any `pi_env` passthrough
  (e.g. `QWEN_HOME`) the operator wants every dispatch to carry;
* the **default sandbox mode** used when an invocation does not name one;
* the **sync/async dispatch threshold** (`sync_max_steps` / `always_async`)
  — a dispatch-timing decision only; pi has no `--max-steps` flag to
  forward it to (see README's "argv this bridge generates");
* the **identity repository** (issue #114) — the clone whose
  culture.yaml + AGENTS.md + .pi/skills triple the bridge loads at
  startup (`load_agent_config_source`); the image carries no agent
  identity, and a missing leg is a boot refusal, not a warning;
* a **state dir** for the idempotency store and per-invocation bookkeeping.

Precedence: JSON config file (if present) sets the baseline; environment
variables (`PI_BRIDGE_*`) override individual fields on top of it, so an
operator can ship one config file per repo checkout and still override a
single knob (e.g. the port) from the process supervisor without editing the
file. Passing neither is a valid, if useless, configuration — every field
has a documented default — except the repo allowlist, which is empty by
default (an unconfigured bridge accepts no repo, the safe failure mode).
"""

from __future__ import annotations

import json
import os
import subprocess
from dataclasses import dataclass, field
from pathlib import Path

#: Env var naming the JSON config file to load (optional).
ENV_CONFIG_FILE = "PI_BRIDGE_CONFIG"

#: The `PI_BRIDGE_*` env vars this module recognises, and the config
#: field each overrides. Kept as one table so `Config.load`'s precedence
#: rule ("env wins over file") is visibly total, not ad hoc per field.
_ENV_STRING_FIELDS = {
    "PI_BRIDGE_PI_BIN": "pi_bin",
    "PI_BRIDGE_PROVIDER": "provider",
    "PI_BRIDGE_MODEL": "model",
    "PI_BRIDGE_MODEL_ENDPOINT": "model_endpoint",
    "PI_BRIDGE_DEFAULT_SANDBOX": "default_sandbox",
    "PI_BRIDGE_STATE_DIR": "state_dir",
    # A field-name mapping table entry, not a credential.
    "PI_BRIDGE_AUTH_TOKEN": "auth_token",  # nosec B105
    "PI_BRIDGE_HOST": "host",
    "PI_BRIDGE_DEFAULT_SUCCESS_OUTCOME": "default_success_outcome",
    "PI_BRIDGE_ACTOR_ID": "actor_id",
    "PI_BRIDGE_PRESERVE_BRANCH_PREFIX": "preserve_branch_prefix",
    "PI_BRIDGE_PRESERVE_REMOTE": "preserve_remote",
    "PI_BRIDGE_HANDOVER_REMOTE": "handover_remote",
    "PI_BRIDGE_CONFIG_REPO": "config_repo",
}
_ENV_INT_FIELDS = {
    "PI_BRIDGE_PORT": "port",
    "PI_BRIDGE_SYNC_MAX_STEPS": "sync_max_steps",
    "PI_BRIDGE_DEFAULT_MAX_STEPS": "default_max_steps",
    "PI_BRIDGE_HEARTBEAT_AFTER_SECONDS": "heartbeat_after_seconds",
    "PI_BRIDGE_CALLBACK_MAX_RETRIES": "callback_max_retries",
    "PI_BRIDGE_MAX_INFLIGHT_PER_SESSION_KEY": "max_inflight_per_session_key",
}
_ENV_FLOAT_FIELDS = {
    "PI_BRIDGE_POLL_INTERVAL_SECONDS": "poll_interval_seconds",
    "PI_BRIDGE_CALLBACK_TIMEOUT_SECONDS": "callback_timeout_seconds",
    "PI_BRIDGE_CALLBACK_RETRY_BACKOFF_SECONDS": "callback_retry_backoff_seconds",
    "PI_BRIDGE_SYNC_TIMEOUT_SECONDS": "sync_timeout_seconds",
    "PI_BRIDGE_ASYNC_WAIT_SECONDS": "async_wait_seconds",
    "PI_BRIDGE_WORKTREE_REAP_MIN_IDLE_SECONDS": "worktree_reap_min_idle_seconds",
}
_ENV_BOOL_FIELDS = {
    "PI_BRIDGE_ALWAYS_ASYNC": "always_async",
    "PI_BRIDGE_SESSION_CONCURRENCY_ENABLED": "session_concurrency_enabled",
    "PI_BRIDGE_PRESERVE_ON_FAILURE": "preserve_on_failure",
    "PI_BRIDGE_PRESERVE_PUSH": "preserve_push",
}

#: `PI_BRIDGE_REPO_ALLOWLIST` is a `os.pathsep`-joined list of absolute
#: repo paths, matching how PATH-shaped env vars are conventionally written;
#: each entry is resolved (symlinks + `..` collapsed) at load time so a
#: later membership check is a plain string-equality test, never a fresh
#: filesystem walk per request.
ENV_REPO_ALLOWLIST = "PI_BRIDGE_REPO_ALLOWLIST"
ENV_REPO_ALLOWLIST_PREFIXES = "PI_BRIDGE_REPO_ALLOWLIST_PREFIXES"

#: `PI_BRIDGE_REPO_IDENTITIES` declares the repository-identity map as
#: `os.pathsep`-joined `name=path` pairs (task t2, issue #125) — the same
#: separator the allowlist uses, so the two read alike in a unit file.
ENV_REPO_IDENTITIES = "PI_BRIDGE_REPO_IDENTITIES"

#: `PI_BRIDGE_CONFIG_REPO` names the repository this bridge's IDENTITY is
#: loaded from (issue #114, plan task t4): the clone whose
#: culture.yaml + AGENTS.md + .pi/skills triple the startup load consumes.
#: The image carries no agent identity — the repository IS the config. When
#: unset, the load falls back to the allowlist holding exactly one entry
#: (`only_allowed_repo`): the image deployment clones exactly one
#: repository, and that one is both what the identity loads from and what
#: the bridge may dispatch into. Set it explicitly when the two differ
#: (e.g. the allowlist scopes worktrees under a prefix while the identity
#: loads from the main clone).
ENV_CONFIG_REPO = "PI_BRIDGE_CONFIG_REPO"

# --- the #114 identity triple (issue #114, plan task t4) ------------------
#
# The leg names below are what the per-leg refusals quote, so the three
# file legs stay grep-distinct from each other and from the support legs
# (configured-repo resolution, revision). What a pi ACP session reads at
# run time is exactly these paths: AGENTS.md is the measured pi context
# file for a project, and .pi/skills is the project's skill set — the
# bridge verifies their presence at startup and reports which revision it
# verified them in; it never feeds the contents to pi itself.

CULTURE_YAML_NAME = "culture.yaml"
PROMPT_FILE_NAME = "AGENTS.md"
SKILLS_DIR_NAME = ".pi/skills"
SKILL_MANIFEST_NAME = "SKILL.md"

#: How long a `git` probe here gets. Same rule as `deployment.py`'s: a boot
#: measurement must never be the reason startup hangs.
_GIT_PROBE_TIMEOUT_SECONDS = 5.0


class ConfigError(Exception):
    """Raised for a config file/env value the bridge cannot use."""


class ConfigLegError(ConfigError):
    """A leg of the #114 identity triple cannot be loaded (issue #114).

    Distinct from a malformed bridge config (a plain `ConfigError`): these
    are the per-leg REFUSALS the startup load raises when the configured
    repository fails closed. Each message names the leg it refuses on —
    the operator fixing one leg must hear that leg's name, not another
    leg's — and every leg has its own message, so a refusal is never a
    shared string two causes could both produce. Being a `ConfigError` is
    load-bearing: the entrypoint catches `ConfigError` at boot (print +
    exit 2) before dialin/serve, so a refused leg serves no invoke.
    """


@dataclass(frozen=True)
class AgentIdentity:
    """The first agent block of a culture.yaml — the identity leg.

    `suffix` is WHO the agent is (its nick); `backend` is WHAT it declares
    it runs on; `model` the model it names, "" when it names none. Parsed
    with the dependency-free scalar scan `nodes whoami` uses (see
    `culture_nodes/cli/_commands/whoami.py`'s `read_agent_fields`), so the
    bridge and the CLI answer the same question the same way. The loader
    does not FILTER on these values — the pi bridge serving an identity
    that declares another backend is a spec-leg decision (spec scope s9),
    not a config-leg refusal.
    """

    suffix: str
    backend: str
    model: str = ""


@dataclass(frozen=True)
class AgentConfigSource:
    """The #114 identity triple loaded from the configured repository.

    The image carries no agent identity: this record IS the identity the
    bridge loaded at startup — the repository, the revision of that
    repository (the clone HEAD), and the three legs the load verified. The
    capability surface reports `repo` + `revision` as the loaded config
    repo + revision (plan tasks t3/t5 wire this record, read off
    `Config.agent_config`, into the capability document); the leg paths are
    what a pi ACP session then reads on its own, which is why only the
    paths and the skill COUNT are recorded here, never the contents — what
    the skills (or the prompt file) contain is pi's business, not the
    config load's.
    """

    #: Resolved (symlink-collapsed) path of the configured repository.
    repo: str
    #: The clone HEAD: the full 40-char lowercase commit sha the triple was
    #: loaded from, measured at startup (never cached).
    revision: str
    #: The culture.yaml identity leg: the first agent block.
    agent: AgentIdentity
    #: `<repo>/culture.yaml` — the path the identity leg was read from.
    culture_path: str
    #: `<repo>/AGENTS.md` — the pi prompt-file leg.
    prompt_file: str
    #: `<repo>/.pi/skills` — the skills leg.
    skills_dir: str
    #: How many skills (subdirectories holding a SKILL.md) the skills leg
    #: holds. Always >= 1: an empty or missing directory is a refusal.
    skill_count: int


@dataclass
class Config:
    """Resolved bridge configuration. See module docstring for precedence."""

    # --- identity of the bridge as a ledger producer -----------------
    actor_id: str = "pi-bridge"

    # --- repo allowlist (the bridge only works repos it is configured
    # for) --------------------------------------------------------------
    repo_allowlist: tuple[str, ...] = ()
    repo_allowlist_prefixes: tuple[str, ...] = ()
    #: Repository IDENTITY -> local checkout, for the identities the actors
    #: served by this bridge are registered under (see `repositories.py`).
    #: It says WHICH repository a name means, never that the bridge may
    #: touch it: `repo_allowed` is still the last word, so a declaration
    #: pointing outside the allowlist is refused. Empty is the ordinary
    #: case — an identity whose repository segment matches an allowlisted
    #: checkout's directory name resolves with no declaration at all.
    repo_identities: dict[str, str] = field(default_factory=dict)

    # --- the #114 identity triple (issue #114, plan task t4) ------------
    #: The repository this bridge's IDENTITY is loaded from: the clone whose
    #: culture.yaml + AGENTS.md + .pi/skills triple `load_agent_config_source`
    #: consumes at startup. The image carries no agent identity — the
    #: repository IS the config. When None, the load falls back to
    #: `only_allowed_repo()`; when that is None too, the load refuses,
    #: because a bridge that cannot name its identity repository has no
    #: identity. NOT re-checked against the allowlist: reading the identity
    #: repository is a boot-time identity load, not a dispatch, and a
    #: prefix-scoped allowlist would refuse the very main clone it is
    #: scoped from.
    config_repo: str | None = None
    #: The #114 identity triple loaded at startup, set by
    #: `load_agent_config_source`. None until that load has run and
    #: succeeded: the capability layer receives this very Config object,
    #: and `agent_config.repo` + `agent_config.revision` is what it reports
    #: as the loaded config repo + revision (plan tasks t3/t5). A None here
    #: where the bridge is serving would mean the startup load never ran —
    #: a boot refusal, not a servable state.
    agent_config: AgentConfigSource | None = None

    # --- pi dispatch --------------------------------------------------
    pi_bin: str = "pi"
    #: Extra env vars merged onto the subprocess environment (e.g.
    #: QWEN_HOME to point at a specific auth profile). Operator-supplied;
    #: the bridge never invents values here on its own initiative.
    pi_env: dict[str, str] = field(default_factory=dict)
    provider: str = ""
    model: str = ""
    #: The OpenAI-style base URL of the served model, e.g.
    #: ``http://thor:8000/v1``. The bridge does not call it (pi reads its
    #: provider from ~/.pi/agent/models.json); it is preflight's health
    #: target and the harness-comparison metadata the actor registers with.
    model_endpoint: str = ""
    #: The sandbox value used when `input.sandbox` is absent.
    #: One of pi_cli.SANDBOX_MODES.
    default_sandbox: str = "workspace-write"

    # --- sync/async dispatch policy ------------------------------------
    #: A request whose expected step budget exceeds this threshold is
    #: dispatched asynchronously; at or under it, synchronously. An
    #: invocation's `input.async` (bool), when present, overrides the
    #: threshold decision outright. NOT forwarded to pi (no native
    #: step-budget flag) — dispatch-timing signal only.
    sync_max_steps: int = 6
    #: The step budget assumed for threshold comparison when
    #: `input.max_steps` is absent.
    default_max_steps: int = 6
    #: When set, every invocation is dispatched asynchronously regardless
    #: of the threshold or an `input.async` override — the "always-async"
    #: escape hatch the task names.
    always_async: bool = False

    # --- session-key concurrency (t6, c44/h37) --------------------------
    #: How many invocations may hold one `input.session_key`'s in-flight
    #: slot at once before a further concurrent arrival forks (dispatches
    #: cold, ignoring the `continuation_ref` it carried). See
    #: `session_registry.py`'s module docstring for the fork-vs-queue
    #: argument. 1 means "exactly one in-flight invocation per session
    #: key" — the acceptance criterion's own phrasing.
    max_inflight_per_session_key: int = 1
    #: Kill-switch back to t5's unserialized behaviour (every invocation
    #: dispatches with its `continuation_ref` as given, session_key
    #: collisions included) — for an operator who needs to rule this
    #: mechanism out while diagnosing something else.
    session_concurrency_enabled: bool = True

    # --- outcome vocabulary ---------------------------------------------
    #: Domain outcome used for a `status: ok` TaskResult when the
    #: invocation's `input.success_outcome` is absent.
    default_success_outcome: str = "completed"

    # --- preserve-on-failure (task t25, issue #49) ----------------------
    #: Commit-on-failure toggle: when a node's dispatch ends in a genuine
    #: technical failure (never a domain outcome), the bridge preserves the
    #: workspace's changes on a freshly minted branch via git plumbing (see
    #: `preserve.py`'s module docstring). Off means "never attempt it" —
    #: e.g. for a bridge host where preservation is deliberately unwanted.
    preserve_on_failure: bool = True
    #: Prefix for the code-minted preserve branch name.
    preserve_branch_prefix: str = "preserve/"
    #: Push-or-local: when True (the default), a preserve commit is pushed
    #: best-effort to `preserve_remote`; when the push fails or this is
    #: False, the commit stays local-only — an ordinary recorded outcome
    #: (task t25's own risk register: bridge-host push credentials for
    #: thor/orin are unverified), never an error.
    preserve_push: bool = True
    #: The remote a preserve branch is pushed to, when `preserve_push` is
    #: True.
    preserve_remote: str = "origin"

    # --- handover ref (task t9/t10, issue #90, #13) ----------------------
    #: The remote whose configured URL a handover ref's handle is built
    #: from (`preserve.handover_ref`). It is READ ONLY — `git remote
    #: get-url` — because a handover deliberately does not push; the name
    #: is separate from `preserve_remote` because the two answer different
    #: questions ("where a preserve branch is pushed to" versus "which
    #: remote another host would fetch this ref from"), and a host that
    #: pushes preserve branches to a scratch remote must still be able to
    #: name the shared one in a handle.
    handover_remote: str = "origin"

    # --- worktree reaping (task t17) -------------------------------------
    #: How long a minted worktree must have gone untouched before age stops
    #: being a reason to DEFER its removal. Read by `reap.ReapPolicy`; see
    #: `reap.py`'s docstring for why age is the weakest of the four idleness
    #: signals and never on its own a reason to reap.
    worktree_reap_min_idle_seconds: float = 86_400.0

    # --- HTTP surface ----------------------------------------------------
    host: str = "127.0.0.1"
    #: Different default than colleague-bridge's 8085 so both can run on
    #: one host without colliding.
    port: int = 8086
    #: Bearer token the bridge requires on `Authorization`. Empty means
    #: unauthenticated — legitimate only for a local/loopback deployment
    #: (mirrors internal/actors.Endpoint's own docstring).
    auth_token: str | None = None

    # --- asynchronous dispatch timing -----------------------------------
    heartbeat_after_seconds: int = 20
    poll_interval_seconds: float = 0.15
    callback_timeout_seconds: float = 10.0
    callback_max_retries: int = 5
    callback_retry_backoff_seconds: float = 0.25

    # --- process bounds ----------------------------------------------------
    #: Bounds one SYNCHRONOUS `pi --acp` subprocess. On expiry the bridge
    #: sends SIGTERM (never SIGKILL) and answers a timeout. pi responds
    #: to SIGTERM by exiting promptly WITHOUT a terminal session/prompt
    #: response — see pi_cli.py's module docstring for the
    #: grounding — which is exactly why this bridge never trusts
    #: exit code alone.
    sync_timeout_seconds: float = 300.0
    #: Overall ceiling the async runner waits for a pi subprocess to
    #: finish before SIGTERM + reporting a timeout failure. Generous by
    #: default — background work is expected to run long.
    async_wait_seconds: float = 3600.0

    # --- state -------------------------------------------------------------
    state_dir: str = ".pi-bridge-state"

    @property
    def state_path(self) -> Path:
        return Path(self.state_dir)

    def repo_allowed(self, repo: str) -> bool:
        """True for an exact entry or a strict child of a scoped prefix."""
        try:
            resolved = str(Path(repo).expanduser().resolve())
        except OSError:
            return False
        if resolved in self.repo_allowlist:
            return True
        candidate = Path(resolved)
        return any(
            candidate != Path(root) and candidate.is_relative_to(root)
            for root in self.repo_allowlist_prefixes
        )

    def only_allowed_repo(self) -> str | None:
        """The one repo this bridge can work in, when there is exactly one.

        A trigger-created run's input IS the event payload (task t17b), so a
        deployment-neutral workflow has nowhere to put a checkout path: a
        literal in the graph would make it deployment-specific, and the
        emitter that raises the event is a pure emitter that knows nothing
        about checkouts.

        This is the LAST of the three answers to that gap, not the first.
        `repositories.py` resolves the actor's registered repository identity
        first (task t2, issue #125), and this inference only runs for an actor
        registered without one — which is every deployment that worked before
        the identity existed, and must keep working.

        When the allowlist names exactly one repository and no prefixes, the
        caller restating it adds no safety: this bridge physically cannot work
        anywhere else, and the allowlist check would reject anything else
        anyway. Ambiguity fails closed — two entries, or any prefix rule, and
        `input.repo` stays required, because then the choice is real and
        guessing it would silently pick a workspace the caller did not name.
        That fail-closed shape is the one `repositories.py` mirrors; what
        changed in t2 is that a multi-entry allowlist is no longer a dead end,
        because cardinality stopped being how the repository is chosen.
        """
        if len(self.repo_allowlist) == 1 and not self.repo_allowlist_prefixes:
            return self.repo_allowlist[0]
        return None

    @classmethod
    def load(cls, config_path: str | None = None, env: dict[str, str] | None = None) -> "Config":
        """Build a Config from an optional JSON file, then env overrides.

        *config_path* wins over `PI_BRIDGE_CONFIG` when both are given
        (an explicit caller argument is the most specific source); *env*
        defaults to `os.environ` and is only ever a parameter so tests can
        supply a clean map instead of mutating the real process
        environment.
        """
        env = os.environ if env is None else env
        data: dict = {}
        path = config_path or env.get(ENV_CONFIG_FILE)
        if path:
            data = _read_config_file(path)

        cfg = cls(**_coerce_file_fields(data))
        _apply_env_overrides(cfg, env)
        _normalize_allowlist(cfg)
        return cfg


# --- the #114 identity triple load (issue #114, plan task t4) -------------


def load_agent_config_source(cfg: Config) -> AgentConfigSource:
    """Load the #114 identity triple from the configured repository.

    Called ONCE at startup, before the bridge serves anything: the
    entrypoint catches `ConfigError` at boot (print + exit 2) before
    dialin/serve, and `ConfigLegError` is one, so a missing leg is a boot
    refusal that serves no invoke — never a warning the bridge starts up
    behind, and never a fallback to an invented identity.

    The configured repository is `cfg.config_repo` when the operator named
    it (`PI_BRIDGE_CONFIG_REPO` / the config file's `config_repo`), else
    the allowlist's single entry (`only_allowed_repo`): the image
    deployment clones exactly one repository, and that one is the identity
    source. Ambiguity — no entry, several entries, or a prefix rule —
    refuses, because guessing which repository an agent's identity comes
    from is the failure this issue exists to close.

    The legs are verified in the order a missing one is most likely to be
    noticed: the identity (culture.yaml), the pi prompt file (AGENTS.md),
    the skills (.pi/skills), and only then the revision (the clone HEAD).
    Each refusal names its own leg and no other (see `ConfigLegError`), so
    the operator fixing one leg hears that leg's name.

    On success the loaded record is stored on `cfg.agent_config` — the very
    Config object the capability layer receives — and returned; that is the
    field plan tasks t3/t5 consume for the capability document's loaded
    config repo + revision.
    """
    repo = _configured_repo(cfg)
    agent, culture_path = _load_culture_leg(repo)
    prompt_file = _load_prompt_leg(repo)
    skills_dir, skill_count = _load_skills_leg(repo)
    revision = _load_revision(repo)
    source = AgentConfigSource(
        repo=str(repo),
        revision=revision,
        agent=agent,
        culture_path=str(culture_path),
        prompt_file=str(prompt_file),
        skills_dir=str(skills_dir),
        skill_count=skill_count,
    )
    cfg.agent_config = source
    return source


def _configured_repo(cfg: Config) -> Path:
    """The one repository the identity loads from, or a named refusal."""
    raw = cfg.config_repo
    if raw is None:
        raw = cfg.only_allowed_repo()
    if raw is None:
        raise ConfigLegError(
            "configured repository: this bridge cannot name the one repository its "
            f"identity loads from (issue #114) — set {ENV_CONFIG_REPO}, or allowlist "
            "exactly one repository without prefix rules; the image carries no agent "
            "identity, so the bridge refuses to start"
        )
    repo = Path(raw).expanduser().resolve()
    if not repo.is_dir():
        raise ConfigLegError(
            f"configured repository: {repo} is not a directory — the #114 identity "
            "triple loads from a repository checkout; the bridge refuses to start"
        )
    return repo


def _load_culture_leg(repo: Path) -> tuple[AgentIdentity, Path]:
    """The identity leg: culture.yaml present, readable, declaring an agent."""
    path = repo / CULTURE_YAML_NAME
    if not path.is_file():
        raise ConfigLegError(
            f"{CULTURE_YAML_NAME}: the configured repository {repo} has no "
            f"{CULTURE_YAML_NAME} — the identity leg of the #114 config triple is "
            "missing; the image carries no agent identity, so the bridge refuses to "
            "start"
        )
    return _read_agent_identity(path), path


def _read_agent_identity(path: Path) -> AgentIdentity:
    """The first agent block of culture.yaml, parsed without a YAML
    dependency.

    The same scalar scan `nodes whoami` runs (see
    `culture_nodes/cli/_commands/whoami.py`'s `read_agent_fields`): the
    top-level fields of the FIRST `- suffix:` entry. A culture.yaml that
    declares no agent block, or whose first block names no suffix or no
    backend, is an EMPTY identity leg — a refusal, never a fallback to an
    invented nick.
    """
    try:
        text = path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError) as exc:
        raise ConfigLegError(
            f"{CULTURE_YAML_NAME}: {path} could not be read as UTF-8 text ({exc}) — the "
            "identity leg of the #114 config triple is unusable; the bridge refuses to "
            "start"
        ) from exc
    seen_agent = False
    suffix = backend = model = ""
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith(("- suffix:", "suffix:")):
            if seen_agent:  # a second agent block: the identity is the first
                break
            seen_agent = True
            suffix = _culture_scalar(stripped, "suffix")
        elif seen_agent and stripped.startswith("backend:"):
            backend = _culture_scalar(stripped, "backend")
        elif seen_agent and stripped.startswith("model:"):
            model = _culture_scalar(stripped, "model")
    if not (seen_agent and suffix and backend):
        raise ConfigLegError(
            f"{CULTURE_YAML_NAME}: the configured repository's {CULTURE_YAML_NAME} "
            "declares no agent identity (a first agent block with suffix + backend) — "
            "the identity leg of the #114 config triple is empty; the image carries no "
            "agent identity, so the bridge refuses to start"
        )
    return AgentIdentity(suffix=suffix, backend=backend, model=model)


def _load_prompt_leg(repo: Path) -> Path:
    """The pi prompt-file leg: AGENTS.md present."""
    path = repo / PROMPT_FILE_NAME
    if not path.is_file():
        raise ConfigLegError(
            f"{PROMPT_FILE_NAME}: the configured repository {repo} has no "
            f"{PROMPT_FILE_NAME} — the pi prompt-file leg of the #114 config triple "
            "is missing; the bridge refuses to start"
        )
    return path


def _load_skills_leg(repo: Path) -> tuple[Path, int]:
    """The skills leg: a .pi/skills directory holding at least one skill
    (a subdirectory with a SKILL.md). An empty or missing directory is the
    same refusal — what the skills CONTAIN is out of scope; their presence
    is the leg."""
    path = repo / SKILLS_DIR_NAME
    skill_count = 0
    if path.is_dir():
        skill_count = sum(
            1
            for entry in path.iterdir()
            if entry.is_dir() and (entry / SKILL_MANIFEST_NAME).is_file()
        )
    if skill_count == 0:
        raise ConfigLegError(
            f"{SKILLS_DIR_NAME}: the configured repository {repo} carries no usable "
            f"skills leg — the #114 config triple requires a {SKILLS_DIR_NAME} "
            f"directory holding at least one skill (a subdirectory with a "
            f"{SKILL_MANIFEST_NAME}); an empty or missing directory is a refusal, so "
            "the bridge refuses to start"
        )
    return path, skill_count


def _load_revision(repo: Path) -> str:
    """The clone HEAD: which revision of the configured repository the
    triple was loaded from. The capability surface reports it (issue
    #114), and a revision nobody can measure is a refusal — never an
    invented one."""
    try:
        proc = subprocess.run(  # noqa: S603,S607 # nosec B603,B607 - fixed binary, no shell
            # (the argv is the constant list below); B607 is deliberate: `git` resolves from
            # PATH, as every other git call in this project does.
            ["git", "-C", str(repo), "rev-parse", "HEAD"],
            capture_output=True,
            text=True,
            check=False,
            timeout=_GIT_PROBE_TIMEOUT_SECONDS,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        raise ConfigLegError(
            f"revision: git could not be run in the configured repository {repo} ({exc}) "
            "— the clone revision the capability surface must report is unmeasurable; "
            "the bridge refuses to start"
        ) from exc
    if proc.returncode != 0:
        raise ConfigLegError(
            f"revision: the configured repository {repo} has no resolvable git HEAD "
            "(unborn branch or not a work tree) — the clone revision the capability "
            "surface must report is unmeasurable; the bridge refuses to start"
        )
    sha = _full_commit_sha(proc.stdout.strip())
    if not sha:
        raise ConfigLegError(
            f"revision: git answered {proc.stdout.strip()!r} for the configured "
            f"repository {repo}'s HEAD, which is not a full commit id — the clone "
            "revision the capability surface must report is unmeasurable; the bridge "
            "refuses to start"
        )
    return sha


def _culture_scalar(line: str, key: str) -> str:
    """The scalar after `key:` on a culture.yaml line, quotes stripped.

    Mirrors `nodes whoami`'s own parser (the CLI and the bridge must answer
    the identity question the same way); empty values come back as "" —
    the caller decides that a missing identity is a refusal.
    """
    _, _, value = line.partition(f"{key}:")
    return value.strip().strip("'\"")


def _full_commit_sha(value: str) -> str:
    """*value* if it is an unambiguous 40-character lowercase hex commit id,
    else "".

    The same refusal `deployment.py`'s `_full_commit_sha` makes, for the
    same reason: `HEAD`, a branch name and an abbreviation each mean
    something different tomorrow, and a record nobody can resolve later is
    not a record.
    """
    if len(value) != 40:
        return ""
    return value if all(c in "0123456789abcdef" for c in value) else ""


def _read_config_file(path: str) -> dict:
    try:
        raw = Path(path).read_text(encoding="utf-8")
    except OSError as exc:
        raise ConfigError(f"cannot read bridge config file {path!r}: {exc}") from exc
    try:
        data = json.loads(raw)
    except ValueError as exc:  # json.JSONDecodeError is a ValueError
        raise ConfigError(f"bridge config file {path!r} is not valid JSON: {exc}") from exc
    if not isinstance(data, dict):
        raise ConfigError(f"bridge config file {path!r} must contain a JSON object")
    return data


#: Field names a config file may set, each mapped to a coercion applied to
#: the raw JSON value (so a file-supplied "8086" string integer field still
#: works the same as a real JSON number would).
_FILE_FIELDS = {
    "actor_id": str,
    "repo_allowlist": lambda v: tuple(str(x) for x in v),
    "repo_allowlist_prefixes": lambda v: tuple(str(x) for x in v),
    "repo_identities": lambda v: {str(k): str(x) for k, x in dict(v).items()},
    "config_repo": str,
    "pi_bin": str,
    "pi_env": lambda v: {str(k): str(x) for k, x in dict(v).items()},
    "provider": str,
    "model": str,
    "model_endpoint": str,
    "default_sandbox": str,
    "sync_max_steps": int,
    "default_max_steps": int,
    "always_async": bool,
    "max_inflight_per_session_key": int,
    "session_concurrency_enabled": bool,
    "default_success_outcome": str,
    "preserve_on_failure": bool,
    "preserve_branch_prefix": str,
    "preserve_push": bool,
    "preserve_remote": str,
    "handover_remote": str,
    "worktree_reap_min_idle_seconds": float,
    "host": str,
    "port": int,
    "auth_token": str,
    "heartbeat_after_seconds": int,
    "poll_interval_seconds": float,
    "callback_timeout_seconds": float,
    "callback_max_retries": int,
    "callback_retry_backoff_seconds": float,
    "sync_timeout_seconds": float,
    "async_wait_seconds": float,
    "state_dir": str,
}


def _coerce_file_fields(data: dict) -> dict:
    out = {}
    for key, value in data.items():
        if key not in _FILE_FIELDS:
            raise ConfigError(f"unknown bridge config key: {key!r}")
        try:
            out[key] = _FILE_FIELDS[key](value)
        except (TypeError, ValueError) as exc:
            raise ConfigError(f"bridge config key {key!r} has an invalid value: {exc}") from exc
    return out


def _apply_env_overrides(cfg: Config, env: dict[str, str]) -> None:
    for name, field_name in _ENV_STRING_FIELDS.items():
        if name in env:
            setattr(cfg, field_name, env[name])
    for name, field_name in _ENV_INT_FIELDS.items():
        if name in env:
            setattr(cfg, field_name, _parse_int(name, env[name]))
    for name, field_name in _ENV_FLOAT_FIELDS.items():
        if name in env:
            setattr(cfg, field_name, _parse_float(name, env[name]))
    for name, field_name in _ENV_BOOL_FIELDS.items():
        if name in env:
            setattr(cfg, field_name, _parse_bool(name, env[name]))
    if ENV_REPO_ALLOWLIST in env:
        raw = env[ENV_REPO_ALLOWLIST]
        cfg.repo_allowlist = tuple(p for p in raw.split(os.pathsep) if p.strip())
    if ENV_REPO_ALLOWLIST_PREFIXES in env:
        raw = env[ENV_REPO_ALLOWLIST_PREFIXES]
        cfg.repo_allowlist_prefixes = tuple(p for p in raw.split(os.pathsep) if p.strip())
    if ENV_REPO_IDENTITIES in env:
        cfg.repo_identities = _parse_identities(env[ENV_REPO_IDENTITIES])


def _parse_identities(raw: str) -> dict[str, str]:
    """Parse `name=path` pairs joined by `os.pathsep` into an identity map.

    A pair missing its `=` is a ConfigError rather than a silently dropped
    entry: a bridge that came up holding half its identity map would refuse
    dispatches with a naming error nobody would connect back to a typo here.
    """
    identities: dict[str, str] = {}
    for pair in raw.split(os.pathsep):
        entry = pair.strip()
        if not entry:
            continue
        name, sep, path = entry.partition("=")
        if not sep or not name.strip() or not path.strip():
            raise ConfigError(f"{ENV_REPO_IDENTITIES} entry {entry!r} is not a 'name=path' pair")
        identities[name.strip()] = path.strip()
    return identities


def _normalize_allowlist(cfg: Config) -> None:
    resolved = []
    for entry in cfg.repo_allowlist:
        try:
            resolved.append(str(Path(entry).expanduser().resolve()))
        except OSError as exc:
            raise ConfigError(
                f"repo allowlist entry {entry!r} could not be resolved: {exc}"
            ) from exc
    cfg.repo_allowlist = tuple(resolved)
    prefixes: list[str] = []
    for entry in cfg.repo_allowlist_prefixes:
        try:
            prefixes.append(str(Path(entry).expanduser().resolve()))
        except OSError as exc:
            raise ConfigError(f"cannot resolve repo allowlist prefix {entry!r}: {exc}") from exc
    cfg.repo_allowlist_prefixes = tuple(prefixes)


def _parse_int(name: str, raw: str) -> int:
    try:
        return int(raw)
    except ValueError as exc:
        raise ConfigError(f"{name}={raw!r} is not an integer") from exc


def _parse_float(name: str, raw: str) -> float:
    try:
        return float(raw)
    except ValueError as exc:
        raise ConfigError(f"{name}={raw!r} is not a number") from exc


def _parse_bool(name: str, raw: str) -> bool:
    lowered = raw.strip().lower()
    if lowered in ("1", "true", "yes", "on"):
        return True
    if lowered in ("0", "false", "no", "off", ""):
        return False
    raise ConfigError(f"{name}={raw!r} is not a boolean (1/0, true/false, yes/no, on/off)")
