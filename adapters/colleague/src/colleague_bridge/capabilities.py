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

import os
import pwd
import sys
from typing import Any, Callable, Sequence

from colleague_bridge import deployment, preflight
from colleague_bridge.config import Config


def _unix_user() -> str:
    """The OS account this bridge process runs as.

    Prefixed onto the confinement sentence (task t2, issue #243) so the
    capability surface names which account a dispatch really runs as — the
    fact that decides what a session can reach once agents run as dedicated
    OS users rather than inside a shared sandbox. stdlib only: `pwd` keeps
    this adapter's zero-runtime-dependency promise intact.
    """
    return pwd.getpwuid(os.getuid()).pw_name


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
    f"unix-user:{_unix_user()}: "
    "none: `colleague work` runs with this bridge process's own privileges and takes no "
    "sandbox flag. colleague isolates each work item in a throwaway git worktree of the "
    "dispatched repo, which bounds where changes land but not what the session can reach; "
    "the other boundary is the repo allowlist below, enforced by this bridge before dispatch "
    "rather than by the kernel during it"
)

#: What the one mode grants (issue #96): everything this bridge process
#: itself has. Stated in the shared vocabulary rather than left implied by
#: `_CONFINEMENT` above, because the toolchain facts below are read against
#: it — a tool needing a writable cache or network egress gets both here,
#: which is precisely what a codex dispatch under `--sandbox read-only` does
#: not.
_MODE_GRANTS = {preflight.MODE_UNSANDBOXED: preflight.GRANTS}

#: The toolchains this bridge reports on: the CLI it drives, plus the three
#: the dispatched probe runs on thor and orin tested (issue #96). Same list
#: as the other bridges', so two hosts' surfaces are comparable tool for
#: tool.
TOOLCHAINS = (
    preflight.Toolchain("colleague"),
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
    is the same on all four bridges. Nothing colleague dispatches depends on
    unprivileged user namespaces (there is no bubblewrap helper in this
    backend's path at all), so no mode here is ever reported unavailable
    because of them — see `codex_bridge.capabilities` for the backend where
    that measurement is the load-bearing one.

    *locate* and *version* are injectable so a test can assert what this
    surface says about a snap-packaged toolchain and a standalone one alike,
    neither of which is the host running pytest.

    *git_probe* is the write attempt behind `git_metadata_writable` (issue
    #94), injectable for the same reason: the state worth asserting is a
    checkout whose `.git` REFUSES a write, and that is not the state of the
    machine running pytest. The default is the real attempt, and on THIS
    backend the process making it has exactly a dispatched session's
    authority: `colleague work` takes no sandbox flag and runs with this
    bridge's own privileges (`_CONFINEMENT`). The worktree colleague isolates
    each work item in is a linked worktree, whose `.git` is a FILE pointing at
    the parent repo's metadata directory — which `preflight.git_metadata_dir`
    follows, so the answer is about the directory a ref would really land in.
    """
    available, unavailable = preflight.measure_sandbox_modes(
        SANDBOX_MODE_CANDIDATES,
        probes=probes,
    )
    grants = preflight.dispatch_grants({mode: _MODE_GRANTS[mode] for mode in available})
    writable_paths = list(cfg.repo_allowlist + cfg.repo_allowlist_prefixes)
    return preflight.host_block(
        hostname=preflight.hostname(),
        sandbox_modes=available,
        sandbox_modes_unavailable=unavailable,
        default_sandbox_mode=preflight.MODE_UNSANDBOXED,
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
            extra=_publication_clause(cfg),
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
            sys.modules[__package__], "culture-nodes-colleague-bridge"
        ),
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
