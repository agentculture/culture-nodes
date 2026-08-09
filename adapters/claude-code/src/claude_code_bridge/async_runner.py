"""Async invocation lifecycle: accepted -> progress/heartbeat -> terminal.

One `AsyncRunner` per bridge process owns every in-flight asynchronous
invocation, mirroring `adapters/colleague/src/colleague_bridge/
async_runner.py`'s own role for colleague. `start()` is called from the HTTP
request thread right after the 202 response is built (see server.py) and
hands the rest off to a daemon thread, so the request handler never blocks
on claude finishing.

Ordering guarantee the conformance kit checks for (PRD §13.3/§13.4): the
poller thread ALWAYS sends the `accepted` event, synchronously, as the very
first thing it does, before it ever looks at the flight feed or the
background result — so even a claude run that finishes before the poller
gets its first scheduler turn cannot produce a terminal event with no
preceding non-terminal one.

Cancellation (PRD §13.6) is where this module earns its keep beyond mirroring
colleague: `cancel()` writes a `flightfiles` control file the SAME poller
loop below checks every cycle; this is the only place a written stop request
is ever turned into an actual SIGTERM to the claude subprocess — see
`flightfiles.py`'s module docstring for why that translation has to live
here rather than in `claude_cli.py`.
"""

from __future__ import annotations

import logging
import os
import signal
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from claude_code_bridge import claude_cli, flightfiles, mapping
from claude_code_bridge.callbacks import CallbackConfig, CallbackEmitter
from claude_code_bridge.config import Config

logger = logging.getLogger("claude_code_bridge.async_runner")


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def _describe_feed_record(record: dict[str, Any]) -> str:
    note = record.get("note")
    if note:
        return str(note)
    subtype = record.get("subtype")
    if subtype:
        return str(subtype)
    return str(record.get("type") or "progress")


@dataclass
class AsyncInvocation:
    invocation_id: str
    pid: int
    ctx: mapping.InvocationContext
    started_at: float = field(default_factory=time.monotonic)
    done: bool = False
    cancel_requested: bool = False


class AsyncRunner:
    """Owns every in-flight asynchronous invocation for one bridge process."""

    def __init__(self, cfg: Config) -> None:
        self._cfg = cfg
        self._lock = threading.Lock()
        self._invocations: dict[str, AsyncInvocation] = {}

    def start(
        self,
        *,
        start: claude_cli.BackgroundStart,
        ctx: mapping.InvocationContext,
        callback_url: str,
        callback_token: str,
        heartbeat_after_seconds: int,
    ) -> None:
        inv = AsyncInvocation(invocation_id=start.handle_id, pid=start.pid, ctx=ctx)
        with self._lock:
            self._invocations[start.handle_id] = inv

        emitter = CallbackEmitter(
            callback_url,
            callback_token,
            start.handle_id,
            CallbackConfig(
                timeout_seconds=self._cfg.callback_timeout_seconds,
                max_retries=self._cfg.callback_max_retries,
                backoff_seconds=self._cfg.callback_retry_backoff_seconds,
            ),
        )
        thread = threading.Thread(
            target=self._run,
            args=(inv, emitter, heartbeat_after_seconds),
            name=f"claude-code-bridge-poll-{start.handle_id}",
            daemon=True,
        )
        thread.start()

    def cancel(self, invocation_id: str) -> bool:
        """Best-effort cooperative stop (PRD §13.6). Always "succeeds" from
        the caller's point of view: cancellation is durable in Culture Nodes
        and best-effort here, so an unknown or already-finished invocation
        id is not an error, just nothing left to cooperate with.
        """
        with self._lock:
            inv = self._invocations.get(invocation_id)
            if inv is not None:
                inv.cancel_requested = True
        if inv is None:
            return False
        try:
            flightfiles.write_stop(self._cfg.state_dir, invocation_id)
        except OSError as exc:
            logger.warning("writing cooperative stop for %s failed: %s", invocation_id, exc)
        return True

    def _run(
        self, inv: AsyncInvocation, emitter: CallbackEmitter, heartbeat_after_seconds: int
    ) -> None:
        # 1. accepted, synchronously, before anything else can race a terminal event.
        emitter.send(
            "accepted",
            {
                "invocation_id": inv.invocation_id,
                "heartbeat_after_seconds": heartbeat_after_seconds,
            },
        )

        result, detail = self._poll_until_done(inv, emitter, heartbeat_after_seconds)

        timed_out = result is None and not detail.startswith("__pid_gone__")
        ev = mapping.terminal_event(
            result,
            inv.ctx,
            default_success_outcome=self._cfg.default_success_outcome,
            actor_id=self._cfg.actor_id,
            created_at=_now_iso(),
            timed_out=timed_out,
            detail="" if timed_out else detail,
        )
        emitter.send(ev.kind, ev.payload)
        with self._lock:
            inv.done = True

    def _poll_until_done(
        self, inv: AsyncInvocation, emitter: CallbackEmitter, heartbeat_after_seconds: int
    ) -> tuple[dict[str, Any] | None, str]:
        """Tail the flight feed for progress and wait for a terminal result.

        Returns `(result_or_None, detail)`. `detail` explains a `None`
        result: either the overall wait bound was hit, or the claude process
        exited without ever producing a parseable `type: "result"` record
        (marked with the `__pid_gone__` sentinel prefix so `_run` can tell
        "we gave up waiting" from "it's simply gone" without a third return
        value) — mirrors `colleague_bridge.async_runner`'s own
        `_poll_until_done` exactly in shape.
        """
        tail = flightfiles.FeedTail(self._cfg.state_dir, inv.invocation_id)
        heartbeat_interval = max(float(heartbeat_after_seconds), 1.0)
        last_event_at = time.monotonic()
        deadline = time.monotonic() + self._cfg.async_wait_seconds
        sigterm_sent = False

        while True:
            for record in tail.read_new_records():
                if record.get("type") == "result":
                    return record, ""
                emitter.send("progress", {"note": _describe_feed_record(record), "raw": record})
                last_event_at = time.monotonic()

            if not sigterm_sent and flightfiles.stop_requested(
                self._cfg.state_dir, inv.invocation_id
            ):
                self._send_sigterm(inv.pid)
                sigterm_sent = True

            if not claude_cli.is_pid_alive(inv.pid):
                # One last drain in case of a benign write/exit race.
                for record in tail.read_new_records():
                    if record.get("type") == "result":
                        return record, ""
                return None, "__pid_gone__: claude exited without producing a parseable result"

            now = time.monotonic()
            if now >= deadline:
                return None, "the bridge's async wait bound elapsed before claude finished"
            if now - last_event_at >= heartbeat_interval:
                emitter.send("heartbeat", {})
                last_event_at = now

            time.sleep(self._cfg.poll_interval_seconds)

    @staticmethod
    def _send_sigterm(pid: int) -> None:
        """Cooperative stop, never SIGKILL — the same discipline
        `claude_cli.run_sync` uses for a synchronous timeout. Best-effort:
        the process may already be gone by the time this runs."""
        try:
            os.kill(pid, signal.SIGTERM)
        except OSError as exc:
            logger.debug(
                "SIGTERM to pid %s for cancellation failed (likely already exited): %s", pid, exc
            )
