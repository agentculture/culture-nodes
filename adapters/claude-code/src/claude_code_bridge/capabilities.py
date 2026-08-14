"""What THIS bridge knows about the host it dispatches on (issue #67, task
t15) — the backend-specific half of the preflight capability surface.

Every bridge has one of these and it is the only per-backend code in this
feature: the protocol, the measurement helpers and the agreed key set live
once, in `preflight.py`, byte-identical across all four adapters. What
differs here is only what genuinely differs between backends — for
claude-code, that `claude -p` has no sandbox flag at all, so its confinement
story is "none" and its mode vocabulary is `--permission-mode`'s.

Read `preflight.py`'s docstring first; it explains the split and why the
facts must describe what the host CAN DO rather than what the config asks
for.
"""

from __future__ import annotations

from typing import Any, Sequence

from claude_code_bridge import preflight
from claude_code_bridge.config import Config

#: The `claude -p --permission-mode` values a dispatch may name (the server
#: forwards `input.permission_mode` verbatim), least to most permissive.
#:
#: This is what stands in for a sandbox-mode list on this backend, because
#: it is the only per-dispatch knob that changes what a session may do
#: without asking. It is NOT confinement — see `_CONFINEMENT`.
SANDBOX_MODE_CANDIDATES = ("plan", "default", "acceptEdits", "bypassPermissions")

#: The modes headless dispatch cannot deliver, whatever the host is. Not a
#: measurement: this bridge spawns `claude -p` with no TTY (`claude_cli.
#: run_sync`/`spawn`), so a mode that can stop and wait for an approval
#: hangs forever waiting for one nobody can give — the reason `config.py`'s
#: `permission_mode` field is documented as needing a never-blocking value.
_NO_TTY = (
    "headless dispatch allocates no TTY, so this mode blocks forever on an approval prompt "
    "nobody can answer"
)
_UNSUPPORTED = {"plan": _NO_TTY, "default": _NO_TTY}

#: `claude -p` takes no sandbox flag and this bridge passes none: the
#: session runs as the bridge process does. Stated plainly because a reader
#: who sees `sandbox_modes: [..., "bypassPermissions"]` and stops there can
#: mistake a prompting policy for a confinement boundary.
_CONFINEMENT = (
    "none: `claude -p` runs with this bridge process's own privileges and takes no sandbox "
    "flag — --permission-mode decides whether the session asks before acting, never what it "
    "is able to reach. The only boundary here is the repo allowlist below, enforced by this "
    "bridge before dispatch rather than by the kernel during it"
)


def host_facts(
    cfg: Config,
    *,
    probes: Sequence[tuple[str, str]] = preflight.USERNS_SYSCTLS,
) -> dict[str, Any]:
    """Measure this host and return the `host` block for its capability
    surface.

    *probes* is accepted, and passed through, for one reason: the signature
    is the same on all four bridges. Nothing claude-code dispatches depends
    on unprivileged user namespaces (there is no bubblewrap helper in this
    backend's path at all), so no mode here is ever reported unavailable
    because of them — see `codex_bridge.capabilities` for the backend where
    that measurement is the load-bearing one.
    """
    available, unavailable = preflight.measure_sandbox_modes(
        SANDBOX_MODE_CANDIDATES,
        unsupported=_UNSUPPORTED,
        probes=probes,
    )
    return preflight.host_block(
        hostname=preflight.hostname(),
        sandbox_modes=available,
        sandbox_modes_unavailable=unavailable,
        default_sandbox_mode=cfg.permission_mode,
        confinement=_CONFINEMENT,
        commit_policy=preflight.harvest_commit_policy(
            preserve_on_failure=cfg.preserve_on_failure,
            branch_prefix=cfg.preserve_branch_prefix,
            push=cfg.preserve_push,
            remote=cfg.preserve_remote,
        ),
        writable_paths=list(cfg.repo_allowlist),
    )
