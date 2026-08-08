"""Async invocation lifecycle: accepted -> progress/heartbeat -> terminal.

One `AsyncRunner` per bridge process owns every in-flight asynchronous
invocation. `start()` is called from the HTTP request thread right after the
202 response is built (see server.py) and hands the rest off to a daemon
thread, so the request handler never blocks on colleague finishing.

Ordering guarantee the conformance kit checks for (PRD §13.3/§13.4): the
poller thread ALWAYS sends the `accepted` event, synchronously, as the very
first thing it does, before it ever looks at the flight feed or the
background result — so even a colleague run that finishes before the poller
gets its first scheduler turn cannot produce a terminal event with no
preceding non-terminal one.
"""

from __future__ import annotations

import logging
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from colleague_bridge import colleague_cli, flightfiles, mapping
from colleague_bridge.callbacks import CallbackConfig, CallbackEmitter
from colleague_bridge.config import Config

logger = logging.getLogger("colleague_bridge.async_runner")


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def _describe_feed_record(record: dict[str, Any]) -> str:
    intent = record.get("intent")
    if intent:
        return str(intent)
    tool = record.get("tool")
    step_index = record.get("step_index")
    if tool:
        return f"step {step_index}: {tool}"
    return f"step {step_index}"


@dataclass
class AsyncInvocation:
    invocation_id: str
    repo: str
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
        start: colleague_cli.BackgroundStart,
        repo: str,
        ctx: mapping.InvocationContext,
        callback_url: str,
        callback_token: str,
        heartbeat_after_seconds: int,
    ) -> None:
        inv = AsyncInvocation(invocation_id=start.handle_id, repo=repo, pid=start.pid, ctx=ctx)
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
            name=f"colleague-bridge-poll-{start.handle_id}",
            daemon=True,
        )
        thread.start()

    def cancel(self, invocation_id: str) -> bool:
        """Best-effort cooperative stop (PRD §13.6). Always "succeeds" from
        the caller's point of view — see server.py's cancel handler for why:
        cancellation is durable in Culture Nodes and best-effort here, so an
        unknown or already-finished invocation id is not an error, just
        nothing left to cooperate with.
        """
        with self._lock:
            inv = self._invocations.get(invocation_id)
            if inv is not None:
                inv.cancel_requested = True
        if inv is None:
            return False
        try:
            flightfiles.write_stop(inv.repo, invocation_id)
        except OSError as exc:
            logger.warning("writing cooperative stop for %s failed: %s", invocation_id, exc)
        return True

    def _run(self, inv: AsyncInvocation, emitter: CallbackEmitter, heartbeat_after_seconds: int) -> None:
        # 1. accepted, synchronously, before anything else can race a terminal event.
        emitter.send(
            "accepted",
            {"invocation_id": inv.invocation_id, "heartbeat_after_seconds": heartbeat_after_seconds},
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
        """Tail the flight feed for progress and wait for a result.

        Returns `(task_result_or_None, detail)`. `detail` explains a
        `None` result: either the overall wait bound was hit, or the
        colleague process exited without ever producing a parseable
        result (marked with the `__pid_gone__` sentinel prefix so `_run`
        can tell "we gave up waiting" from "it's simply gone" without a
        third return value).
        """
        tail = flightfiles.FeedTail(inv.repo, inv.invocation_id)
        heartbeat_interval = max(float(heartbeat_after_seconds), 1.0)
        last_event_at = time.monotonic()
        deadline = time.monotonic() + self._cfg.async_wait_seconds

        while True:
            for record in tail.read_new_records():
                emitter.send("progress", {"note": _describe_feed_record(record), "raw": record})
                last_event_at = time.monotonic()

            result = colleague_cli.read_background_result(inv.repo, inv.invocation_id)
            if result is not None:
                return result, ""

            if not colleague_cli.is_pid_alive(inv.pid):
                # One last read in case of a benign write/exit race.
                result = colleague_cli.read_background_result(inv.repo, inv.invocation_id)
                if result is not None:
                    return result, ""
                return None, "__pid_gone__: colleague exited without producing a parseable result"

            now = time.monotonic()
            if now >= deadline:
                return None, "the bridge's async wait bound elapsed before colleague finished"
            if now - last_event_at >= heartbeat_interval:
                emitter.send("heartbeat", {})
                last_event_at = now

            time.sleep(self._cfg.poll_interval_seconds)
