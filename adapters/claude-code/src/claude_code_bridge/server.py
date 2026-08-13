"""The bridge's HTTP surface: PRD §13.1/§13.6 over `http.server` (stdlib).

Mirrors `adapters/colleague/src/colleague_bridge/server.py` field for field
and route for route — the actor protocol surface is backend-agnostic; only
the dispatch target (`claude_cli` instead of `colleague_cli`) and the
`input` fields this bridge recognises differ. See that module's own
docstring for the full rationale on the deliberately single-threaded
`http.server.HTTPServer` choice (this is a REFERENCE adapter proving the
actor protocol, not a production-scale actor host).

Routes:

* ``POST /v1/invocations`` — PRD §13.1.
* ``POST /v1/invocations/<id>/cancel`` — PRD §13.6.
* ``DELETE /v1/invocations/<id>`` — an alias for the same cancellation.
* ``GET /healthz`` — operational convenience, no protocol meaning.
"""

from __future__ import annotations

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

from claude_code_bridge import claude_cli, mapping, workspace
from claude_code_bridge.async_runner import AsyncRunner
from claude_code_bridge.config import Config
from claude_code_bridge.idempotency import IdempotencyStore

logger = logging.getLogger("claude_code_bridge.server")

INVOCATIONS_PATH = "/v1/invocations"
_CANCEL_RE = re.compile(r"^/v1/invocations/([^/]+)/cancel$")
_ID_RE = re.compile(r"^/v1/invocations/([^/]+)$")

#: Bounds a request body the handler will read — an actor is entitled to
#: refuse an oversized request, never to let one exhaust the process's
#: memory.
MAX_BODY_BYTES = 8 * 1024 * 1024


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


class Bridge:
    """The long-lived state one bridge process holds: config, idempotency
    store, and the async invocation registry."""

    def __init__(self, cfg: Config) -> None:
        self.cfg = cfg
        self.idempotency = IdempotencyStore(cfg.state_dir)
        self.async_runner = AsyncRunner(cfg)


def decide_async(cfg: Config, *, force_async: bool | None, max_steps: int | None) -> bool:
    """Sync-vs-async dispatch policy — identical logic to
    `colleague_bridge.server.decide_async`: an always-async config flag wins
    outright; else the invocation's own `input.async` override wins; else
    the expected step budget is compared against `Config.sync_max_steps`.
    """
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
    server_version = "claude-code-bridge/0.1"
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
        if hmac.compare_digest(presented, token):
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
        # actually stopped.
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
        _transport_keys = {"instruction", "repo", "sandbox", "model", "success_outcome", "permission_mode"}
        _extras = {k: v for k, v in raw_input.items() if k not in _transport_keys}
        if _extras:
            _serialized = json.dumps(_extras, indent=2, ensure_ascii=False)
            if len(_serialized) > 60000:
                _serialized = _serialized[:60000] + "\n... [truncated at 60000 chars]"
            instruction = (
                instruction
                + "\n\n## Bound inputs (engine-resolved, verbatim)\n"
                + _serialized
            )

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

        role = raw_input.get("role") or None
        if role is not None and not claude_cli.role_is_known(resolved_repo, role):
            self._write_json(
                400,
                {
                    "error": (
                        f"role {role!r} is not a known claude-code agent "
                        f"(.claude/agents/{role}.md not found in the target repo)"
                    ),
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

        model = raw_input.get("model") or None
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
            self._dispatch_async(
                idem_key, body, ctx, instruction, resolved_repo, role, max_steps, model
            )
            return

        self._dispatch_sync(idem_key, ctx, instruction, resolved_repo, role, max_steps, model)

    def _dispatch_sync(
        self,
        idem_key: str,
        ctx: mapping.InvocationContext,
        instruction: str,
        repo: str,
        role: str | None,
        max_steps: int | None,
        model: str | None,
    ) -> None:
        cfg = self.bridge.cfg
        # t10: capture the workspace's starting point as close as possible
        # to the moment claude is actually spawned, so head_before/status
        # bracket the session rather than the whole request-handling ladder.
        handle = workspace.begin(repo)
        try:
            result = claude_cli.run_sync(
                cfg, instruction, repo, role=role, max_steps=max_steps, model=model
            )
        except (
            claude_cli.UnsupportedClaudeVersionError,
            claude_cli.ClaudeVersionProbeError,
        ) as exc:
            # The actor instance, as currently deployed, cannot serve this
            # dispatch — a version gate refusal is the actor being reachable
            # but not currently able to serve, i.e. actor_unavailable, never
            # a claim about the input itself.
            self._write_json(503, {"error": str(exc), "class": "actor_unavailable"})
            return
        response = mapping.sync_response(
            result.task_result,
            ctx,
            default_success_outcome=cfg.default_success_outcome,
            actor_id=cfg.actor_id,
            created_at=_now_iso(),
            timed_out=result.timed_out,
            workspace_measured=workspace.measure(handle),
        )
        # A real dispatch happened (claude was actually invoked) — durably
        # remember the outcome so a redelivered attempt replays it instead of
        # running claude a second time (PRD §20.3). A pre-dispatch
        # validation refusal (400/403/401, above) is deliberately NOT cached.
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
        role: str | None,
        max_steps: int | None,
        model: str | None,
    ) -> None:
        cfg = self.bridge.cfg
        callback = body.get("callback") or {}
        callback_url = callback.get("url") if isinstance(callback, dict) else None
        callback_token = callback.get("token") if isinstance(callback, dict) else None
        if not callback_url or not callback_token:
            self._write_json(
                400,
                {
                    "error": (
                        "callback.url and callback.token are required for an "
                        "asynchronous invocation"
                    ),
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return

        # t10: same bracketing as the sync path, captured right before the
        # detached claude subprocess is spawned.
        handle = workspace.begin(repo)
        try:
            start = claude_cli.spawn_background(
                cfg, instruction, repo, role=role, max_steps=max_steps, model=model
            )
        except (
            claude_cli.UnsupportedClaudeVersionError,
            claude_cli.ClaudeVersionProbeError,
        ) as exc:
            self._write_json(503, {"error": str(exc), "class": "actor_unavailable"})
            return
        except claude_cli.BackgroundDispatchError as exc:
            self._write_json(
                503,
                {
                    "error": str(exc),
                    "class": "actor_unavailable",
                    "detail": (exc.stderr or "")[-2000:],
                },
            )
            return

        accepted_body = {
            "invocation_id": start.handle_id,
            "heartbeat_after_seconds": cfg.heartbeat_after_seconds,
            "supports_cancellation": True,
        }
        self.bridge.idempotency.put(idem_key, 202, accepted_body, request_fingerprint=instruction)
        self._write_json(202, accepted_body)

        self.bridge.async_runner.start(
            start=start,
            ctx=ctx,
            callback_url=callback_url,
            callback_token=callback_token,
            heartbeat_after_seconds=cfg.heartbeat_after_seconds,
            workspace_handle=handle,
        )


_LOOPBACK_HOSTS = frozenset({"127.0.0.1", "localhost", "::1"})


def _refuse_unauthenticated_exposure(cfg: Config) -> None:
    """A bridge endpoint executes work on this machine: binding it beyond
    loopback with no auth token would be an unauthenticated remote-execution
    surface. Refuse at startup unless the operator opts in explicitly
    (CLAUDE_CODE_BRIDGE_ALLOW_UNAUTHENTICATED=1) — never by accident."""
    if cfg.auth_token or cfg.host in _LOOPBACK_HOSTS:
        return
    if os.environ.get("CLAUDE_CODE_BRIDGE_ALLOW_UNAUTHENTICATED") == "1":
        logger.warning(
            "serving on %s WITHOUT authentication " "(CLAUDE_CODE_BRIDGE_ALLOW_UNAUTHENTICATED=1)",
            cfg.host,
        )
        return
    raise SystemExit(
        f"refusing to bind {cfg.host} without an auth token: set "
        "CLAUDE_CODE_BRIDGE_AUTH_TOKEN, bind to loopback, or set "
        "CLAUDE_CODE_BRIDGE_ALLOW_UNAUTHENTICATED=1 to accept an "
        "unauthenticated non-loopback endpoint deliberately"
    )


def make_server(cfg: Config) -> BridgeHTTPServer:
    _refuse_unauthenticated_exposure(cfg)
    bridge = Bridge(cfg)
    return BridgeHTTPServer((cfg.host, cfg.port), Handler, bridge)


def serve_forever(cfg: Config) -> None:  # pragma: no cover - exercised via __main__, not unit tests
    server = make_server(cfg)
    logger.info("claude-code-bridge listening on http://%s:%d", cfg.host, server.server_address[1])
    try:
        server.serve_forever()
    finally:
        server.server_close()


def start_background(cfg: Config) -> tuple[BridgeHTTPServer, threading.Thread]:
    """Start the server on a daemon thread; returns `(server, thread)`.

    `server.server_address` reports the actual bound port when `cfg.port ==
    0` was used to pick a free one.
    """
    server = make_server(cfg)
    thread = threading.Thread(
        target=server.serve_forever,
        kwargs={"poll_interval": 0.05},
        name="claude-code-bridge-http",
        daemon=True,
    )
    thread.start()
    return server, thread
