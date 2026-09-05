"""The bridge's HTTP surface: PRD §13.1/§13.6 over `http.server` (stdlib).
Mirrors `colleague_bridge/server.py`'s request-handling ladder field for
field; the only substantive differences are the input fields this bridge
validates (`sandbox` instead of `role`; no `mode`) and that async dispatch
calls `AsyncRunner.start()` directly (it owns the `codex exec` Popen itself
— see `async_runner.py`'s module docstring) instead of a two-step
spawn-then-register like colleague-bridge's own `--background` dance.

Deliberately single-threaded request handling (`http.server.HTTPServer`,
not `ThreadingHTTPServer`): this is a REFERENCE adapter proving the actor
protocol against a real backend, not a production-scale actor host. A
synchronous invocation blocks the server for the duration of one `codex
exec` call (bounded by `Config.sync_timeout_seconds`); an asynchronous
invocation only blocks it for the near-instant `Popen()` call before
handing the rest to `async_runner.AsyncRunner`'s own daemon threads, so the
server itself is free again immediately after a 202. A deployment that
needs to serve many concurrent SYNCHRONOUS invocations should run several
bridge processes behind a load balancer, or fork this file into a threaded
server — documented here rather than silently assumed.

Routes:

* ``POST /v1/invocations`` — PRD §13.1.
* ``POST /v1/invocations/<id>/cancel`` — PRD §13.6, the path
  `internal/actors.Client.Cancel` actually calls.
* ``DELETE /v1/invocations/<id>`` — an alias for the same cancellation, for
  a caller that prefers the REST-conventional verb; not exercised by the
  conformance kit's client, which only ever calls the `/cancel` path above.
* ``GET /healthz`` — operational convenience, no protocol meaning.
* ``GET /v1/capabilities`` — the preflight capability surface this bridge
  advertises (issue #67, task t15): the measured host facts an actor
  registration carries in ``capabilities``. Authenticated; optional in the
  protocol, so an actor that serves nothing here is still conformant.
"""

from __future__ import annotations

import dataclasses
import hmac
import json
import logging
import os
import re
import threading
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any

from codex_bridge import (
    capabilities,
    codex_cli,
    mapping,
    preflight,
    preserve,
    repositories,
    scope_guard,
    workspace,
)
from codex_bridge.async_runner import AsyncRunner
from codex_bridge.config import Config
from codex_bridge.idempotency import IdempotencyStore
from codex_bridge.session_registry import SessionRegistry

logger = logging.getLogger("codex_bridge.server")

INVOCATIONS_PATH = "/v1/invocations"
_CANCEL_RE = re.compile(r"^/v1/invocations/([^/]+)/cancel$")
_ID_RE = re.compile(r"^/v1/invocations/([^/]+)$")

#: Bounds a request body the handler will read. An actor is entitled to
#: refuse an oversized request; it is never entitled to let one exhaust the
#: process's memory (the same stance `internal/actors/client.go` takes on
#: response bodies, mirrored here on the way in).
MAX_BODY_BYTES = 8 * 1024 * 1024


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def _resume_session_lost(result: codex_cli.SyncRunResult) -> bool:
    """Recognise only provider diagnostics that say the resume handle died."""
    text = "\n".join(
        part
        for part in (
            result.stderr,
            json.dumps(result.task_result, sort_keys=True) if result.task_result else "",
        )
        if part
    ).lower()
    return any(
        marker in text
        for marker in (
            "session not found",
            "thread not found",
            "unknown session",
            "unknown thread",
            "invalid continuation",
            "expired session",
            "expired thread",
        )
    )


def _prepend_resume_rebrief(instruction: str, raw_input: dict[str, Any]) -> str:
    """Bound the cold fork's context to the facts needed to answer safely."""
    resume_event = raw_input.get("resume_event")
    question = raw_input.get("question")
    answer: Any = None
    if isinstance(resume_event, dict):
        payload = resume_event.get("payload", resume_event)
        if isinstance(payload, dict):
            answer = payload.get("answer")
    minimal = {
        "question_id": raw_input.get("question_id"),
        "originating_question_id": (
            resume_event.get("originating_question_id") if isinstance(resume_event, dict) else None
        ),
    }
    rebrief = {
        "question": question,
        "answer": answer,
        "context": {key: value for key, value in minimal.items() if value is not None},
    }
    return (
        "RESUME RE-BRIEF (provider session was lost):\n"
        + json.dumps(rebrief, ensure_ascii=False, sort_keys=True)
        + "\n\n"
        + instruction
    )


class Bridge:
    """The long-lived state one bridge process holds: config, idempotency
    store, and the async invocation registry. One instance is shared by
    every request the (single-threaded) HTTP server handles."""

    def __init__(self, cfg: Config) -> None:
        self.cfg = cfg
        self.idempotency = IdempotencyStore(cfg.state_dir)
        self.async_runner = AsyncRunner(cfg)
        # t6 (c44/h37): exactly one in-flight invocation per session_key;
        # a concurrent collision forks cold rather than interleaving turns
        # on one provider thread — see session_registry.py's docstring.
        self.session_registry = SessionRegistry(max_inflight=cfg.max_inflight_per_session_key)


def decide_async(cfg: Config, *, force_async: bool | None, max_steps: int | None) -> bool:
    """Sync-vs-async dispatch policy: an always-async config flag wins
    outright; else the invocation's own `input.async` override wins; else
    the expected step budget (the invocation's `input.max_steps`, or
    `Config.default_max_steps` when absent) is compared against
    `Config.sync_max_steps`. Identical decision colleague-bridge makes —
    `max_steps` here is a dispatch-timing signal only (codex has no native
    step-budget flag to forward it to; see README.md)."""
    if cfg.always_async:
        return True
    if force_async is not None:
        return force_async
    expected_steps = max_steps if max_steps is not None else cfg.default_max_steps
    return expected_steps > cfg.sync_max_steps


class BridgeHTTPServer(HTTPServer):
    def __init__(
        self, address: tuple[str, int], handler_cls: type[BaseHTTPRequestHandler], bridge: Bridge
    ) -> None:
        super().__init__(address, handler_cls)
        self.bridge = bridge


class Handler(BaseHTTPRequestHandler):
    server_version = "codex-bridge/0.1"
    #: Bound how long one connection may hold this single-threaded server.
    #:
    #: protocol_version = "HTTP/1.1" turns keep-alive ON, and this server is
    #: deliberately NOT a ThreadingHTTPServer (see the module docstring), so
    #: it serves exactly one connection at a time. Without a timeout, a client
    #: that opens a socket and then says nothing holds the accept loop
    #: forever and every subsequent dispatch waits behind it — no error, no
    #: log, just a bridge that stops answering.
    #:
    #: BaseHTTPRequestHandler turns a read timeout into close_connection, so
    #: an idle peer is dropped rather than served badly. It bounds the gap
    #: BETWEEN requests on a kept-alive connection, not the work itself:
    #: dispatch runs after the request line is read, so a slow model turn is
    #: unaffected.
    timeout = 30

    protocol_version = "HTTP/1.1"

    # -- stdlib plumbing --------------------------------------------------

    def log_message(self, fmt: str, *args: Any) -> None:  # noqa: A002 - stdlib signature
        logger.info("%s - %s", self.address_string(), fmt % args)

    @property
    def bridge(self) -> Bridge:
        return self.server.bridge  # type: ignore[attr-defined]

    def _write_json(
        self,
        status: int,
        body: dict[str, Any],
        *,
        extra_headers: dict[str, str] | None = None,
        close: bool = False,
    ) -> None:
        payload = json.dumps(body).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        for name, value in (extra_headers or {}).items():
            self.send_header(name, value)
        if close:
            # send_header sees the Connection: close and flips
            # self.close_connection, so the handler loop really hangs up
            # after this response.
            self.send_header("Connection", "close")
        self.end_headers()
        try:
            self.wfile.write(payload)
        except (BrokenPipeError, ConnectionResetError):
            logger.warning("client disconnected before the response could be written")

    def _read_raw_body(self) -> bytes:
        try:
            length = int(self.headers.get("Content-Length", "0") or "0")
        except ValueError:
            length = 0
        length = max(0, min(length, MAX_BODY_BYTES))
        return self.rfile.read(length) if length else b""

    def _drain_body(self) -> None:
        self._read_raw_body()

    def _refuse_oversized_body(self) -> bool:
        """Answer 413 + Connection: close when the declared body exceeds
        MAX_BODY_BYTES; True means the request was refused and handled.

        Truncating the read at the cap instead left the remainder unread
        on the keep-alive connection, so the next request's parse started
        mid-body — request desynchronization. Something is drained before
        the 413 because a socket closed with bytes still in flight can RST
        the response out of the client's receive buffer, but the drain is
        bounded at MAX_BODY_BYTES rather than the declared length: this
        runs before auth, so draining everything a client declares would
        let an unauthenticated caller hold the single-threaded server
        reading for as long as it likes — pre-auth read amplification.
        """
        try:
            length = int(self.headers.get("Content-Length", "0") or "0")
        except ValueError:
            length = 0
        if length <= MAX_BODY_BYTES:
            return False
        remaining = MAX_BODY_BYTES
        while remaining > 0:
            chunk = self.rfile.read(min(remaining, 65536))
            if not chunk:
                break
            remaining -= len(chunk)
        self._write_json(
            413,
            {
                "error": f"request body exceeds {MAX_BODY_BYTES} bytes",
                "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
            },
            close=True,
        )
        return True

    def _read_json_body(self) -> dict[str, Any] | None:
        raw = self._read_raw_body()
        if not raw:
            self._write_json(
                400,
                {"error": "request body is required", "class": mapping.CLASS_ACTOR_REJECTED_INPUT},
            )
            return None
        try:
            data = json.loads(raw)
        except ValueError:
            self._write_json(
                400,
                {
                    "error": "request body is not valid JSON",
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return None
        if not isinstance(data, dict):
            self._write_json(
                400,
                {
                    "error": "request body must be a JSON object",
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return None
        return data

    def _require_auth(self) -> bool:
        token = self.bridge.cfg.auth_token
        if not token:
            return True
        header = self.headers.get("Authorization", "")
        presented = header[len("Bearer ") :] if header.startswith("Bearer ") else ""
        if hmac.compare_digest(presented, token):
            return True
        self._drain_body()
        self._write_json(
            401, {"error": "a scoped workload token is required", "class": "auth_or_policy"}
        )
        return False

    # -- HTTP verbs --------------------------------------------------------

    def do_GET(self) -> None:  # noqa: N802 - stdlib naming
        try:
            if self.path == "/healthz":
                self._write_json(200, {"status": "ok"})
                return
            if self.path == preflight.CAPABILITIES_PATH:
                self._handle_capabilities()
                return
            self._write_json(404, {"error": "not found"})
        except Exception:  # noqa: BLE001 - the server must answer, never crash
            logger.exception("unhandled error handling GET %s", self.path)
            self._write_json(
                500, {"error": "internal bridge error", "class": mapping.CLASS_EXECUTION}
            )

    def _handle_capabilities(self) -> None:
        """Serve this bridge's preflight capability surface (issue #67, task
        t15) — measured now, on this host, not read back from a config file.

        Authenticated like the invocation route: the block names a hostname
        and real filesystem paths, so `/healthz` stays the only
        unauthenticated route. Advertising is all a bridge does here; the
        gate that reads it is per-actor, default-off, and entirely
        engine-side (`internal/preflight`).
        """
        if not self._require_auth():
            return
        self._write_json(200, preflight.capability_block(capabilities.host_facts(self.bridge.cfg)))

    def do_POST(self) -> None:  # noqa: N802 - stdlib naming
        try:
            if self._refuse_oversized_body():
                return
            if self.path == INVOCATIONS_PATH:
                self._handle_invocation()
                return
            m = _CANCEL_RE.match(self.path)
            if m:
                self._handle_cancel(m.group(1))
                return
            self._drain_body()
            self._write_json(404, {"error": "not found"})
        except Exception:  # noqa: BLE001 - the server must answer, never crash
            logger.exception("unhandled error handling POST %s", self.path)
            self._write_json(
                500, {"error": "internal bridge error", "class": mapping.CLASS_EXECUTION}
            )

    def do_DELETE(self) -> None:  # noqa: N802 - stdlib naming
        try:
            if self._refuse_oversized_body():
                return
            m = _ID_RE.match(self.path)
            if m:
                self._handle_cancel(m.group(1))
                return
            self._drain_body()
            self._write_json(404, {"error": "not found"})
        except Exception:  # noqa: BLE001 - the server must answer, never crash
            logger.exception("unhandled error handling DELETE %s", self.path)
            self._write_json(
                500, {"error": "internal bridge error", "class": mapping.CLASS_EXECUTION}
            )

    # -- route bodies --------------------------------------------------------

    def _handle_cancel(self, invocation_id: str) -> None:
        if not self._require_auth():
            return
        self._drain_body()  # an optional CancelRequest body; best-effort, unread fields ignored
        self.bridge.async_runner.cancel(invocation_id)
        # PRD §13.6: cancellation is durable in Culture Nodes and best-effort
        # at the actor — the endpoint must answer, not prove the work
        # actually stopped, and it answers success whether or not this
        # invocation id is one the bridge still knows about.
        self._write_json(202, {"invocation_id": invocation_id, "status": "cancel-requested"})

    def _handle_invocation(self) -> None:  # noqa: C901 - one straight-line validation ladder
        cfg = self.bridge.cfg
        if not self._require_auth():
            return

        idem_key = (self.headers.get("Idempotency-Key") or "").strip()
        if not idem_key:
            self._drain_body()
            self._write_json(
                400,
                {
                    "error": "Idempotency-Key header is required",
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return

        replay = self.bridge.idempotency.get(idem_key)
        if replay is not None:
            self._drain_body()
            self._write_json(replay.status_code, replay.body)
            return

        body = self._read_json_body()
        if body is None:
            return

        if body.get("protocol_version") != "1.0":
            got = body.get("protocol_version")
            self._write_json(
                400,
                {
                    "error": f"protocol_version {got!r} is not supported; this bridge speaks 1.0",
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return

        raw_input = body.get("input")
        raw_input = raw_input if isinstance(raw_input, dict) else {}

        instruction = raw_input.get("instruction")
        if not isinstance(instruction, str) or not instruction.strip():
            self._write_json(
                400,
                {
                    "error": "input.instruction is required and must be a non-empty string",
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return

        # Engine-resolved bindings beyond the transport fields ride into the
        # session as a serialized context block: a node's input.bindings
        # (fixReport, evidence projections, ...) are resolved by the worker
        # and arrive here as extra input keys — dropping them silently made
        # bound inputs invisible to the model (found live: pr-upkeep cycle 5,
        # the review session honestly reported its bindings missing;
        # deviation d3). Same block in all three bridges (all-backends rule).
        _transport_keys = {
            "instruction",
            "repo",
            "sandbox",
            "model",
            "success_outcome",
            "permission_mode",
            # t5: session_key (spec claim c3's eventual workstream key,
            # ADR 0010 §4) and continuation_ref both stay out of the
            # Bound-inputs block so neither ever reaches the model as
            # prompt text. continuation_ref itself is a TOP-LEVEL §13.1
            # request field per internal/actors/protocol.go (read from
            # `body`, not `raw_input`, below) — it is listed here only as a
            # defensive belt-and-suspenders exclusion for a caller that
            # nests it in `input` anyway.
            "session_key",
            "continuation_ref",
            # t2/#125: the registry-supplied repository identity is an
            # ADDRESSING field — it says which checkout this session runs
            # in, the same way `repo` does. Left out of this set it would be
            # appended to the prompt as an engine-resolved "Bound input",
            # which is prose the model would be right to treat as an
            # instruction about a repository.
            repositories.INPUT_KEY,
        }
        _extras = {k: v for k, v in raw_input.items() if k not in _transport_keys}
        if _extras:
            _serialized = json.dumps(_extras, indent=2, ensure_ascii=False)
            if len(_serialized) > 60000:
                _serialized = _serialized[:60000] + "\n... [truncated at 60000 chars]"
            instruction = (
                instruction + "\n\n## Bound inputs (engine-resolved, verbatim)\n" + _serialized
            )

        repo = raw_input.get("repo")
        if not isinstance(repo, str) or not repo.strip():
            # Issue #125: a trigger-created run's input is the event payload,
            # which carries no checkout path. The control plane supplies the
            # actor's REGISTERED repository identity instead (task t1), and
            # this host is the only party that can turn that name into a
            # directory — see repositories.py for the mapping and for why
            # both a collision and a miss are named refusals rather than a
            # first match. An explicitly bound `input.repo` still wins: the
            # identity answers "which repository is this actor's lane",
            # which is only a question when nobody has answered it.
            _identity = repositories.resolve_for_input(cfg, raw_input)
            if _identity.refusal is not None:
                self._write_json(_identity.refusal.status, _identity.refusal.body)
                return
            # No identity registered: the pre-t2 cardinality inference, which
            # still resolves every single-repo deployment shipped today.
            repo = _identity.repo or cfg.only_allowed_repo()
        if not isinstance(repo, str) or not repo.strip():
            self._write_json(
                400,
                {
                    "error": (
                        "input.repo is required (this actor is registered with no "
                        f"{repositories.INPUT_KEY}, and this bridge's allowlist does not "
                        "name exactly one repository, so it cannot be inferred)"
                    ),
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return
        if not cfg.repo_allowed(repo):
            self._write_json(
                403,
                {
                    "error": f"repo {repo!r} is not in this bridge's configured allowlist",
                    "class": "auth_or_policy",
                },
            )
            return
        resolved_repo = str(Path(repo).expanduser().resolve())

        base_ref = raw_input.get("base_ref")
        if base_ref is not None and (not isinstance(base_ref, str) or not base_ref):
            self._write_json(
                400,
                {
                    "error": "input.base_ref must be a non-empty string",
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return

        model = raw_input.get("model") or None
        if model is not None and not isinstance(model, str):
            self._write_json(
                400,
                {
                    "error": "input.model must be a string",
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return

        sandbox = raw_input.get("sandbox") or None
        if sandbox is not None and sandbox not in codex_cli.SANDBOX_MODES:
            self._write_json(
                400,
                {
                    "error": f"sandbox {sandbox!r} is not one of {sorted(codex_cli.SANDBOX_MODES)}",
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return

        max_steps = raw_input.get("max_steps")
        if max_steps is not None and (
            isinstance(max_steps, bool) or not isinstance(max_steps, int)
        ):
            self._write_json(
                400,
                {
                    "error": "input.max_steps must be an integer",
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return

        force_async = raw_input.get("async")
        if force_async is not None and not isinstance(force_async, bool):
            self._write_json(
                400,
                {
                    "error": "input.async must be a boolean",
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return

        success_outcome = raw_input.get("success_outcome") or None
        incomplete_outcome = raw_input.get("incomplete_outcome") or None
        session_key = raw_input.get("session_key") or None
        if session_key is not None and not isinstance(session_key, str):
            self._write_json(
                400,
                {
                    "error": "input.session_key must be a string",
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return
        # §13.1's continuation_ref is a top-level request field (a sibling
        # of run_id/node_run_id/attempt_id), mirroring how those are read
        # from `body` two lines below — not nested inside `input`. The
        # `raw_input.get(...)` fallback is defensive only.
        continuation_ref = body.get("continuation_ref") or raw_input.get("continuation_ref") or None

        # t9 / #90: does this dispatch hand its changes over as a git ref?
        #
        # Both halves of that path — the `.git` sandbox widening in
        # codex_cli and preserve.handover_ref — were written and unit-tested
        # while NOTHING set this flag, so every dispatch ran with a read-only
        # `.git` and no session could commit its own work (deviation d2). It
        # is read here, once, and threaded to both.
        #
        # Opt-in on purpose, and the two boundaries agree: a package that
        # hands nothing over is given neither the ref plumbing nor `.git`
        # write. Widening the sandbox for every session to spare this flag
        # would serve the minority at the majority's expense (issue #91).
        handover = bool(raw_input.get("handover"))
        if handover and sandbox != codex_cli.SANDBOX_WORKSPACE_WRITE:
            self._write_json(
                400,
                {
                    "error": (
                        "input.handover requires sandbox 'workspace-write': a session that "
                        "cannot write its workspace has no changes to hand over"
                    ),
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return

        ctx = mapping.InvocationContext(
            run_id=str(body.get("run_id") or ""),
            node_run_id=body.get("node_run_id") or None,
            attempt_id=body.get("attempt_id") or None,
            success_outcome=success_outcome,
            incomplete_outcome=incomplete_outcome,
            continuation_ref=continuation_ref,
        )

        # t6 (c44/h37): exactly one in-flight invocation per session_key.
        # `held` is True iff THIS invocation claimed the slot and therefore
        # owes it a `release()`; `forked` is True iff another invocation
        # already held it, in which case this one dispatches cold
        # (continuation_ref discarded) instead of interleaving a turn onto
        # the same provider thread. See session_registry.py for the
        # fork-vs-queue argument.
        held = False
        forked = False
        if cfg.session_concurrency_enabled and session_key:
            held = self.bridge.session_registry.acquire(session_key, idem_key)
            forked = not held
            if forked:
                ctx = dataclasses.replace(ctx, continuation_ref=None)

        if decide_async(cfg, force_async=force_async, max_steps=max_steps):
            self._dispatch_async(
                idem_key,
                body,
                ctx,
                instruction,
                resolved_repo,
                model,
                sandbox,
                base_ref=base_ref,
                handover=handover,
                session_key=session_key,
                held=held,
                forked=forked,
            )
            return

        self._dispatch_sync(
            idem_key,
            ctx,
            instruction,
            resolved_repo,
            model,
            sandbox,
            raw_input=raw_input,
            base_ref=base_ref,
            handover=handover,
            session_key=session_key,
            held=held,
            forked=forked,
        )

    def _dispatch_sync(
        self,
        idem_key: str,
        ctx: mapping.InvocationContext,
        instruction: str,
        repo: str,
        model: str | None,
        sandbox: str | None,
        *,
        raw_input: dict[str, Any] | None = None,
        base_ref: str | None = None,
        handover: bool = False,
        session_key: str | None = None,
        held: bool = False,
        forked: bool = False,
    ) -> None:
        cfg = self.bridge.cfg
        # t10: capture the workspace's starting point as close as possible
        # to the moment codex is actually spawned, so head_before/status
        # bracket the session rather than the whole request-handling ladder.
        try:
            handle = workspace.begin(repo, base_ref=base_ref)
        except workspace.WorkspaceProvisionError as exc:
            if held:
                self.bridge.session_registry.release(session_key, idem_key)
            self._write_json(503, {"error": str(exc), "class": "actor_unavailable"})
            return
        try:
            result = codex_cli.run_sync(
                cfg,
                instruction,
                repo,
                model=model,
                sandbox=sandbox,
                continuation_ref=ctx.continuation_ref,
                writable_git=handover,
            )
            if ctx.continuation_ref and session_key and _resume_session_lost(result):
                self.bridge.session_registry.record_lost_resume(session_key, idem_key)
                cold_instruction = _prepend_resume_rebrief(instruction, raw_input or {})
                ctx = dataclasses.replace(ctx, continuation_ref=None)
                forked = True
                result = codex_cli.run_sync(
                    cfg,
                    cold_instruction,
                    repo,
                    model=model,
                    sandbox=sandbox,
                    continuation_ref=None,
                    writable_git=handover,
                )
        finally:
            # t6 (c44/h37): the provider call is over (successfully or
            # not) — release the session_key slot so the NEXT invocation
            # for it (queued behind this one in wall-clock time, not in a
            # literal queue) may use its continuation_ref as given instead
            # of forking.
            if held:
                self.bridge.session_registry.release(session_key, idem_key)
        measured = workspace.measure(handle)
        response = mapping.sync_response(
            result.task_result,
            ctx,
            default_success_outcome=cfg.default_success_outcome,
            actor_id=cfg.actor_id,
            created_at=_now_iso(),
            timed_out=result.timed_out,
            workspace_measured=measured,
        )
        # Issue #98: the workflow-scope boundary, decided on what the
        # session actually changed. Placed between the response and the
        # preserve hook on purpose — the refusal has to be a non-200 BEFORE
        # that hook reads `response.status_code`, so refused work lands on a
        # preserve branch rather than being reported as a success or thrown
        # away.
        scope_violations = scope_guard.violations(repo, measured)
        if scope_violations:
            response = mapping.SyncResponse(
                status_code=403,
                body=scope_guard.refusal_body(scope_violations, measured),
            )
        # t25 (c26/h17, c41/h34): a genuine technical failure (never a
        # domain outcome — mapping.sync_response only ever answers 200 for
        # one) gets its workspace changes preserved on a branch, bridge-side,
        # before this response ever reaches the caller. Gated on the status
        # THIS response already carries, never on this module's own reading
        # of codex's result.
        if response.status_code != 200:
            preserve_result = preserve.preserve_on_failure(
                repo,
                measured,
                enabled=cfg.preserve_on_failure,
                push=cfg.preserve_push,
                remote=cfg.preserve_remote,
                branch_prefix=cfg.preserve_branch_prefix,
                run_id=ctx.run_id,
                node_run_id=ctx.node_run_id,
                attempt_id=ctx.attempt_id,
                reason=str(response.body.get("error") or "bridge reported a non-success status"),
            )
            response.body["preserve"] = preserve_result.to_dict()
        # t9 / #90: the OTHER half of the handover opt-in, and the half that
        # had no caller in any bridge — `preserve.handover_ref` was written
        # and unit-tested everywhere and invoked nowhere, so no dispatch in
        # any backend had ever created a handover ref. A dispatch that asked
        # for one, and SUCCEEDED, creates it here and reports it in the body,
        # which is what gives the control plane a ref to fetch and measure
        # (t10, issue #13) instead of an agent's account of its own work.
        #
        # Success only, and mutually exclusive with the preserve hook above
        # by construction (that one gates on != 200, this on == 200): a
        # failed session's changes belong on a preserve branch, and handing
        # them over as a ref would offer the graph a deliverable the session
        # never finished.
        #
        # `enabled` is passed rather than checked here so the opt-in stays
        # declared in one place — handover_ref's own documented contract —
        # and a dispatch that asked for nothing runs no git command at all.
        # The block is attached only when something was actually attempted,
        # so an ordinary dispatch's response is byte-for-byte unchanged.
        if response.status_code == 200:
            handover_result = preserve.handover_ref(
                repo,
                measured,
                enabled=handover,
                remote=cfg.handover_remote,
                run_id=ctx.run_id,
                node_run_id=ctx.node_run_id,
                attempt_id=ctx.attempt_id,
                reason=preserve.handover_success_reason(response.body.get("outcome")),
            )
            if handover_result.attempted:
                response.body["handover"] = handover_result.to_dict()
        # A real dispatch happened (codex was actually invoked) — durably
        # remember the outcome so a redelivered attempt replays it instead
        # of running codex a second time (PRD §20.3). A pre-dispatch
        # validation refusal (400/403/401, above) is deliberately NOT
        # cached: no side effect occurred, so a corrected retry with the
        # same key must be free to proceed.
        self.bridge.idempotency.put(
            idem_key, response.status_code, response.body, request_fingerprint=instruction
        )
        # capacity_exhausted's delay (t5, deviation d4) rides the HTTP
        # Retry-After header, never the JSON body — internal/actors/
        # client.go reads it from exactly that header and nowhere else.
        extra_headers = {}
        if response.retry_after_seconds is not None:
            extra_headers["Retry-After"] = str(max(0, round(response.retry_after_seconds)))
        # t6 (c44/h37): a fork must be observable on the wire, not merely
        # inferable from an unexpectedly-fresh continuation_ref — see
        # session_registry.py.
        if forked:
            extra_headers["X-Session-Fork"] = "1"
        self._write_json(response.status_code, response.body, extra_headers=extra_headers or None)

    def _dispatch_async(
        self,
        idem_key: str,
        body: dict[str, Any],
        ctx: mapping.InvocationContext,
        instruction: str,
        repo: str,
        model: str | None,
        sandbox: str | None,
        *,
        base_ref: str | None = None,
        handover: bool = False,
        session_key: str | None = None,
        held: bool = False,
        forked: bool = False,
    ) -> None:
        cfg = self.bridge.cfg
        callback = body.get("callback") or {}
        callback_url = callback.get("url") if isinstance(callback, dict) else None
        callback_token = callback.get("token") if isinstance(callback, dict) else None
        if not callback_url or not callback_token:
            if held:
                self.bridge.session_registry.release(session_key, idem_key)
            self._write_json(
                400,
                {
                    "error": "callback.url and callback.token are required for async dispatch",
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return

        try:
            invocation_id = self.bridge.async_runner.start(
                instruction=instruction,
                repo=repo,
                model=model,
                sandbox=sandbox,
                continuation_ref=ctx.continuation_ref,
                writable_git=handover,
                handover=handover,
                ctx=ctx,
                callback_url=callback_url,
                callback_token=callback_token,
                heartbeat_after_seconds=cfg.heartbeat_after_seconds,
                base_ref=base_ref,
                # t6 (c44/h37): the runner releases this session_key's
                # slot once codex's turn actually finishes — None here
                # whenever `held` is False (no slot to release, forked or
                # unserialized).
                session_registry=self.bridge.session_registry if held else None,
                session_key=session_key if held else None,
                session_holder=idem_key,
            )
        except (codex_cli.SpawnError, workspace.WorkspaceProvisionError) as exc:
            if held:
                self.bridge.session_registry.release(session_key, idem_key)
            self._write_json(503, {"error": str(exc), "class": "actor_unavailable"})
            return

        accepted_body = {
            "invocation_id": invocation_id,
            "heartbeat_after_seconds": cfg.heartbeat_after_seconds,
            "supports_cancellation": True,
        }
        self.bridge.idempotency.put(idem_key, 202, accepted_body, request_fingerprint=instruction)
        # t6 (c44/h37): observable on the wire even for the fast 202 path —
        # see session_registry.py and _dispatch_sync's matching header.
        self._write_json(
            202, accepted_body, extra_headers={"X-Session-Fork": "1"} if forked else None
        )


_LOOPBACK_HOSTS = frozenset({"127.0.0.1", "localhost", "::1"})


def _refuse_unauthenticated_exposure(cfg: Config) -> None:
    """A bridge endpoint executes work on this machine: binding it beyond
    loopback with no auth token would be an unauthenticated remote-execution
    surface. Refuse at startup unless the operator opts in explicitly
    (CODEX_BRIDGE_ALLOW_UNAUTHENTICATED=1) — never by accident."""
    if cfg.auth_token or cfg.host in _LOOPBACK_HOSTS:
        return
    if os.environ.get("CODEX_BRIDGE_ALLOW_UNAUTHENTICATED") == "1":
        logger.warning(
            "serving on %s WITHOUT authentication " "(CODEX_BRIDGE_ALLOW_UNAUTHENTICATED=1)",
            cfg.host,
        )
        return
    raise SystemExit(
        f"refusing to bind {cfg.host} without an auth token: set "
        "CODEX_BRIDGE_AUTH_TOKEN, bind to loopback, or set "
        "CODEX_BRIDGE_ALLOW_UNAUTHENTICATED=1 to accept an "
        "unauthenticated non-loopback endpoint deliberately"
    )


def make_server(cfg: Config) -> BridgeHTTPServer:
    _refuse_unauthenticated_exposure(cfg)
    bridge = Bridge(cfg)
    return BridgeHTTPServer((cfg.host, cfg.port), Handler, bridge)


def serve_forever(cfg: Config) -> None:  # pragma: no cover - exercised via __main__, not unit tests
    server = make_server(cfg)
    logger.info("codex-bridge listening on http://%s:%d", cfg.host, server.server_address[1])
    try:
        server.serve_forever()
    finally:
        server.server_close()


def start_background(cfg: Config) -> tuple[BridgeHTTPServer, threading.Thread]:
    """Start the server on a daemon thread; returns `(server, thread)`.

    Used by tests and by anything embedding the bridge in a longer-lived
    process. `server.server_address` reports the actual bound port when
    `cfg.port == 0` was used to pick a free one.
    """
    server = make_server(cfg)
    # A short poll_interval (default is 0.5s) so `server.shutdown()` in a
    # test's teardown returns quickly instead of padding every test with a
    # fixed delay; harmless in production (it only affects how promptly a
    # shutdown request is noticed, not steady-state request latency).
    thread = threading.Thread(
        target=server.serve_forever,
        kwargs={"poll_interval": 0.05},
        name="codex-bridge-http",
        daemon=True,
    )
    thread.start()
    return server, thread
