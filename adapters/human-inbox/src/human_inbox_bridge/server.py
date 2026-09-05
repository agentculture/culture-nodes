"""The bridge's HTTP surface: PRD §13.1/§13.6 plus the human inbox.

Mirrors the sibling bridges' `server.py` route for route on the protocol
side — the actor protocol surface is backend-agnostic — and adds the human
surface the whole adapter exists for. See `adapters/colleague/src/
colleague_bridge/server.py`'s docstring for the rationale on the
deliberately single-threaded `http.server.HTTPServer` choice (this is a
REFERENCE adapter proving the actor protocol for `kind=human` actors, not a
production-scale actor host).

Protocol routes (what the culture-nodes worker talks to):

* ``POST /v1/invocations`` — PRD §13.1. ALWAYS answers 202 (§13.3): a
  human answers later or never, so there is no synchronous path. The task
  is durably parked before the 202 is written, the non-terminal
  ``accepted`` event is delivered through the callback, and then nothing
  runs — no poller, no heartbeats, no lease to hold. The 202 carries
  ``heartbeat_after_seconds: 0`` (no liveness promise; the worker treats 0
  as an open-ended wait).
* ``POST /v1/invocations/<id>/cancel`` — PRD §13.6.
* ``DELETE /v1/invocations/<id>`` — an alias for the same cancellation.
* ``GET /healthz`` — operational convenience, no protocol meaning.
* ``GET /identity`` — who this bridge serves, which durable store it owns,
  and the actor key its dial-in client presents. Not part of the actor
  protocol either: it exists so the co-located merge tracker can confirm it
  submits to the bridge whose store it reads, without any address at all
  (issue #72's guard, rebuilt for migration 0036 — see identity.py).

Human surface (same server, same bearer token):

* ``GET /inbox/tasks[?status=pending]`` — list tasks (callback credentials
  redacted).
* ``POST /inbox/tasks/<id>/submit`` — body ``{outcome, output?, note?, observed?}``;
  delivers the terminal ``completed`` event through the standard
  authenticated callback path and marks the task completed only when the
  delivery was accepted. A failed delivery leaves the task pending so the
  human can resubmit — a submission is never silently lost.
"""

from __future__ import annotations

import hmac
import json
import logging
import os
import re
import secrets
import threading
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any
from urllib.parse import parse_qs, urlsplit

from human_inbox_bridge import identity, mapping
from human_inbox_bridge.callbacks import CallbackConfig, CallbackEmitter
from human_inbox_bridge.config import Config
from human_inbox_bridge.idempotency import IdempotencyStore
from human_inbox_bridge.store import (
    STATUS_CANCELLED,
    STATUS_COMPLETED,
    STATUS_PENDING,
    HumanTask,
    TaskStore,
)

logger = logging.getLogger("human_inbox_bridge.server")

INVOCATIONS_PATH = "/v1/invocations"
INBOX_TASKS_PATH = "/inbox/tasks"
IDENTITY_PATH = "/identity"
_CANCEL_RE = re.compile(r"^/v1/invocations/([^/]+)/cancel$")
_ID_RE = re.compile(r"^/v1/invocations/([^/]+)$")
_SUBMIT_RE = re.compile(r"^/inbox/tasks/([^/]+)/submit$")

#: Bounds a request body the handler will read — an actor is entitled to
#: refuse an oversized request, never to let one exhaust the process's
#: memory.
MAX_BODY_BYTES = 8 * 1024 * 1024


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


class Bridge:
    """The long-lived state one bridge process holds: config, idempotency
    store, and the durable task store."""

    def __init__(self, cfg: Config) -> None:
        self.cfg = cfg
        # Minted before the stores so the co-located tracker's startup guard
        # has something to read the moment this process is listening (issue
        # #72's guard, rebuilt in task t7 — see identity.py).
        self.store_identity = identity.ensure_store_identity(cfg.state_dir)
        self.idempotency = IdempotencyStore(cfg.state_dir)
        self.tasks = TaskStore(cfg.state_dir)

    def emitter_for(self, task: HumanTask) -> CallbackEmitter:
        return CallbackEmitter(
            task.callback_url,
            task.callback_token,
            task.invocation_id,
            CallbackConfig(
                timeout_seconds=self.cfg.callback_timeout_seconds,
                max_retries=self.cfg.callback_max_retries,
                backoff_seconds=self.cfg.callback_retry_backoff_seconds,
            ),
        )


class BridgeHTTPServer(HTTPServer):
    def __init__(
        self, address: tuple[str, int], handler_cls: type[BaseHTTPRequestHandler], bridge: Bridge
    ) -> None:
        super().__init__(address, handler_cls)
        self.bridge = bridge


class Handler(BaseHTTPRequestHandler):
    server_version = "human-inbox-bridge/0.1"
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

    def _write_json(self, status: int, body: dict[str, Any], *, close: bool = False) -> None:
        payload = json.dumps(body).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
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
            if self._refuse_oversized_body():
                return
            split = urlsplit(self.path)
            if split.path == "/healthz":
                self._write_json(200, {"status": "ok"})
                return
            if split.path == IDENTITY_PATH:
                self._handle_identity()
                return
            if split.path == INBOX_TASKS_PATH:
                self._handle_inbox_list(split.query)
                return
            self._write_json(404, {"error": "not found"})
        except Exception:  # noqa: BLE001 - the server must answer, never crash
            logger.exception("unhandled error handling GET %s", self.path)
            self._write_json(
                500, {"error": "internal bridge error", "class": mapping.CLASS_EXECUTION}
            )

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
            m = _SUBMIT_RE.match(self.path)
            if m:
                self._handle_submit(m.group(1))
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

    # -- identity ----------------------------------------------------------

    def _handle_identity(self) -> None:
        """``GET /identity`` — who this bridge is, without an address.

        The co-located merge tracker's startup guard (issue #72) used to
        settle "is this the actor's bridge?" by comparing the actor's
        registered ``endpoint_ref`` against its own bridge URL. Migration
        0036 removes that column, so the question has to be answered by the
        bridge itself. Three facts do it, and none of them is an address:

        * ``actor_id`` — who this bridge serves as a ledger producer.
        * ``store_id`` — which durable state directory it owns. The tracker
          reads the same value off the local filesystem, so a match proves
          the process answering here is the one whose tasks it is reading.
          See identity.py for why that is the load-bearing half.
        * ``dial_in`` — the actor key its dial-in client presents, which is
          how the control plane decides where this actor's work goes.

        Behind the bridge token: the store id is not a capability on its
        own, but an unauthenticated reader must not be handed the one value
        a wrong bridge would need in order to look right.
        """
        if not self._require_auth():
            return
        dial_in_key = identity.dial_in_actor_key()
        self._write_json(
            200,
            {
                "actor_id": self.bridge.cfg.actor_id,
                "store_id": self.bridge.store_identity.store_id,
                "dial_in": {"configured": bool(dial_in_key), "actor_key": dial_in_key},
            },
        )

    # -- protocol routes ---------------------------------------------------

    def _handle_invocation(self) -> None:
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

        callback = body.get("callback") or {}
        callback_url = callback.get("url") if isinstance(callback, dict) else None
        callback_token = callback.get("token") if isinstance(callback, dict) else None
        if not callback_url or not callback_token:
            # This bridge is ALWAYS asynchronous — a human answers later or
            # never — so an invocation without a callback could never be
            # completed at all.
            self._write_json(
                400,
                {
                    "error": (
                        "callback.url and callback.token are required: this bridge always "
                        "answers asynchronously (a human completes the task later)"
                    ),
                    "class": mapping.CLASS_ACTOR_REJECTED_INPUT,
                },
            )
            return

        invocation_id = f"hit_{secrets.token_hex(8)}"
        task = HumanTask(
            invocation_id=invocation_id,
            status=STATUS_PENDING,
            created_at=_now_iso(),
            instruction=instruction,
            run_id=str(body.get("run_id") or ""),
            node_run_id=body.get("node_run_id") or None,
            attempt_id=body.get("attempt_id") or None,
            callback_url=str(callback_url),
            callback_token=str(callback_token),
            # `input.repository_identity` (task t2, issue #125) needs no
            # resolution here and gets none: this bridge has no
            # `repo_allowlist`, checks nothing out, and dispatches to a
            # PERSON rather than a session in a directory. It rides into
            # `extra_input` like every other non-instruction key, which is a
            # structured field the inbox returns verbatim and the tracker
            # reads by name — never prompt text appended to what the human is
            # asked to do, so the "Bound inputs" leak the three checkout
            # bridges exclude the key from cannot happen here.
            extra_input={k: v for k, v in raw_input.items() if k != "instruction"},
        )
        # Durably parked BEFORE the 202 is written: a crash after this line
        # loses nothing a restart cannot list and complete.
        self.bridge.tasks.save(task)

        accepted_body = {
            "invocation_id": invocation_id,
            "heartbeat_after_seconds": cfg.heartbeat_after_seconds,
            "supports_cancellation": True,
        }
        self.bridge.idempotency.put(idem_key, 202, accepted_body, request_fingerprint=instruction)
        self._write_json(202, accepted_body)

        # The non-terminal `accepted` event (§13.4 ordering: a terminal
        # event never arrives with no preceding non-terminal one). Delivered
        # off-thread so the 202 is never delayed by callback retries; its
        # sequence is reserved through the store, so a restart between this
        # and the eventual submission still yields a monotonic stream.
        threading.Thread(
            target=self._deliver_accepted,
            args=(task,),
            name=f"human-inbox-accepted-{invocation_id}",
            daemon=True,
        ).start()

    def _deliver_accepted(self, task: HumanTask) -> None:
        try:
            event_id, sequence = self.bridge.tasks.reserve_sequence(task.invocation_id)
            payload = {"invocation_id": task.invocation_id}
            if self.bridge.cfg.heartbeat_after_seconds > 0:
                payload["heartbeat_after_seconds"] = self.bridge.cfg.heartbeat_after_seconds
            self.bridge.emitter_for(task).resend(event_id, sequence, "accepted", payload)
        except Exception:  # noqa: BLE001 - a failed accepted delivery must not kill the thread
            logger.exception("accepted-event delivery for %s failed", task.invocation_id)

    def _handle_cancel(self, invocation_id: str) -> None:
        if not self._require_auth():
            return
        self._drain_body()  # an optional CancelRequest body; best-effort, unread fields ignored
        task = self.bridge.tasks.get(invocation_id)
        if task is not None and task.status == STATUS_PENDING:
            task.status = STATUS_CANCELLED
            task.cancelled_at = _now_iso()
            self.bridge.tasks.save(task)
        # PRD §13.6: cancellation is durable in Culture Nodes and best-effort
        # at the actor — the endpoint must answer, not prove the work
        # actually stopped.
        self._write_json(202, {"invocation_id": invocation_id, "status": "cancel-requested"})

    # -- human surface -----------------------------------------------------

    def _handle_inbox_list(self, query: str) -> None:
        if not self._require_auth():
            return
        params = parse_qs(query)
        status = (params.get("status") or [None])[0]
        tasks = self.bridge.tasks.list(status=status)
        self._write_json(200, {"tasks": [t.public_dict() for t in tasks]})

    def _handle_submit(self, invocation_id: str) -> None:
        if not self._require_auth():
            return
        task = self.bridge.tasks.get(invocation_id)
        if task is None:
            self._drain_body()
            self._write_json(404, {"error": f"no such task: {invocation_id}"})
            return
        if task.status != STATUS_PENDING:
            self._drain_body()
            self._write_json(
                409,
                {
                    "error": f"task {invocation_id} is {task.status}, not pending",
                    "status": task.status,
                },
            )
            return

        body = self._read_json_body()
        if body is None:
            return
        refusal = mapping.submission_error(body)
        if refusal is not None:
            self._write_json(400, {"error": refusal, "class": mapping.CLASS_ACTOR_REJECTED_INPUT})
            return

        ctx = mapping.InvocationContext(
            run_id=task.run_id, node_run_id=task.node_run_id, attempt_id=task.attempt_id
        )
        ev = mapping.completed_event(
            body, ctx, actor_id=self.bridge.cfg.actor_id, created_at=_now_iso()
        )
        event_id, sequence = self.bridge.tasks.reserve_sequence(invocation_id)
        delivered = self.bridge.emitter_for(task).resend(event_id, sequence, ev.kind, ev.payload)

        if not delivered:
            # The submission is NOT recorded as done: the task stays pending
            # so the human can resubmit once the control plane is reachable
            # again. (The consumed sequence number is lost, not reused —
            # monotonicity over density.)
            self._write_json(
                502,
                {
                    "error": (
                        "the completed event could not be delivered to the run's callback; "
                        "the task remains pending — submit again once the control plane is "
                        "reachable"
                    ),
                    "delivered": False,
                    "event_id": event_id,
                    "sequence": sequence,
                },
            )
            return

        # Reload rather than mutate the stale pre-reserve copy, then mark done.
        task = self.bridge.tasks.get(invocation_id) or task
        task.status = STATUS_COMPLETED
        task.completed_at = _now_iso()
        task.submission = {
            "outcome": str(body["outcome"]).strip(),
            "output": body.get("output") if isinstance(body.get("output"), dict) else {},
            "note": body.get("note"),
            "submitted_at": task.completed_at,
        }
        if "observed" in body:
            # Preserve the validated tracker marker in the durable audit
            # trail. Manual submissions omit it and retain their exact
            # historical stored shape.
            task.submission["observed"] = body["observed"]
        self.bridge.tasks.save(task)
        self._write_json(
            200,
            {
                "invocation_id": invocation_id,
                "status": STATUS_COMPLETED,
                "delivered": True,
                "event_id": event_id,
                "sequence": sequence,
            },
        )


_LOOPBACK_HOSTS = frozenset({"127.0.0.1", "localhost", "::1"})


def _refuse_unauthenticated_exposure(cfg: Config) -> None:
    """This endpoint accepts work into runs and completes attempts: binding
    it beyond loopback with no auth token would let anyone complete other
    people's node runs. Refuse at startup unless the operator opts in
    explicitly (HUMAN_INBOX_BRIDGE_ALLOW_UNAUTHENTICATED=1) — never by
    accident."""
    if cfg.auth_token or cfg.host in _LOOPBACK_HOSTS:
        return
    if os.environ.get("HUMAN_INBOX_BRIDGE_ALLOW_UNAUTHENTICATED") == "1":
        logger.warning(
            "serving on %s WITHOUT authentication (HUMAN_INBOX_BRIDGE_ALLOW_UNAUTHENTICATED=1)",
            cfg.host,
        )
        return
    raise SystemExit(
        f"refusing to bind {cfg.host} without an auth token: set "
        "HUMAN_INBOX_BRIDGE_AUTH_TOKEN, bind to loopback, or set "
        "HUMAN_INBOX_BRIDGE_ALLOW_UNAUTHENTICATED=1 to accept an "
        "unauthenticated non-loopback endpoint deliberately"
    )


def make_server(cfg: Config) -> BridgeHTTPServer:
    _refuse_unauthenticated_exposure(cfg)
    bridge = Bridge(cfg)
    return BridgeHTTPServer((cfg.host, cfg.port), Handler, bridge)


def serve_forever(cfg: Config) -> None:  # pragma: no cover - exercised via __main__, not unit tests
    server = make_server(cfg)
    logger.info("human-inbox-bridge listening on http://%s:%d", cfg.host, server.server_address[1])
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
        name="human-inbox-bridge-http",
        daemon=True,
    )
    thread.start()
    return server, thread
