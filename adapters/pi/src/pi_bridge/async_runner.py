"""Async invocation lifecycle: accepted -> progress/heartbeat -> terminal.

Differs architecturally from `colleague_bridge/async_runner.py` in one
respect, documented here rather than left implicit: colleague-bridge relies
on colleague's own `--background` flag to detach a child process it must
then re-discover by PID + a result file, and tails a file-based flight feed
(`colleague_bridge/flightfiles.py`) for progress — because colleague-bridge
does not itself parent that detached child. `pi --acp` has no equivalent
detach flag, so this bridge instead spawns and OWNS the `pi --acp`
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
daemon thread, so the request handler never blocks on pi finishing.

Ordering guarantee the conformance kit checks for (PRD §13.3/§13.4): the
runner thread ALWAYS sends the `accepted` event, synchronously, as the very
first thing it does, before it ever reads a byte of the child's stdout — so
even a pi session that finishes before the runner thread gets its first
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

from pi_bridge import mapping, pi_cli, preserve, scope_guard, workspace
from pi_bridge.callbacks import CallbackConfig, CallbackEmitter
from pi_bridge.config import Config
from pi_bridge.session_registry import SessionRegistry

logger = logging.getLogger("pi_bridge.async_runner")

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


def _stderr_reader(stderr, sink: list[str]) -> None:
    """Runs in its own daemon thread: drains the child's stderr into *sink*.

    Two jobs, and the second one is the reason it cannot be dropped:

    1. The driver writes its refusal line (`wire.REFUSAL_MARKER`) to stderr.
       Nothing on this path read it, so an ACP policy refusal arrived as the
       generic "killed, crashed, or timed out" incomplete-session failure and
       the actionable message was computed and discarded (#225).
    2. `dispatch.spawn` opens stderr as a PIPE. An unread pipe has a finite
       kernel buffer (~64KB): a session chatty enough on stderr would block
       the child forever, and this runner would sit on a stdout EOF that never
       comes until its own deadline fired and reported a timeout. Draining is
       what makes that unreachable.

    No queue: unlike stdout, nothing streams from stderr as it arrives — it is
    only ever read once the process is done.
    """
    try:
        for line in iter(stderr.readline, ""):
            sink.append(line)
    except (ValueError, OSError):  # pipe closed under us; nothing to salvage
        pass


@dataclass
class AsyncInvocation:
    invocation_id: str
    proc: subprocess.Popen
    ctx: mapping.InvocationContext
    #: t10: the git snapshot `start()` captured right before this
    #: invocation's pi subprocess was spawned; `_run` measures against
    #: it once the session ends.
    workspace_handle: workspace.WorkspaceHandle
    started_at: float = field(default_factory=time.monotonic)
    done: bool = False
    cancel_requested: bool = False
    #: t9 / #90: did THIS dispatch ask for its changes to be handed over as
    #: a git ref? Carried per-invocation rather than read off the config,
    #: because it is a per-dispatch opt-in the caller makes and not a
    #: property of the host this bridge runs on.
    handover: bool = False
    #: t6 (c44/h37): the session_key slot this invocation holds, and the
    #: registry to release it from once pi's turn actually finishes.
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
        # #227: the ACP session mode, forwarded to `pi_cli.spawn`. `spawn`
        # has always accepted it; this method never passed it, so every async
        # dispatch reached the mode gate with the empty string and was refused.
        # Async is the path production uses (`always_async: true`), so the
        # omission here meant the bridge executed nothing at all.
        mode: str | None,
        ctx: mapping.InvocationContext,
        callback_url: str,
        callback_token: str,
        heartbeat_after_seconds: int,
        continuation_ref: str | None = None,
        # t9 / #90: threaded through to `pi_cli.spawn` so an async
        # handover dispatch gets the same `.git` widening a sync one does.
        # Async is the path production actually uses (`always_async: true`),
        # so a wire that stopped at run_sync would have left the flag dead
        # in exactly the deployment that needs it.
        writable_git: bool = False,
        # t9 / #90: the ref-creation half of the same opt-in. It is a
        # SEPARATE parameter from `writable_git` even though this bridge's
        # server passes the one request flag to both, because the two are
        # different mechanisms with different reach: `writable_git` is a
        # backend sandbox flag and exists in no other bridge, while creating
        # the ref is the part every backend does.
        handover: bool = False,
        session_registry: SessionRegistry | None = None,
        session_key: str | None = None,
        session_holder: str | None = None,
    ) -> str:
        """Spawn `pi --acp` in the background and return its invocation
        id immediately. Raises `pi_cli.SpawnError` if the subprocess
        itself could not be started (mirrors colleague-bridge's own
        `BackgroundDispatchError`, for the same 503-mapping purpose in
        `server.py`).

        t10: unlike `claude_code_bridge`/`colleague_bridge` (where
        `server.py` calls `workspace.begin()` before a two-step
        spawn-then-register dance), this bridge owns the `pi --acp`
        `Popen` call directly from THIS method (see the module docstring),
        so the workspace snapshot is captured right here, immediately
        before it, instead — the same "as close as possible to the actual
        subprocess spawn" bracketing, just wired at the point this
        architecture actually spawns the child.

        *continuation_ref* (task t5): threaded straight through to
        `pi_cli.spawn` — the async path is the one long, therefore
        resume-worth-it, sessions actually take.
        """
        handle = workspace.begin(repo)
        proc = pi_cli.spawn(
            self._cfg,
            instruction,
            repo,
            model=model,
            sandbox=sandbox,
            mode=mode,
            continuation_ref=continuation_ref,
            writable_git=writable_git,
        )
        invocation_id = uuid.uuid4().hex
        inv = AsyncInvocation(
            invocation_id=invocation_id,
            proc=proc,
            ctx=ctx,
            workspace_handle=handle,
            handover=handover,
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
            name=f"pi-bridge-run-{invocation_id}",
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
                pi_cli.terminate_group(inv.proc)
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
            stdout_text, timed_out, stderr_text = self._stream_until_done(
                inv, emitter, heartbeat_after_seconds
            )
        finally:
            # t6 (c44/h37): pi's own turn is over (successfully,
            # failed, or timed out) — release the session_key slot the
            # instant the PROVIDER call itself is done, not after the
            # (possibly slow/retried) terminal callback below, so the next
            # same-key arrival stops forking as soon as it honestly can.
            if inv.session_registry is not None:
                inv.session_registry.release(inv.session_key, inv.session_holder)
        task_result = pi_cli.parse_session(stdout_text)
        if inv.cancel_requested:
            task_result = {
                "status": "error",
                "error": "pi invocation cancelled",
                "termination_reason": "cancelled",
            }

        # t10: measured AFTER the session ends, against the snapshot taken
        # right before it started — this is what makes head_before/after
        # and the diff bracket the actual pi subprocess's lifetime.
        measured = workspace.measure(inv.workspace_handle)

        # #225: a driver REFUSAL is not an execution failure and must not be
        # reported as one. The driver writes its reason to stderr and exits
        # REFUSAL_EXIT_CODE; both halves are checked, mirroring `run_sync`,
        # because an exit code with no marker is a driver fault worth saying
        # out loud rather than silently re-classing as a crash.
        refusal = pi_cli.refusal_detail(stderr_text)
        refused = refusal is not None or inv.proc.returncode == pi_cli.REFUSAL_EXIT_CODE
        if refused:
            ev = mapping.TerminalEvent(
                kind="failed",
                payload={
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                    "message": refusal
                    or (
                        "the pi ACP driver refused before serving but wrote no marker "
                        "line - driver fault"
                    ),
                    "detail": (
                        "refused by the bridge's own ACP policy gate before the turn began; "
                        "nothing ran and the workspace is untouched"
                    ),
                    "workspace_measured": measured,
                },
            )
        else:
            ev = mapping.terminal_event(
                task_result,
                inv.ctx,
                default_success_outcome=self._cfg.default_success_outcome,
                actor_id=self._cfg.actor_id,
                created_at=_now_iso(),
                timed_out=timed_out,
                workspace_measured=measured,
            )
        # Issue #98: the same workflow-scope boundary server.py applies to a
        # synchronous response, applied to the terminal event — decided on
        # the change set THIS bridge measured, never on the instruction text
        # or on pi's own account of what it touched. Before the preserve
        # hook below, so a refused change set is preserved rather than lost.
        scope_violations = scope_guard.violations(inv.workspace_handle.repo, measured)
        if scope_violations and not refused:
            ev = mapping.TerminalEvent(
                kind="failed",
                payload=scope_guard.refusal_payload(scope_violations, measured),
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
        # t9 / #90: the SUCCESS twin of the preserve hook above, and the
        # path production actually takes (`always_async`). `preserve.
        # handover_ref` had no caller in any bridge until this wire existed,
        # so no asynchronous dispatch had ever created a handover ref
        # either. "completed" is the only other kind terminal_event ever
        # produces, so the two hooks partition the terminal branches between
        # them and no invocation runs both. The block rides on the terminal
        # event's payload, which is what carries it to the control plane —
        # see server.py's synchronous copy for the full argument.
        if ev.kind == "completed":
            handover_result = preserve.handover_ref(
                inv.workspace_handle.repo,
                measured,
                enabled=inv.handover,
                remote=self._cfg.handover_remote,
                run_id=inv.ctx.run_id,
                node_run_id=inv.ctx.node_run_id,
                attempt_id=inv.ctx.attempt_id,
                reason=preserve.handover_success_reason(ev.payload.get("outcome")),
            )
            if handover_result.attempted:
                ev.payload["handover"] = handover_result.to_dict()
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

        Returns `(full_stdout_text, timed_out, full_stderr_text)`. `timed_out` is only True
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
        transcript = pi_cli.transcript_path(self._cfg, inv.invocation_id)
        transcript.touch(exist_ok=True)

        q: "queue.Queue[Any]" = queue.Queue()
        reader = threading.Thread(target=_stdout_reader, args=(proc.stdout, q), daemon=True)
        reader.start()

        # #225: drained in parallel with stdout, never after it — a stderr
        # pipe that fills while this loop waits on stdout is a deadlock, not
        # a slow read.
        stderr_lines: list[str] = []
        stderr_reader = threading.Thread(
            target=_stderr_reader, args=(proc.stderr, stderr_lines), daemon=True
        )
        stderr_reader.start()

        while True:
            now = time.monotonic()
            if now >= deadline:
                self._terminate_and_drain(proc, q, lines)
                stderr_reader.join(timeout=2.0)
                return "\n".join(lines), True, "".join(stderr_lines)

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
            with transcript.open("a", encoding="utf-8") as sink:
                sink.write(item)
            event = _try_parse(line)
            if event is not None:
                emitter.send("progress", {"note": _describe_event(event), "raw": event})
            last_event_at = time.monotonic()

        try:
            proc.wait(timeout=10.0)
        except subprocess.TimeoutExpired:
            logger.warning(
                "pi process for %s did not exit promptly after stdout EOF", inv.invocation_id
            )
        stderr_reader.join(timeout=2.0)
        return "\n".join(lines), False, "".join(stderr_lines)

    @staticmethod
    def _terminate_and_drain(
        proc: subprocess.Popen, q: "queue.Queue[Any]", lines: list[str]
    ) -> None:
        """The bridge's own `async_wait_seconds` deadline fired: SIGTERM
        (never SIGKILL) and collect whatever was already queued, without
        waiting further — the session is reported as a timeout, not as
        whatever partial state it happened to reach."""
        if proc.poll() is None:
            pi_cli.terminate_group(proc)
        while True:
            try:
                item = q.get_nowait()
            except queue.Empty:
                break
            if item is _EOF:
                break
            lines.append(item.rstrip("\n"))
