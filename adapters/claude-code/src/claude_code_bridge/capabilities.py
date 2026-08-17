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

import sys
from typing import Any, Callable, Sequence

from claude_code_bridge import deployment, preflight
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

#: What each mode grants (issue #96). Every mode grants everything, and that
#: is not a shortcut — it is the same fact `_CONFINEMENT` states, in the
#: shared vocabulary: a `claude -p` session runs with this bridge process's
#: privileges, so it writes where that process can write, reaches the network
#: that process reaches, and can start a helper that sets up its own
#: confinement. A permission mode decides whether the session ASKS, never
#: what it CAN.
#:
#: This is the contrast that makes the key worth having. The same toolchain
#: list measured on the codex bridge comes back unusable in two of three
#: modes; here it comes back usable — which is exactly why plan t5 routes
#: Python-side verification to a claude bridge on spark rather than to an
#: agent host under a codex sandbox.
_MODE_GRANTS = dict.fromkeys(SANDBOX_MODE_CANDIDATES, preflight.GRANTS)

#: The toolchains this bridge reports on: the CLI it drives, plus the three
#: the dispatched probe runs on thor and orin tested (issue #96). The list is
#: deliberately the same as the codex bridge's, minus its CLI and plus this
#: one, so the two surfaces are comparable tool for tool.
TOOLCHAINS = (
    preflight.Toolchain("claude"),
    preflight.Toolchain("uv", requires=(preflight.GRANT_HOME_WRITE,)),
    preflight.Toolchain("go", requires=(preflight.GRANT_HOME_WRITE,)),
    preflight.Toolchain("gh", requires=(preflight.GRANT_NETWORK_EGRESS,)),
)


def host_facts(
    cfg: Config,
    *,
    probes: Sequence[tuple[str, str]] = preflight.USERNS_SYSCTLS,
    locate: Callable[[str], tuple[str | None, bool]] = preflight.locate_toolchain,
    version: Callable[[str], str | None] = preflight.toolchain_version,
    git_probe: Callable[[Any], bool] | None = preflight.probe_git_metadata_write,
) -> dict[str, Any]:
    """Measure this host and return the `host` block for its capability
    surface.

    *probes* is accepted, and passed through, for one reason: the signature
    is the same on all four bridges. Nothing claude-code dispatches depends
    on unprivileged user namespaces (there is no bubblewrap helper in this
    backend's path at all), so no mode here is ever reported unavailable
    because of them — see `codex_bridge.capabilities` for the backend where
    that measurement is the load-bearing one.

    *locate* and *version* are injectable for the same reason: a test asserts
    what this surface says about a host that has a snap-packaged uv and about
    one that has a standalone binary, neither of which is the host running
    pytest.

    *git_probe* is the write attempt behind `git_metadata_writable` (issue
    #94), injectable so a test can assert both a checkout whose `.git` accepts
    a write and one whose `.git` refuses it — the second is the state that
    matters and it is not the state of the machine running pytest. The default
    is the real attempt, and on THIS backend the process making it has exactly
    a dispatched session's authority: `claude -p` takes no sandbox flag and
    runs with this bridge's own privileges (`_CONFINEMENT`), so what this
    process may write under `.git` is what a session may write under it.
    """
    available, unavailable = preflight.measure_sandbox_modes(
        SANDBOX_MODE_CANDIDATES,
        unsupported=_UNSUPPORTED,
        probes=probes,
    )
    grants = preflight.dispatch_grants({mode: _MODE_GRANTS[mode] for mode in available})
    writable_paths = list(cfg.repo_allowlist + cfg.repo_allowlist_prefixes)
    return preflight.host_block(
        hostname=preflight.hostname(),
        sandbox_modes=available,
        sandbox_modes_unavailable=unavailable,
        default_sandbox_mode=cfg.permission_mode,
        confinement=_CONFINEMENT,
        dispatch_grants=grants,
        toolchains=preflight.measure_toolchains(
            TOOLCHAINS, grants=grants, locate=locate, version=version
        ),
        commit_policy=preflight.harvest_commit_policy(
            preserve_on_failure=cfg.preserve_on_failure,
            branch_prefix=cfg.preserve_branch_prefix,
            push=cfg.preserve_push,
            remote=cfg.preserve_remote,
        ),
        writable_paths=writable_paths,
        # The fact `writable_paths` above cannot state (issue #94): a reader
        # who sees a checkout listed there concludes a ref can be created in
        # it, and on a backend that carves `.git` out of its writable roots
        # that is false. MEASURED by attempting the write, never derived from
        # a mode name.
        git_metadata_writable=preflight.measure_git_metadata_writable(
            writable_paths, probe=git_probe
        ),
        artifact_publish="unsupported-by-host",
        # Which revision of THIS bridge is answering (task t32, issue #120
        # item 4). Measured from the module object rather than from a
        # configured path, so it describes the code that is actually running.
        # The distribution name is the one per-backend value: it is what the
        # install recorded, and it is how the PEP 610 metadata that decides
        # editable-vs-copy is looked up.
        deployment=deployment.deployment_facts(
            sys.modules[__package__], "culture-nodes-claude-code-bridge"
        ),
    )
