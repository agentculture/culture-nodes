"""What THIS bridge knows about the host it dispatches on (issue #67, task
t15) — the backend-specific half of the preflight capability surface.

Every bridge has one of these and it is the only per-backend code in this
feature: the protocol, the measurement helpers and the agreed key set live
once, in `preflight.py`, byte-identical across all four adapters. What
differs here is only what genuinely differs between backends — for
colleague, that `colleague work` has no sandbox concept at all, and that a
completed work item may publish a branch of its own.

Read `preflight.py`'s docstring first; it explains the split and why the
facts must describe what the host CAN DO rather than what the config asks
for.
"""

from __future__ import annotations

from typing import Any, Sequence

from colleague_bridge import preflight
from colleague_bridge.config import Config

#: `colleague work` takes no sandbox flag and this bridge passes none
#: (`colleague_cli._common_argv`), so there is exactly one mode a dispatch
#: can get here and the shared vocabulary has a name for it. A list of one
#: is still the honest answer: the alternative — omitting the key — would
#: read as "this bridge could not measure its confinement", which is not
#: what is true.
SANDBOX_MODE_CANDIDATES = (preflight.MODE_UNSANDBOXED,)

#: What actually bounds a colleague session. The worktree is a real bound on
#: where changes LAND and deliberately not described as confinement of what
#: the session can REACH — the distinction this key exists to keep straight.
_CONFINEMENT = (
    "none: `colleague work` runs with this bridge process's own privileges and takes no "
    "sandbox flag. colleague isolates each work item in a throwaway git worktree of the "
    "dispatched repo, which bounds where changes land but not what the session can reach; "
    "the other boundary is the repo allowlist below, enforced by this bridge before dispatch "
    "rather than by the kernel during it"
)


def host_facts(
    cfg: Config,
    *,
    probes: Sequence[tuple[str, str]] = preflight.USERNS_SYSCTLS,
) -> dict[str, Any]:
    """Measure this host and return the `host` block for its capability
    surface.

    *probes* is accepted, and passed through, for one reason: the signature
    is the same on all four bridges. Nothing colleague dispatches depends on
    unprivileged user namespaces (there is no bubblewrap helper in this
    backend's path at all), so no mode here is ever reported unavailable
    because of them — see `codex_bridge.capabilities` for the backend where
    that measurement is the load-bearing one.
    """
    available, unavailable = preflight.measure_sandbox_modes(
        SANDBOX_MODE_CANDIDATES,
        probes=probes,
    )
    return preflight.host_block(
        hostname=preflight.hostname(),
        sandbox_modes=available,
        sandbox_modes_unavailable=unavailable,
        default_sandbox_mode=preflight.MODE_UNSANDBOXED,
        confinement=_CONFINEMENT,
        commit_policy=preflight.harvest_commit_policy(
            preserve_on_failure=cfg.preserve_on_failure,
            branch_prefix=cfg.preserve_branch_prefix,
            push=cfg.preserve_push,
            remote=cfg.preserve_remote,
            extra=_publication_clause(cfg),
        ),
        writable_paths=list(cfg.repo_allowlist + cfg.repo_allowlist_prefixes),
        artifact_publish="unsupported-by-host",
    )


def _publication_clause(cfg: Config) -> str | None:
    """The one clause of the commit policy that IS colleague's own.

    `open_pr` means a completed work item publishes a branch and opens a
    pull request, which makes the harvest sentence above only half the
    truth; `allow_dirty` means a dispatch does not require a clean worktree
    to start, so a session's changes can arrive mixed with someone else's.
    Both are things a dispatched task depends on and neither is visible from
    anywhere else in the briefing.
    """
    clauses = []
    if cfg.open_pr:
        clauses.append(
            "a completed work item publishes its branch and opens a pull request (open_pr), so "
            "this bridge does not only harvest"
        )
    if cfg.allow_dirty:
        clauses.append(
            "dispatch does not require a clean worktree (allow_dirty), so changes may arrive "
            "mixed with whatever was already uncommitted"
        )
    return "; ".join(clauses) if clauses else None
