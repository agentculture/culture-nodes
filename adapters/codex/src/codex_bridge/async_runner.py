"""Async invocation lifecycle: accepted -> progress/heartbeat -> terminal.

Differs architecturally from `colleague_bridge/async_runner.py` in one
respect, documented here rather than left implicit: colleague-bridge relies
on colleague's own `--background` flag to detach a child process it must
then re-discover by PID + a result file, and tails a file-based flight feed
(`colleague_bridge/flightfiles.py`) for progress — because colleague-bridge
does not itself parent that detached child. `codex exec` has no equivalent
detach flag, so this bridge instead spawns and OWNS the `codex exec`
subprocess directly, for its full lifetime, from this runner's own
background thread: progress is read straight off the child's stdout pipe as
it streams (no flight-file convention to mirror), and cancellation is a
direct SIGTERM to a subprocess this runner already holds a handle to (no
cooperative-stop control file to write). This is a simpler mechanism, not a
lesser guarantee: PRD §13.6 cancellation is still best-effort and durable
only in Culture Nodes, and SIGTERM (never SIGKILL) is still the one signal
this module ever sends — exactly matching colleague-bridge's own
cooperative-stop stance. There is deliberately no `flightfiles.py` in this
package; there is no shared file convention to speak.

One `AsyncRunner` per bridge process owns every in-flight asynchronous
invocation. `start()` is called from the HTTP request thread right after
`Popen` returns (near-instant — `Popen` never blocks on the child, so this
plays the same role as colleague-bridge's own fast `--background` parent
call without needing a real two-step dispatch) and hands the rest off to a
daemon thread, so the request handler never blocks on codex finishing.

Ordering guarantee the conformance kit checks for (PRD §13.3/§13.4): the
runner thread ALWAYS sends the `accepted` event, synchronously, as the very
first thing it does, before it ever reads a byte of the child's stdout — so
even a codex session that finishes before the runner thread gets its first
scheduler turn cannot produce a terminal event with no preceding
non-terminal one.
"""

from __future__ import annotations

import logging
import queue
import subprocess
import threading
import time
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from codex_bridge import codex_cli, mapping, preserve, workspace
from codex_bridge.callbacks import CallbackConfig, CallbackEmitter
from codex_bridge.config import Config
from codex_bridge.session_registry import SessionRegistry

logger = logging.getLogger("codex_bridge.async_runner")

#: Sentinel pushed onto a reader thread's queue once the child's stdout hits
#: EOF (the process has exited, or is about to).
_EOF = object()


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def _describe_event(event: dict[str, Any]) -> str:
    kind = event.get("type")
    if kind in ("item.completed", "item.started"):
        item = event.get("item") or {}
        item_type = item.get("type")
        if item_type:
            return f"{kind}: {item_type}"
    return str(kind)


def _try_parse(line: str) -> dict[str, Any] | None:
    import json

    try:
        event = json.loads(line)
    except ValueError:
        return None
    return event if isinstance(event, dict) else None


def _stdout_reader(stdout, q: "queue.Queue[Any]") -> None:
    """Runs in its own daemon thread: blocking-reads complete lines off the
    child's stdout pipe and pushes each onto *q*, so the runner's own loop
    below can wait on the queue with a timeout (for heartbeats/deadline)
    instead of blocking indefinitely on a raw `readline()`."""
    try:
        for line in iter(stdout.readline, ""):
            q.put(line)
    finally:
        q.put(_EOF)


@dataclass
class AsyncInvocation:
    invocation_id: str
    proc: subprocess.Popen
    ctx: mapping.InvocationContext
    #: t10: the git snapshot `start()` captured right before this
    #: invocation's codex subprocess was spawned; `_run` measures against
    #: it once the session ends.
    workspace_handle: workspace.WorkspaceHandle
    started_at: float = field(default_factory=time.monotonic)
    done: bool = False
    cancel_requested: bool = False
    #: t6 (c44/h37): the session_key slot this invocation holds, and the
    #: registry to release it from once codex's turn actually finishes.
    #: Both None when this invocation forked or session serialization
    #: found no session_key to track — nothing to release either way.
    session_registry: SessionRegistry | None = None
    session_key: str | None = None
    session_holder: str | None = None


class AsyncRunner:
    """Owns every in-flight asynchronous invocation for one bridge process."""

    def __init__(self, cfg: Config) -> None:
        self._cfg = cfg
        self._lock = threading.Lock()
        self._invocations: dict[str, AsyncInvocation] = {}

    def start(
        self,
        *,
        instruction: str,
        repo: str,
        model: str | None,
        sandbox: str | None,
        ctx: mapping.InvocationContext,
        callback_url: str,
        callback_token: str,
        heartbeat_after_seconds: int,
        continuation_ref: str | None = None,
        session_registry: SessionRegistry | None = None,
        session_key: str | None = None,
        session_holder: str | None = None,
    ) -> str:
        """Spawn `codex exec` in the background and return its invocation
        id immediately. Raises `codex_cli.SpawnError` if the subprocess
        itself could not be started (mirrors colleague-bridge's own
        `BackgroundDispatchError`, for the same 503-mapping purpose in
        `server.py`).

        t10: unlike `claude_code_bridge`/`colleague_bridge` (where
        `server.py` calls `workspace.begin()` before a two-step
        spawn-then-register dance), this bridge owns the `codex exec`
        `Popen` call directly from THIS method (see the module docstring),
        so the workspace snapshot is captured right here, immediately
        before it, instead — the same "as close as possible to the actual
        subprocess spawn" bracketing, just wired at the point this
        architecture actually spawns the child.

        *continuation_ref* (task t5): threaded straight through to
        `codex_cli.spawn` — the async path is the one long, therefore
        resume-worth-it, sessions actually take.
        """
        handle = workspace.begin(repo)
        proc = codex_cli.spawn(
            self._cfg,
            instruction,
            repo,
            model=model,
            sandbox=sandbox,
            continuation_ref=continuation_ref,
        )
        invocation_id = uuid.uuid4().hex
        inv = AsyncInvocation(
            invocation_id=invocation_id,
            proc=proc,
            ctx=ctx,
            workspace_handle=handle,
            session_registry=session_registry,
            session_key=session_key,
            session_holder=session_holder,
        )
        with self._lock:
            self._invocations[invocation_id] = inv

        emitter = CallbackEmitter(
            callback_url,
            callback_token,
            invocation_id,
            CallbackConfig(
                timeout_seconds=self._cfg.callback_timeout_seconds,
                max_retries=self._cfg.callback_max_retries,
                backoff_seconds=self._cfg.callback_retry_backoff_seconds,
            ),
        )
        thread = threading.Thread(
            target=self._run,
            args=(inv, emitter, heartbeat_after_seconds),
            name=f"codex-bridge-run-{invocation_id}",
            daemon=True,
        )
        thread.start()
        return invocation_id

    def cancel(self, invocation_id: str) -> bool:
        """Best-effort cooperative stop (PRD §13.6): SIGTERM the owned
        subprocess directly (never SIGKILL). Always "succeeds" from the
        caller's point of view — an unknown or already-finished invocation
        id is not an error, just nothing left to cooperate with (mirrors
        colleague-bridge's own `cancel()` semantics: cancellation is
        durable in Culture Nodes and best-effort at the actor)."""
        with self._lock:
            inv = self._invocations.get(invocation_id)
            if inv is not None:
                inv.cancel_requested = True
        if inv is None:
            return False
        try:
            if inv.proc.poll() is None:
                inv.proc.terminate()  # SIGTERM: cooperative, never SIGKILL
        except OSError as exc:
            logger.warning("sending SIGTERM for %s failed: %s", invocation_id, exc)
        return True

    def _run(
        self, inv: AsyncInvocation, emitter: CallbackEmitter, heartbeat_after_seconds: int
    ) -> None:
        # 1. accepted, synchronously, before anything else can race a
        # terminal event.
        emitter.send(
            "accepted",
            {
                "invocation_id": inv.invocation_id,
                "heartbeat_after_seconds": heartbeat_after_seconds,
            },
        )

        try:
            stdout_text, timed_out = self._stream_until_done(inv, emitter, heartbeat_after_seconds)
        finally:
            # t6 (c44/h37): codex's own turn is over (successfully,
            # failed, or timed out) — release the session_key slot the
            # instant the PROVIDER call itself is done, not after the
            # (possibly slow/retried) terminal callback below, so the next
            # same-key arrival stops forking as soon as it honestly can.
            if inv.session_registry is not None:
                inv.session_registry.release(inv.session_key, inv.session_holder)
        task_result = codex_cli.parse_session(stdout_text)

        # t10: measured AFTER the session ends, against the snapshot taken
        # right before it started — this is what makes head_before/after
        # and the diff bracket the actual codex subprocess's lifetime.
        measured = workspace.measure(inv.workspace_handle)

        ev = mapping.terminal_event(
            task_result,
            inv.ctx,
            default_success_outcome=self._cfg.default_success_outcome,
            actor_id=self._cfg.actor_id,
            created_at=_now_iso(),
            timed_out=timed_out,
            workspace_measured=measured,
        )
        # t25 (c26/h17, c41/h34): the async equivalent of server.py's sync
        # hook — a "failed" terminal event (never "completed", which is the
        # only other kind terminal_event ever produces) gets its workspace
        # changes preserved on a branch before the terminal callback fires.
        if ev.kind == "failed":
            preserve_result = preserve.preserve_on_failure(
                inv.workspace_handle.repo,
                measured,
                enabled=self._cfg.preserve_on_failure,
                push=self._cfg.preserve_push,
                remote=self._cfg.preserve_remote,
                branch_prefix=self._cfg.preserve_branch_prefix,
                run_id=inv.ctx.run_id,
                node_run_id=inv.ctx.node_run_id,
                attempt_id=inv.ctx.attempt_id,
                reason=str(ev.payload.get("message") or "bridge reported an asynchronous failure"),
            )
            ev.payload["preserve"] = preserve_result.to_dict()
        emitter.send(ev.kind, ev.payload)
        with self._lock:
            inv.done = True

    def _stream_until_done(
        self, inv: AsyncInvocation, emitter: CallbackEmitter, heartbeat_after_seconds: int
    ) -> tuple[str, bool]:
        """Read the child's stdout line by line as it streams (via a
        dedicated reader thread + queue, so this loop can wait with a
        timeout instead of blocking indefinitely on `readline()`), emitting
        a `progress` callback per event and a `heartbeat` when idle too
        long, until the process exits or the bridge's own
        `async_wait_seconds` deadline elapses (SIGTERM, never SIGKILL, on
        that timeout — cancellation-by-cooperative-stop, not by dying, is
        the only way this bridge ever ends a session it did not simply see
        finish on its own).

        Returns `(full_stdout_text, timed_out)`. `timed_out` is only True
        when THIS bridge's own deadline fired — a session that ends because
        it was cancelled, crashed, or exited early on its own is reported
        via `parse_session`'s ordinary "incomplete" classification instead
        (never as a `timed_out` timeout failure, and never as success).
        """
        proc = inv.proc
        heartbeat_interval = max(float(heartbeat_after_seconds), 1.0)
        deadline = time.monotonic() + self._cfg.async_wait_seconds
        last_event_at = time.monotonic()
        lines: list[str] = []

        q: "queue.Queue[Any]" = queue.Queue()
        reader = threading.Thread(target=_stdout_reader, args=(proc.stdout, q), daemon=True)
        reader.start()

        while True:
            now = time.monotonic()
            if now >= deadline:
                self._terminate_and_drain(proc, q, lines)
                return "\n".join(lines), True

            remaining_heartbeat = heartbeat_interval - (now - last_event_at)
            wait_for = max(
                0.01, min(remaining_heartbeat, deadline - now, self._cfg.poll_interval_seconds)
            )
            try:
                item = q.get(timeout=wait_for)
            except queue.Empty:
                if time.monotonic() - last_event_at >= heartbeat_interval:
                    emitter.send("heartbeat", {})
                    last_event_at = time.monotonic()
                continue

            if item is _EOF:
                break

            line = item.rstrip("\n")
            lines.append(line)
            event = _try_parse(line)
            if event is not None:
                emitter.send("progress", {"note": _describe_event(event), "raw": event})
            last_event_at = time.monotonic()

        try:
            proc.wait(timeout=10.0)
        except subprocess.TimeoutExpired:
            logger.warning(
                "codex process for %s did not exit promptly after stdout EOF", inv.invocation_id
            )
        return "\n".join(lines), False

    @staticmethod
    def _terminate_and_drain(
        proc: subprocess.Popen, q: "queue.Queue[Any]", lines: list[str]
    ) -> None:
        """The bridge's own `async_wait_seconds` deadline fired: SIGTERM
        (never SIGKILL) and collect whatever was already queued, without
        waiting further — the session is reported as a timeout, not as
        whatever partial state it happened to reach."""
        if proc.poll() is None:
            proc.terminate()
        while True:
            try:
                item = q.get_nowait()
            except queue.Empty:
                break
            if item is _EOF:
                break
            lines.append(item.rstrip("\n"))
