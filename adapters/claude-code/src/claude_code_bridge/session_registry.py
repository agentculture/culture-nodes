"""Per-session-key concurrency guard (task t6, spec claims c44/h37).

Two work items dispatched concurrently with the same `session_key` target
what the engine considers ONE resumable conversation (the same
actor+repo+workstream, per ADR 0010 §4) — running both against the
provider's `--resume` handle at the same time would be two threads writing
into a single conversation, which is exactly the interleaving task t5's
stickiness work was built to avoid, not merely a performance concern.

Two strategies close that hole, and task t6 names both explicitly:

* **QUEUE** the second invocation behind the first: preserves the
  continuation ref exactly (the second turn still resumes, just later), but
  the wait is unbounded — and this bridge's HTTP server is DELIBERATELY
  single-threaded (see `server.py`'s own docstring). A synchronous dispatch
  already blocks the entire accept loop for the duration of one claude call,
  for every session key, not just its own; the only way a same-key second
  request is even accepted while the first is still running is via the
  async path's background thread. Queueing the actual provider call
  honestly would mean decoupling "accept the HTTP request" from "spawn the
  subprocess" for the synchronous path too — an architecture change this
  reference bridge does not make today.
* **FORK** the second invocation cold: ignore the `continuation_ref` it
  arrived with and dispatch it as a brand-new provider session. This spends
  a session the engine's budget accounting did not plan for (c46's
  "workstream sessions" undercounts a forked collision), but it never blocks
  the bridge, needs no architecture change, and is trivially correct against
  the one property that actually matters: two different provider sessions
  cannot interleave turns on one provider thread, because they are not the
  same thread.

This bridge picks **FORK**. Queueing only pays for itself once this
reference adapter also decouples request-accept from subprocess-execution
for the synchronous path, which it deliberately does not do. But forking
silently would trade one invisible race for one invisible budget leak
(budget.go's own argument, echoed in spec claim c45, applies here too): a
fork must be OBSERVABLE, never merely inferred after the fact from an
unexpectedly-fresh `continuation_ref`. `SessionRegistry.acquire()` therefore
logs a warning and records a `ForkEvent` any caller (test or operator) can
inspect directly off the registry; `server.py` also sets an
`X-Session-Fork` response header on a forked invocation's response.

`Config.max_inflight_per_session_key` (default 1) is how many holders may
occupy one session_key's slot before a further concurrent arrival forks;
`Config.session_concurrency_enabled` is a kill-switch back to t5's
unserialized behaviour, for an operator who needs to rule this mechanism out
while diagnosing something else.
"""

from __future__ import annotations

import logging
import threading
from dataclasses import dataclass
from datetime import datetime, timezone

logger = logging.getLogger("claude_code_bridge.session_registry")


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


@dataclass(frozen=True)
class ForkEvent:
    """One recorded fork decision — the observable record t6 requires
    ("record which happened, so the behavior is observable rather than
    inferred") instead of leaving a concurrent collision to be inferred
    from an unexpectedly-fresh `continuation_ref` after the fact."""

    session_key: str
    at: str
    owner_holders: tuple[str, ...]
    forked_holder: str


class SessionRegistry:
    """Tracks how many invocations currently hold the provider-execution
    slot for each `session_key`, and forks (never blocks) whoever arrives
    once every slot is taken. See module docstring for the fork-vs-queue
    argument."""

    def __init__(self, max_inflight: int = 1) -> None:
        self._lock = threading.Lock()
        self._holders: dict[str, list[str]] = {}
        self._max_inflight = max(1, int(max_inflight))
        #: Append-only record of every fork decision this registry instance
        #: has made — inspectable directly by tests/operators, never only
        #: inferable from behaviour.
        self.fork_events: list[ForkEvent] = []

    def acquire(self, session_key: str | None, holder: str) -> bool:
        """Attempt to claim one of `session_key`'s inflight slots for
        *holder*.

        Returns True iff *holder* becomes one of the current holders (may
        dispatch using its `continuation_ref` as given). Returns False iff
        every slot is already occupied (the caller must fork: dispatch
        cold, `continuation_ref=None`) — a `ForkEvent` is recorded and a
        warning logged before returning.

        `session_key is None` (or empty) always returns True without
        touching any state — no session key means nothing to serialize
        against, matching pre-t6 behaviour exactly.
        """
        if not session_key:
            return True
        with self._lock:
            current = self._holders.setdefault(session_key, [])
            if len(current) >= self._max_inflight:
                event = ForkEvent(
                    session_key=session_key,
                    at=_now_iso(),
                    owner_holders=tuple(current),
                    forked_holder=holder,
                )
                self.fork_events.append(event)
                logger.warning(
                    "session_key %r already has %d in-flight invocation(s) "
                    "(holders=%s); forking invocation %s cold instead of "
                    "resuming its continuation_ref (t6, spec c44/h37)",
                    session_key,
                    len(current),
                    list(current),
                    holder,
                )
                return False
            current.append(holder)
            return True

    def release(self, session_key: str | None, holder: str) -> None:
        """Release *holder*'s slot on `session_key`, if it holds one.

        Safe to call unconditionally (e.g. from a `finally` block) even
        when `acquire` was never called for this holder, or already
        returned False for it (a forked holder never occupied a slot, so
        this is a harmless no-op for it).
        """
        if not session_key:
            return
        with self._lock:
            current = self._holders.get(session_key)
            if not current:
                return
            if holder in current:
                current.remove(holder)
            if not current:
                del self._holders[session_key]

    def record_lost_resume(self, session_key: str, holder: str) -> None:
        """Record a provider-ref loss before the caller retries cold."""
        with self._lock:
            event = ForkEvent(
                session_key=session_key,
                at=_now_iso(),
                owner_holders=(),
                forked_holder=holder,
            )
            self.fork_events.append(event)
            logger.warning(
                "provider session for session_key %r was lost; forking invocation %s "
                "cold with a question/answer re-brief (t12, c30/h15)",
                session_key,
                holder,
            )
