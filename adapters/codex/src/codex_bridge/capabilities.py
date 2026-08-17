"""What THIS bridge knows about the host it dispatches on (issue #67, task
t15) — the backend-specific half of the preflight capability surface.

Every bridge has one of these and it is the only per-backend code in this
feature: the protocol, the measurement helpers and the agreed key set live
once, in `preflight.py`, byte-identical across all four adapters. What
differs here is only what genuinely differs between backends — codex's
`--sandbox` vocabulary and the bubblewrap helper its confinement rests on.

Read `preflight.py`'s docstring first; it explains the split and why the
facts must describe what the host CAN DO rather than what the config asks
for.
"""

from __future__ import annotations

import sys
from typing import Any, Callable, Sequence

from codex_bridge import deployment, preflight
from codex_bridge.config import Config

#: The `codex exec --sandbox` values a dispatch may name, in increasing
#: order of what they permit. Ordered (where `codex_cli.SANDBOX_MODES` is an
#: unordered set) because this is the order a reader of the briefing sees.
#: Deliberately a second declaration rather than a derivation of that set: a
#: mode added there needs a decision about whether its confinement depends on
#: unprivileged user namespaces, and deriving this tuple would advertise the
#: new mode as available without anyone making that decision. The test
#: `test_the_candidate_modes_are_exactly_the_ones_this_bridge_can_pass`
#: compares the two, so the decision is demanded rather than skipped.
SANDBOX_MODE_CANDIDATES = ("read-only", "workspace-write", "danger-full-access")

#: The two modes whose confinement codex implements with a bubblewrap helper
#: — which needs unprivileged user namespaces, and therefore is the pair
#: that silently degrades where the kernel restricts them (#18/#63).
#: `danger-full-access` confines nothing, so it needs nothing and works
#: everywhere.
_REQUIRES_USERNS = ("read-only", "workspace-write")

#: What each `--sandbox` mode actually GRANTS a dispatched session (issue
#: #96). Not a preference and not read off the config: each entry is what a
#: dispatched probe run measured on thor or orin.
#:
#: * `read-only` grants NOTHING. Run 01M0356BK8QYR3119R8VY1YY9Q found neither
#:   /tmp nor the working directory writable, and run 01M039NZ2TZYFG68YZT93A6DC7
#:   could reach neither api.github.com nor pypi.org.
#: * `workspace-write` grants the working directory and the temporary
#:   directory, and still no egress: codex's writable roots are cwd + TMPDIR,
#:   and its network stays off unless a deployment turns it on.
#: * `danger-full-access` confines nothing by definition, so it grants
#:   everything — including `nested-confinement`, which is why a snap-packaged
#:   binary works there and nowhere else on this backend.
#:
#: $HOME is deliberately absent from `workspace-write`: a tool that
#: initialises a cache under ~/.cache fails there exactly as it does under
#: read-only, which is the whole of run 01M0342X60F3NY8MH150G48AZ6.
_MODE_GRANTS = {
    "read-only": (),
    "workspace-write": (preflight.GRANT_WORKSPACE_WRITE, preflight.GRANT_TMP_WRITE),
    "danger-full-access": preflight.GRANTS,
}

#: Why a missing grant stops a tool, in this backend's own words, each
#: citing the dispatched run that measured it. `measure_toolchains` puts
#: these in the per-mode reason so a briefing says why, not only what.
_GRANT_ABSENCE_REASONS = {
    preflight.GRANT_NESTED_CONFINEMENT: (
        "codex confines this mode with a bubblewrap helper, and a snap-packaged binary's own "
        "snap-confine cannot start inside it — 'required permitted capability cap_dac_override "
        "not found' (thor, run 01M03374VAKH0KHN0GDZ466NP4)"
    ),
    preflight.GRANT_HOME_WRITE: (
        "nothing under $HOME is writable in this mode, so a tool that initialises a cache there "
        "fails before it does any work — 'Could not create temporary file ... Read-only file "
        "system (os error 30) at path /home/orin/.cache/uv' (orin, run "
        "01M0342X60F3NY8MH150G48AZ6). Redirecting the cache only helps where the mode grants "
        "some other writable root: under read-only it grants none, not even /tmp (orin, run "
        "01M0356BK8QYR3119R8VY1YY9Q)"
    ),
    preflight.GRANT_NETWORK_EGRESS: (
        "a dispatched session has no network egress in this mode, even where the same host's "
        "login shell does: `gh auth status` over ssh on thor reports logged in while a dispatch "
        "there could reach neither api.github.com nor pypi.org (run 01M039NZ2TZYFG68YZT93A6DC7)"
    ),
    preflight.GRANT_WORKSPACE_WRITE: (
        "the working directory is not writable in this mode — 'touch: cannot touch ...: Read-only "
        "file system' (orin, run 01M0356BK8QYR3119R8VY1YY9Q)"
    ),
    # nosec B108 - this is prose ABOUT /tmp, reporting a measured sandbox
    # limitation. Nothing here opens, creates, or names a temp file; bandit is
    # pattern-matching the literal inside a diagnostic message.
    preflight.GRANT_TMP_WRITE: (  # nosec B108
        "/tmp is not writable in this mode either, so a tool cannot fall back to it (orin, run "
        "01M0356BK8QYR3119R8VY1YY9Q)"
    ),
}

#: The toolchains this bridge reports on: the CLI it drives, plus the three
#: the probe runs actually tested. Adding a fourth means DECLARING what it
#: needs — which is the point. A tool listed with no requirements is a claim
#: that it runs in every mode this host offers, so `git` is absent here
#: rather than listed as unconditionally fine: reading a repo and pushing to
#: one need very different grants.
#:
#: `codex` itself requires nothing because it is what CREATES the sandbox —
#: it runs as this bridge process does. Its version is here to be watched:
#: the probe findings below are pinned to codex-cli's behaviour, so a bump
#: changes the recorded baseline and re-opens them (`scripts/
#: toolchain-baseline.sh check`).
TOOLCHAINS = (
    preflight.Toolchain("codex"),
    preflight.Toolchain("uv", requires=(preflight.GRANT_HOME_WRITE,)),
    preflight.Toolchain("go", requires=(preflight.GRANT_HOME_WRITE,)),
    preflight.Toolchain("gh", requires=(preflight.GRANT_NETWORK_EGRESS,)),
)


# --- git metadata writability (issue #94) ----------------------------------
#
# Why this bridge reports `git_metadata_writable: not-probed` rather than what
# its own process can do.
#
# The measurement rule the shared module states: a bridge that confines its
# sessions more tightly than the process building this surface must not report
# the process's answer. codex is that bridge. This process is inside no
# sandbox, so it writes under `.git` freely — while a `workspace-write`
# session on the same host cannot, because codex carves `.git` out of its
# writable roots (`codex_cli.git_writable_override`, and commit df7d974 at
# refs/culture-nodes/probe on thor, which measured both halves). Reporting
# `supported` from here would state, in the one field a consumer reads to
# decide whether a handover ref can be created, the opposite of what a
# dispatch gets.
#
# So the attempt is not made, and that is said out loud rather than guessed at
# from a mode name in either direction. `not-probed` and NOT
# `unsupported-by-sandbox`, because this bridge measured nothing: two of
# codex's three modes carve `.git` out and `danger-full-access` does not, and
# a dispatch can opt in per-run (`writable_git`) — a single scalar claiming
# one answer for all of them would be the same class of invention.
#
# Making the attempt needs a non-billable way to run one command under `codex
# exec`'s own sandbox. codex-cli 0.147.0 offers none this bridge can rely on:
# `codex debug` has only models/app-server/prompt-input, and `codex sandbox`
# refuses without a `[permissions]` table no deployed host configures. When
# one exists, `host_facts(git_probe=...)` is where it goes — nothing else
# changes.


def host_facts(
    cfg: Config,
    *,
    probes: Sequence[tuple[str, str]] = preflight.USERNS_SYSCTLS,
    capability_probe: Callable[[], tuple[str, str]] | None = None,
    locate: Callable[[str], tuple[str | None, bool]] = preflight.locate_toolchain,
    version: Callable[[str], str | None] = preflight.toolchain_version,
    git_probe: Callable[[Any], bool] | None = None,
) -> dict[str, Any]:
    """Measure this host and return the `host` block for its capability
    surface.

    Every measurement input is injectable so a test can assert both kinds of
    kernel and both kinds of host, rather than whichever one is running the
    suite: *capability_probe* is the executable bwrap/unshare probe that
    DECIDES sandbox availability, *probes* is the sysctl set read only to
    EXPLAIN a probe that failed, and *locate*/*version* are how a toolchain
    is found and asked its version — thor's snap-packaged uv and orin's
    standalone one are both test cases here, and neither host is the one
    running pytest.

    *git_probe* is the write attempt behind `git_metadata_writable` (issue
    #94), and its default here is `None` — the one backend of the four where
    that is the honest default; the section comment above explains why. It is
    a parameter rather than a hard-coded value precisely so a caller that CAN
    attempt the write with a dispatched session's authority — the session
    itself, running `python3 -m codex_bridge.preflight --git-metadata <repo>`
    — measures it with the same function every other bridge measures it with.
    """
    available, unavailable = preflight.measure_sandbox_modes(
        SANDBOX_MODE_CANDIDATES,
        requires_userns=_REQUIRES_USERNS,
        probes=probes,
        capability_probe=capability_probe,
    )
    # Only the modes this host can ACTUALLY deliver get a grants entry: a
    # mode the kernel already ruled out must not be reported as a place a
    # toolchain works.
    grants = preflight.dispatch_grants({mode: _MODE_GRANTS[mode] for mode in available})
    writable_paths = list(cfg.repo_allowlist + cfg.repo_allowlist_prefixes)
    return preflight.host_block(
        hostname=preflight.hostname(),
        sandbox_modes=available,
        sandbox_modes_unavailable=unavailable,
        default_sandbox_mode=cfg.default_sandbox,
        confinement=_confinement(unavailable),
        dispatch_grants=grants,
        toolchains=preflight.measure_toolchains(
            TOOLCHAINS,
            grants=grants,
            grant_absence_reasons=_GRANT_ABSENCE_REASONS,
            locate=locate,
            version=version,
        ),
        commit_policy=preflight.harvest_commit_policy(
            preserve_on_failure=cfg.preserve_on_failure,
            branch_prefix=cfg.preserve_branch_prefix,
            push=cfg.preserve_push,
            remote=cfg.preserve_remote,
        ),
        writable_paths=writable_paths,
        # `writable_paths` above is the fact issue #94 says gets misread, and
        # this is the qualifier it needs. See the section comment above.
        git_metadata_writable=preflight.measure_git_metadata_writable(
            writable_paths, probe=git_probe
        ),
        artifact_publish="unsupported-by-host",
        # Which revision of THIS bridge is answering (task t32, issue #120
        # item 4). Measured from the module object rather than from a
        # configured path, so it describes the code that is actually running.
        # The distribution name is the one per-backend value: it is what
        # `uv tool install` recorded, and it is how the PEP 610 metadata that
        # decides editable-vs-copy is looked up.
        deployment=deployment.deployment_facts(
            sys.modules[__package__], "culture-nodes-codex-bridge"
        ),
    )


def _confinement(unavailable: dict[str, str]) -> str:
    """One sentence on what actually confines a codex session here.

    Stated separately from the mode list because a reader who sees only
    `sandbox_modes: ["danger-full-access"]` can miss that the reason the
    other two are gone is that this host confines nothing at all.
    """
    if unavailable:
        return (
            "nothing is confined on this host: codex enforces --sandbox with a bubblewrap "
            "helper backed by unprivileged user namespaces, which this kernel restricts, so "
            "only the mode that asks for no confinement is honest here"
        )
    return (
        "codex enforces --sandbox with a bubblewrap helper backed by unprivileged user "
        "namespaces, which this kernel permits; danger-full-access confines nothing by "
        "definition"
    )
