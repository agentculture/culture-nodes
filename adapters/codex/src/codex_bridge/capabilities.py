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

from typing import Any, Sequence

from codex_bridge import preflight
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


def host_facts(
    cfg: Config,
    *,
    probes: Sequence[tuple[str, str]] = preflight.USERNS_SYSCTLS,
) -> dict[str, Any]:
    """Measure this host and return the `host` block for its capability
    surface. *probes* is injectable so a test can assert both kinds of
    kernel rather than whichever one is running the suite.
    """
    available, unavailable = preflight.measure_sandbox_modes(
        SANDBOX_MODE_CANDIDATES,
        requires_userns=_REQUIRES_USERNS,
        probes=probes,
    )
    return preflight.host_block(
        hostname=preflight.hostname(),
        sandbox_modes=available,
        sandbox_modes_unavailable=unavailable,
        default_sandbox_mode=cfg.default_sandbox,
        confinement=_confinement(unavailable),
        commit_policy=preflight.harvest_commit_policy(
            preserve_on_failure=cfg.preserve_on_failure,
            branch_prefix=cfg.preserve_branch_prefix,
            push=cfg.preserve_push,
            remote=cfg.preserve_remote,
        ),
        writable_paths=list(cfg.repo_allowlist),
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
