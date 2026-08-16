"""Bridge configuration: env vars, with an optional small JSON file underneath.

Mirrors `adapters/colleague/src/colleague_bridge/config.py` field for field
where the concern is generic (repo allowlist, HTTP surface, async timing,
state dir); the fields that differ are claude-specific: `claude_bin` (instead
of `colleague_bin`), `permission_mode` (claude has no engine-side autonomy
flag the way colleague's engine does — headless dispatch must pick a
`--permission-mode` explicitly, since there is no TTY to answer a permission
prompt), and `min_claude_version` (the version gate — see `claude_cli.py`).

Precedence: JSON config file (if present) sets the baseline; environment
variables (`CLAUDE_CODE_BRIDGE_*`) override individual fields on top of it.
Passing neither is a valid, if useless, configuration — every field has a
documented default — except the repo allowlist, which is empty by default
(an unconfigured bridge accepts no repo, the safe failure mode).
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from pathlib import Path

#: Env var naming the JSON config file to load (optional).
ENV_CONFIG_FILE = "CLAUDE_CODE_BRIDGE_CONFIG"

#: The `CLAUDE_CODE_BRIDGE_*` env vars this module recognises, and the config
#: field each overrides. Kept as one table so `Config.load`'s precedence rule
#: ("env wins over file") is visibly total, not ad hoc per field.
_ENV_STRING_FIELDS = {
    "CLAUDE_CODE_BRIDGE_CLAUDE_BIN": "claude_bin",
    "CLAUDE_CODE_BRIDGE_STATE_DIR": "state_dir",
    "CLAUDE_CODE_BRIDGE_AUTH_TOKEN": "auth_token",
    "CLAUDE_CODE_BRIDGE_HOST": "host",
    "CLAUDE_CODE_BRIDGE_DEFAULT_SUCCESS_OUTCOME": "default_success_outcome",
    "CLAUDE_CODE_BRIDGE_ACTOR_ID": "actor_id",
    "CLAUDE_CODE_BRIDGE_PERMISSION_MODE": "permission_mode",
    "CLAUDE_CODE_BRIDGE_MODEL": "model",
    "CLAUDE_CODE_BRIDGE_MIN_CLAUDE_VERSION": "min_claude_version",
    "CLAUDE_CODE_BRIDGE_PRESERVE_BRANCH_PREFIX": "preserve_branch_prefix",
    "CLAUDE_CODE_BRIDGE_PRESERVE_REMOTE": "preserve_remote",
    "CLAUDE_CODE_BRIDGE_HANDOVER_REMOTE": "handover_remote",
}
_ENV_INT_FIELDS = {
    "CLAUDE_CODE_BRIDGE_PORT": "port",
    "CLAUDE_CODE_BRIDGE_SYNC_MAX_STEPS": "sync_max_steps",
    "CLAUDE_CODE_BRIDGE_DEFAULT_MAX_STEPS": "default_max_steps",
    "CLAUDE_CODE_BRIDGE_HEARTBEAT_AFTER_SECONDS": "heartbeat_after_seconds",
    "CLAUDE_CODE_BRIDGE_CALLBACK_MAX_RETRIES": "callback_max_retries",
    "CLAUDE_CODE_BRIDGE_MAX_INFLIGHT_PER_SESSION_KEY": "max_inflight_per_session_key",
}
_ENV_FLOAT_FIELDS = {
    "CLAUDE_CODE_BRIDGE_POLL_INTERVAL_SECONDS": "poll_interval_seconds",
    "CLAUDE_CODE_BRIDGE_CALLBACK_TIMEOUT_SECONDS": "callback_timeout_seconds",
    "CLAUDE_CODE_BRIDGE_CALLBACK_RETRY_BACKOFF_SECONDS": "callback_retry_backoff_seconds",
    "CLAUDE_CODE_BRIDGE_SYNC_TIMEOUT_SECONDS": "sync_timeout_seconds",
    "CLAUDE_CODE_BRIDGE_BACKGROUND_DISPATCH_TIMEOUT_SECONDS": "background_dispatch_timeout_seconds",
    "CLAUDE_CODE_BRIDGE_ASYNC_WAIT_SECONDS": "async_wait_seconds",
    "CLAUDE_CODE_BRIDGE_WORKTREE_REAP_MIN_IDLE_SECONDS": "worktree_reap_min_idle_seconds",
}
_ENV_BOOL_FIELDS = {
    "CLAUDE_CODE_BRIDGE_ALWAYS_ASYNC": "always_async",
    "CLAUDE_CODE_BRIDGE_SESSION_CONCURRENCY_ENABLED": "session_concurrency_enabled",
    "CLAUDE_CODE_BRIDGE_PRESERVE_ON_FAILURE": "preserve_on_failure",
    "CLAUDE_CODE_BRIDGE_PRESERVE_PUSH": "preserve_push",
}

#: `CLAUDE_CODE_BRIDGE_REPO_ALLOWLIST` is a `os.pathsep`-joined list of
#: absolute repo paths; each entry is resolved (symlinks + `..` collapsed) at
#: load time so a later membership check is a plain string-equality test.
ENV_REPO_ALLOWLIST = "CLAUDE_CODE_BRIDGE_REPO_ALLOWLIST"
ENV_REPO_ALLOWLIST_PREFIXES = "CLAUDE_CODE_BRIDGE_REPO_ALLOWLIST_PREFIXES"

#: `CLAUDE_CODE_BRIDGE_REPO_IDENTITIES` declares the repository-identity map
#: as `os.pathsep`-joined `name=path` pairs (task t2, issue #125) — the same
#: separator the allowlist uses, so the two read alike in a unit file.
ENV_REPO_IDENTITIES = "CLAUDE_CODE_BRIDGE_REPO_IDENTITIES"


class ConfigError(Exception):
    """Raised for a config file/env value the bridge cannot use."""


@dataclass
class Config:
    """Resolved bridge configuration. See module docstring for precedence."""

    # --- identity of the bridge as a ledger producer -----------------
    actor_id: str = "claude-code-bridge"

    # --- repo allowlist (c15/h13: the bridge only works repos it is
    # configured for) --------------------------------------------------
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

    # --- claude dispatch -------------------------------------------
    claude_bin: str = "claude"
    #: Extra env vars merged onto the subprocess environment (e.g.
    #: ANTHROPIC_API_KEY, CLAUDE_CODE_* overrides). Operator-supplied; the
    #: bridge never invents values here on its own initiative.
    claude_env: dict[str, str] = field(default_factory=dict)
    #: `claude -p`'s `--permission-mode`. Headless dispatch has no TTY to
    #: answer a permission prompt, so this must be a mode that never blocks
    #: on one ("bypassPermissions" or "acceptEdits" — "manual"/"dontAsk"/
    #: "plan" would hang forever waiting for an approval nobody can give).
    #: `Config.load` and the server's per-invocation `input.permission_mode`
    #: both defer to this default when unset.
    permission_mode: str = "bypassPermissions"
    #: Default model alias/name forwarded as `--model`, when the invocation
    #: does not name one via `input.model`. Empty means "let claude pick its
    #: own default".
    model: str = ""

    # --- claude CLI version gate ---------------------------------------
    #: The oldest `claude` CLI version this bridge has been validated
    #: against. See `claude_cli.py`'s module docstring for the fleet
    #: baseline this was pinned from and why. A stored `"X.Y.Z"` string
    #: (not a tuple) so it round-trips through the JSON config file and env
    #: overrides the same way every other string field does.
    min_claude_version: str = "2.1.220"

    # --- sync/async dispatch policy ------------------------------------
    #: An invocation whose expected step budget (`input.max_steps`, or
    #: `default_max_steps` when absent) exceeds this threshold is dispatched
    #: asynchronously; at or under it, synchronously. `input.async`, when
    #: present, overrides the threshold decision outright.
    sync_max_steps: int = 6
    default_max_steps: int = 6
    #: When set, every invocation is dispatched asynchronously regardless of
    #: the threshold or an `input.async` override.
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
    #: Domain outcome used for a successful result when the invocation's
    #: `input.success_outcome` is absent.
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
    port: int = 8086
    #: Bearer token the bridge requires on `Authorization`. Empty means
    #: unauthenticated — legitimate only for a local/loopback deployment.
    auth_token: str | None = None

    # --- asynchronous dispatch timing -----------------------------------
    heartbeat_after_seconds: int = 20
    poll_interval_seconds: float = 0.15
    callback_timeout_seconds: float = 10.0
    callback_max_retries: int = 5
    callback_retry_backoff_seconds: float = 0.25

    # --- process bounds ----------------------------------------------------
    #: Bounds one SYNCHRONOUS `claude -p` subprocess. On expiry the bridge
    #: sends SIGTERM (never SIGKILL) and answers 408.
    sync_timeout_seconds: float = 300.0
    #: Bounds the (near-instant) parent call that spawns a detached
    #: background `claude -p` process and returns its start payload.
    background_dispatch_timeout_seconds: float = 30.0
    #: Overall ceiling the async poller waits for a detached claude run to
    #: produce a terminal result before giving up and reporting a timeout.
    async_wait_seconds: float = 3600.0

    # --- state -------------------------------------------------------------
    state_dir: str = ".claude-code-bridge-state"

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

        *config_path* wins over `CLAUDE_CODE_BRIDGE_CONFIG` when both are
        given; *env* defaults to `os.environ` and is only ever a parameter
        so tests can supply a clean map instead of mutating the real process
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
#: the raw JSON value.
_FILE_FIELDS = {
    "actor_id": str,
    "repo_allowlist": lambda v: tuple(str(x) for x in v),
    "repo_allowlist_prefixes": lambda v: tuple(str(x) for x in v),
    "repo_identities": lambda v: {str(k): str(x) for k, x in dict(v).items()},
    "claude_bin": str,
    "claude_env": lambda v: {str(k): str(x) for k, x in dict(v).items()},
    "permission_mode": str,
    "model": str,
    "min_claude_version": str,
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
    "background_dispatch_timeout_seconds": float,
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
