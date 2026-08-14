"""The bridge's HTTP surface: PRD §13.1 / §13.6, answered fully
synchronously.

Mirrors the sibling bridges' `server.py` route for route on the protocol
side (see `adapters/colleague/src/colleague_bridge/server.py`'s docstring
for the rationale on the deliberately single-threaded
`http.server.HTTPServer` choice -- this is a REFERENCE adapter, not a
production-scale actor host), with one structural simplification: this
bridge never answers 202. `webhook.post` is a single call bounded to
`webhook.POST_TIMEOUT_SECONDS` (5s), so every invocation completes well
inside a synchronous response -- there is no callback surface, no
idempotency-replay-of-an-async-accept, no heartbeat, nothing that
outlives the request that triggered it.

Routes:

* ``POST /v1/invocations`` -- PRD §13.1. Always answers 200 (§13.2) with
  a synchronous `InvocationResult`, or a 4xx `actor_rejected_input`/
  `auth_or_policy` refusal for a malformed or unauthenticated request.
* ``POST /v1/invocations/<id>/cancel`` -- PRD §13.6. Always answers 202
  `cancel-requested`, matching the sibling bridges' convention: "answers
  success whether or not this invocation id is one the bridge still knows
  about." Since every invocation here is already finished by the time a
  response reaches the caller, there is never anything outstanding to
  cancel -- the endpoint exists so a generic actor client never has to
  special-case this bridge.
* ``DELETE /v1/invocations/<id>`` -- an alias for the same cancellation.
* ``GET /healthz`` -- operational convenience, no protocol meaning, the
  only unauthenticated route.
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
from typing import Any
from urllib.parse import urlsplit

from notify_bridge import mapping, payload
from notify_bridge.config import Config
from notify_bridge.idempotency import IdempotencyStore
from notify_bridge.webhook import post as webhook_post
from notify_bridge.webhook import resolve_webhook

logger = logging.getLogger("notify_bridge.server")

INVOCATIONS_PATH = "/v1/invocations"
_CANCEL_RE = re.compile(r"^/v1/invocations/([^/]+)/cancel$")
_ID_RE = re.compile(r"^/v1/invocations/([^/]+)$")

#: Bounds a request body the handler will read -- an actor is entitled to
#: refuse an oversized request, never to let one exhaust the process's
#: memory (mirrors every sibling bridge's own bound).
MAX_BODY_BYTES = 8 * 1024 * 1024

_SUPPORTED_PROTOCOL_VERSION = "1.0"


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


class Bridge:
    """The long-lived state one bridge process holds: config and the
    idempotency replay store. Nothing else -- there is no task store,
    because nothing this bridge does ever outlives one request."""

    def __init__(self, cfg: Config) -> None:
        self.cfg = cfg
        self.idempotency = IdempotencyStore(cfg.state_dir)


class BridgeHTTPServer(HTTPServer):
    def __init__(
        self, address: tuple[str, int], handler_cls: type[BaseHTTPRequestHandler], bridge: Bridge
    ) -> None:
        super().__init__(address, handler_cls)
        self.bridge = bridge


class Handler(BaseHTTPRequestHandler):
    server_version = "notify-bridge/0.1"
    protocol_version = "HTTP/1.1"

    # -- stdlib plumbing --------------------------------------------------

    def log_message(self, fmt: str, *args: Any) -> None:  # noqa: A002 - stdlib signature
        logger.info("%s - %s", self.address_string(), fmt % args)

    @property
    def bridge(self) -> Bridge:
        return self.server.bridge  # type: ignore[attr-defined]

    def _write_json(self, status: int, body: dict[str, Any], *, close: bool = False) -> None:
        raw = json.dumps(body).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        if close:
            self.send_header("Connection", "close")
        self.end_headers()
        try:
            self.wfile.write(raw)
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
        The declared body is drained in bounded chunks before the 413 so
        closing the socket cannot RST the response out of the client's
        receive buffer, and the next request on the connection is never
        desynchronized."""
        try:
            length = int(self.headers.get("Content-Length", "0") or "0")
        except ValueError:
            length = 0
        if length <= MAX_BODY_BYTES:
            return False
        remaining = length
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
            if self._refuse_oversized_body():
                return
            split = urlsplit(self.path)
            if split.path == "/healthz":
                self._write_json(200, {"status": "ok"})
                return
            self._write_json(404, {"error": "not found"})
        except Exception:  # noqa: BLE001 - the server must answer, never crash
            logger.exception("unhandled error handling GET %s", self.path)
            self._write_json(500, {"error": "internal bridge error", "class": "execution"})

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
            self._write_json(500, {"error": "internal bridge error", "class": "execution"})

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
            self._write_json(500, {"error": "internal bridge error", "class": "execution"})

    # -- protocol routes ---------------------------------------------------

    def _handle_invocation(self) -> None:  # noqa: C901 - one straight-line validation ladder
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

        if body.get("protocol_version") != _SUPPORTED_PROTOCOL_VERSION:
            got = body.get("protocol_version")
            self._write_json(
                400,
                {
                    "error": (
                        f"protocol_version {got!r} is not supported; "
                        f"this bridge speaks {_SUPPORTED_PROTOCOL_VERSION}"
                    ),
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return

        raw_input = body.get("input")
        raw_input = raw_input if isinstance(raw_input, dict) else {}
        parsed, refusal = mapping.parse_message(raw_input)
        if refusal is not None:
            self._write_json(400, {"error": refusal, "class": mapping.CLASS_ACTOR_REJECTED_INPUT})
            return
        assert parsed is not None  # mapping guarantees parsed xor refusal

        ctx = mapping.InvocationContext(
            run_id=str(body.get("run_id") or ""),
            node_run_id=body.get("node_run_id") or None,
            attempt_id=body.get("attempt_id") or None,
        )

        # The one place the URL is ever read: resolved fresh, held only in
        # this local, never logged, never returned in any response.
        raw_url, _enabled = resolve_webhook()
        message_body = payload.build_message(raw_url, parsed.message)
        post_result, status_code = webhook_post(raw_url, message_body)

        result_body = mapping.result_for(
            post_result,
            status_code,
            require_delivery=parsed.require_delivery,
            ctx=ctx,
            actor_id=self.bridge.cfg.actor_id,
            created_at=_now_iso(),
        )
        self.bridge.idempotency.put(idem_key, 200, result_body, request_fingerprint=idem_key)
        self._write_json(200, result_body)

    def _handle_cancel(self, invocation_id: str) -> None:
        if not self._require_auth():
            return
        self._drain_body()  # an optional CancelRequest body; unread fields ignored
        # PRD §13.6: cancellation is durable in Culture Nodes and
        # best-effort at the actor. Every invocation this bridge accepts is
        # already finished by the time its response reaches the caller, so
        # there is never anything outstanding to cancel -- answering
        # success unconditionally (the sibling bridges' own convention)
        # means a generic actor client never has to special-case that.
        self._write_json(202, {"invocation_id": invocation_id, "status": "cancel-requested"})


_LOOPBACK_HOSTS = frozenset({"127.0.0.1", "localhost", "::1"})


def _refuse_unauthenticated_exposure(cfg: Config) -> None:
    """This endpoint triggers a real Discord post: binding it beyond
    loopback with no auth token would let anyone spend the operator's
    webhook on their behalf. Refuse at startup unless the operator opts in
    explicitly (NOTIFY_BRIDGE_ALLOW_UNAUTHENTICATED=1) -- never by
    accident."""
    if cfg.auth_token or cfg.host in _LOOPBACK_HOSTS:
        return
    if os.environ.get("NOTIFY_BRIDGE_ALLOW_UNAUTHENTICATED") == "1":
        logger.warning(
            "serving on %s WITHOUT authentication (NOTIFY_BRIDGE_ALLOW_UNAUTHENTICATED=1)",
            cfg.host,
        )
        return
    raise SystemExit(
        f"refusing to bind {cfg.host} without an auth token: set "
        "NOTIFY_BRIDGE_AUTH_TOKEN, bind to loopback, or set "
        "NOTIFY_BRIDGE_ALLOW_UNAUTHENTICATED=1 to accept an unauthenticated "
        "non-loopback endpoint deliberately"
    )


def make_server(cfg: Config) -> BridgeHTTPServer:
    _refuse_unauthenticated_exposure(cfg)
    bridge = Bridge(cfg)
    return BridgeHTTPServer((cfg.host, cfg.port), Handler, bridge)


def serve_forever(cfg: Config) -> None:  # pragma: no cover - exercised via __main__, not unit tests
    server = make_server(cfg)
    logger.info("notify-bridge listening on http://%s:%d", cfg.host, server.server_address[1])
    try:
        server.serve_forever()
    finally:
        server.server_close()


def start_background(cfg: Config) -> tuple[BridgeHTTPServer, threading.Thread]:
    """Start the server on a daemon thread; returns `(server, thread)`.

    `server.server_address` reports the actual bound port when `cfg.port
    == 0` was used to pick a free one.
    """
    server = make_server(cfg)
    thread = threading.Thread(
        target=server.serve_forever,
        kwargs={"poll_interval": 0.05},
        name="notify-bridge-http",
        daemon=True,
    )
    thread.start()
    return server, thread
