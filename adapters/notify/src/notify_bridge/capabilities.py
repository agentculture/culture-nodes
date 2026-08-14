"""What THIS bridge knows about the host it dispatches on (issue #67, task
t15) — the backend-specific half of the preflight capability surface.

Every bridge has one of these and it is the only per-backend code in this
feature: the protocol, the measurement helpers and the agreed key set live
once, in `preflight.py`, byte-identical across all four adapters. What
differs here is only what genuinely differs between backends — and notify
differs the most: it opens no session, spawns no subprocess and touches no
checkout. One outbound HTTPS POST, in-process, bounded to five seconds
(`webhook.POST_TIMEOUT_SECONDS`).

That makes it the useful shape to read against the others: the sandbox keys
are OMITTED here, because a bridge that runs no session has no confinement
modes to report and guessing an empty list would be a claim rather than an
absence. The keys that remain are the ones that still mean something —
which host this runs on, that nothing is written or committed, and that the
set of paths a dispatch may write is empty.

The webhook URL is deliberately not a host fact and never will be: this
module, like every other module outside `webhook.py`, never reads it. See
`webhook.py`'s docstring for the isolation rule.
"""

from __future__ import annotations

from typing import Any, Sequence

from notify_bridge import preflight
from notify_bridge.config import Config

#: No agent session runs here, so there is nothing to confine — stated
#: rather than left to be inferred from the two absent sandbox keys.
_CONFINEMENT = (
    "no session: this bridge performs one outbound HTTPS POST in this process and starts no "
    "agent, subprocess or shell, so there is nothing to sandbox"
)

#: No workspace either, which makes the harvest/preserve vocabulary the
#: other three bridges share inapplicable rather than merely off.
_COMMIT_POLICY = (
    "no workspace: this bridge writes no files and runs no git — there is nothing to commit, "
    "preserve or harvest. Its whole effect is one message delivered to a webhook, which is not "
    "undone by anything downstream"
)


def host_facts(
    cfg: Config,
    *,
    probes: Sequence[tuple[str, str]] = preflight.USERNS_SYSCTLS,
) -> dict[str, Any]:
    """Measure this host and return the `host` block for its capability
    surface.

    *cfg* and *probes* are both accepted so the signature is the same on all
    four bridges, and neither changes the answer here: this bridge has no
    repo allowlist to report and nothing whose confinement a kernel probe
    could decide. Keeping the signature uniform is what lets a caller —
    `server._handle_capabilities`, `__main__`, a future registration
    helper — be the same code everywhere.
    """
    return preflight.host_block(
        hostname=preflight.hostname(),
        confinement=_CONFINEMENT,
        commit_policy=_COMMIT_POLICY,
        writable_paths=[],
    )
