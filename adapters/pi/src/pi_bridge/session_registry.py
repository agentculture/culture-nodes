"""Per-session-key concurrency guard (task t6, spec claims c44/h37).

Mirrors `adapters/claude-code/src/claude_code_bridge/session_registry.py`
field for field — the concurrency mechanism is backend-agnostic, only the
logger name differs. See that module's own docstring for the full
fork-vs-queue argument this bridge makes the same call on: FORK the second
concurrent same-`session_key` invocation cold (discard its
`continuation_ref`) rather than queue it, because this reference bridge's
HTTP server is deliberately single-threaded (see `server.py`) and honest
queueing would require decoupling request-accept from subprocess-execution
for the synchronous path, which it does not do. The fork is always
observable — a logged warning, an in-memory `ForkEvent`, and (per
`server.py`) an `X-Session-Fork` response header — never merely inferred
from an unexpectedly cold `continuation_ref`.
"""

from __future__ import annotations

import logging
import threading
from dataclasses import dataclass
from datetime import datetime, timezone

logger = logging.getLogger("pi_bridge.session_registry")


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
