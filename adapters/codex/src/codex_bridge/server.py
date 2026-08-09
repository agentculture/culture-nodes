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
"""

from __future__ import annotations

import json
import logging
import re
import threading
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any

from codex_bridge import codex_cli, mapping
from codex_bridge.async_runner import AsyncRunner
from codex_bridge.config import Config
from codex_bridge.idempotency import IdempotencyStore

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


class Bridge:
    """The long-lived state one bridge process holds: config, idempotency
    store, and the async invocation registry. One instance is shared by
    every request the (single-threaded) HTTP server handles."""

    def __init__(self, cfg: Config) -> None:
        self.cfg = cfg
        self.idempotency = IdempotencyStore(cfg.state_dir)
        self.async_runner = AsyncRunner(cfg)


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
    protocol_version = "HTTP/1.1"

    # -- stdlib plumbing --------------------------------------------------

    def log_message(self, fmt: str, *args: Any) -> None:  # noqa: A002 - stdlib signature
        logger.info("%s - %s", self.address_string(), fmt % args)

    @property
    def bridge(self) -> Bridge:
        return self.server.bridge  # type: ignore[attr-defined]

    def _write_json(self, status: int, body: dict[str, Any]) -> None:
        payload = json.dumps(body).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
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
        if presented == token:
            return True
        self._drain_body()
        self._write_json(
            401, {"error": "a scoped workload token is required", "class": "auth_or_policy"}
        )
        return False

    # -- HTTP verbs --------------------------------------------------------

    def do_GET(self) -> None:  # noqa: N802 - stdlib naming
        if self.path == "/healthz":
            self._write_json(200, {"status": "ok"})
            return
        self._write_json(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802 - stdlib naming
        try:
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

        repo = raw_input.get("repo")
        if not isinstance(repo, str) or not repo.strip():
            self._write_json(
                400,
                {"error": "input.repo is required", "class": mapping.CLASS_ACTOR_REJECTED_INPUT},
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

        ctx = mapping.InvocationContext(
            run_id=str(body.get("run_id") or ""),
            node_run_id=body.get("node_run_id") or None,
            attempt_id=body.get("attempt_id") or None,
            success_outcome=success_outcome,
            incomplete_outcome=incomplete_outcome,
        )

        if decide_async(cfg, force_async=force_async, max_steps=max_steps):
            self._dispatch_async(idem_key, body, ctx, instruction, resolved_repo, model, sandbox)
            return

        self._dispatch_sync(idem_key, ctx, instruction, resolved_repo, model, sandbox)

    def _dispatch_sync(
        self,
        idem_key: str,
        ctx: mapping.InvocationContext,
        instruction: str,
        repo: str,
        model: str | None,
        sandbox: str | None,
    ) -> None:
        cfg = self.bridge.cfg
        result = codex_cli.run_sync(cfg, instruction, repo, model=model, sandbox=sandbox)
        response = mapping.sync_response(
            result.task_result,
            ctx,
            default_success_outcome=cfg.default_success_outcome,
            actor_id=cfg.actor_id,
            created_at=_now_iso(),
            timed_out=result.timed_out,
        )
        # A real dispatch happened (codex was actually invoked) — durably
        # remember the outcome so a redelivered attempt replays it instead
        # of running codex a second time (PRD §20.3). A pre-dispatch
        # validation refusal (400/403/401, above) is deliberately NOT
        # cached: no side effect occurred, so a corrected retry with the
        # same key must be free to proceed.
        self.bridge.idempotency.put(
            idem_key, response.status_code, response.body, request_fingerprint=instruction
        )
        self._write_json(response.status_code, response.body)

    def _dispatch_async(
        self,
        idem_key: str,
        body: dict[str, Any],
        ctx: mapping.InvocationContext,
        instruction: str,
        repo: str,
        model: str | None,
        sandbox: str | None,
    ) -> None:
        cfg = self.bridge.cfg
        callback = body.get("callback") or {}
        callback_url = callback.get("url") if isinstance(callback, dict) else None
        callback_token = callback.get("token") if isinstance(callback, dict) else None
        if not callback_url or not callback_token:
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
                ctx=ctx,
                callback_url=callback_url,
                callback_token=callback_token,
                heartbeat_after_seconds=cfg.heartbeat_after_seconds,
            )
        except codex_cli.SpawnError as exc:
            self._write_json(503, {"error": str(exc), "class": "actor_unavailable"})
            return

        accepted_body = {
            "invocation_id": invocation_id,
            "heartbeat_after_seconds": cfg.heartbeat_after_seconds,
            "supports_cancellation": True,
        }
        self.bridge.idempotency.put(idem_key, 202, accepted_body, request_fingerprint=instruction)
        self._write_json(202, accepted_body)


def make_server(cfg: Config) -> BridgeHTTPServer:
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
