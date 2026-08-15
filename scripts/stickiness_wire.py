"""Bridge transport for the stickiness A/B harness.

This module owns protocol I/O, including synchronous responses and asynchronous
callbacks. The experiment's records, statistics, arm execution, and report
rendering remain in stickiness_ab.py, so transport changes cannot obscure the
comparison logic they feed. The original module re-exports this public surface.
"""

from __future__ import annotations

import dataclasses
import http.server
import json
import queue
import threading
import time
import urllib.error
import urllib.request
import uuid
from typing import Any

PROTOCOL_VERSION = "1.0"

# --------------------------------------------------------------------------
# Wire client
# --------------------------------------------------------------------------


class BridgeError(Exception):
    """Raised when the bridge is unreachable or answers something the
    harness cannot parse as JSON — never raised for an ordinary domain
    failure (§13.5 execution/timeout/etc.), which is recorded as a failed
    `AttemptRecord` instead so one bad task doesn't abort the whole arm."""


@dataclasses.dataclass(frozen=True)
class InvocationOutcome:
    """One HTTP round trip's parsed result — the raw shape the wire
    returned, before `summarize` turns a list of these into an arm's
    aggregate numbers."""

    ok: bool
    status_code: int
    input_tokens: int | None
    cached_input_tokens: int | None
    thread_id: str | None
    continuation_ref: str | None
    error: str | None
    #: §13.2's own `usage.cost` (USD, when the actor prices its work — claude
    #: does; codex and colleague do not, per their bridges' own mapping.py).
    #: Not one of the task's five required columns, but already on the wire
    #: and, per task t7's own live run, the metric that actually SHOWS the
    #: resume effect when `uncached_input_tokens` cannot (see
    #: `CACHE_CONVENTION_ADDITIVE`'s docstring and the delivery artifact) —
    #: recorded alongside the required five for exactly that reason.
    cost_usd: float | None


class CallbackReceiver:
    """A minimal local §13.4 callback endpoint (stdlib ``http.server``, one
    per harness run) so `BridgeClient` can drive a bridge that dispatches
    asynchronously — whether because it decided to for this invocation, or
    because its config forces ``always_async: true`` regardless of what the
    request asked for, as the real deployed claude-code bridges do today.

    One receiver serves every invocation in a run, sequentially — this
    harness never has more than one invocation in flight at a time (both
    arms dispatch one task, wait for its outcome, then dispatch the next),
    so there is no need to correlate callback events by invocation id: the
    next terminal event this receiver sees IS the answer to the invocation
    the harness is currently waiting on.
    """

    def __init__(self) -> None:
        self.token = uuid.uuid4().hex
        self._queue: queue.Queue[dict[str, Any]] = queue.Queue()
        self._server = http.server.HTTPServer(("127.0.0.1", 0), self._build_handler())
        self._thread = threading.Thread(
            target=self._server.serve_forever, name="stickiness-ab-callback", daemon=True
        )
        self._thread.start()

    @property
    def url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host}:{port}/callback"

    def stop(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)

    def wait_for_terminal(self, timeout: float) -> tuple[str, dict[str, Any]]:
        """Block for the next ``completed``/``failed`` §13.4 event,
        discarding ``accepted``/``progress``/``heartbeat`` events along the
        way. Raises `TimeoutError` if none arrives within *timeout* seconds
        total (not per-event) — a bridge that heartbeats forever without
        ever finishing is exactly the case this must not wait through
        indefinitely.
        """
        deadline = time.monotonic() + timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("no terminal callback event arrived within the wait bound")
            try:
                event = self._queue.get(timeout=remaining)
            except queue.Empty:
                raise TimeoutError(
                    "no terminal callback event arrived within the wait bound"
                ) from None
            kind = event.get("kind")
            if kind in ("completed", "failed"):
                return kind, event.get("payload") or {}
            # accepted / progress / heartbeat — not the answer, keep waiting.

    def _build_handler(receiver):  # noqa: N805 - factory, not a bound method
        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, fmt, *args) -> None:  # noqa: A002 - stdlib signature
                pass  # keep test/operator output quiet; failures still raise

            def do_POST(self) -> None:  # noqa: N802 - stdlib naming
                length = int(self.headers.get("Content-Length", "0") or "0")
                raw = self.rfile.read(length) if length else b""
                header = self.headers.get("Authorization", "")
                presented = header[len("Bearer ") :] if header.startswith("Bearer ") else ""
                if presented != receiver.token:
                    self._reply(401, {"error": "bad or missing callback token"})
                    return
                try:
                    event = json.loads(raw) if raw else {}
                except ValueError:
                    self._reply(400, {"error": "callback body is not valid JSON"})
                    return
                receiver._queue.put(event)  # noqa: SLF001 - same module, intentional
                self._reply(200, {"status": "ok"})

            def _reply(self, status: int, body: dict[str, Any]) -> None:
                payload = json.dumps(body).encode("utf-8")
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

        return Handler


class BridgeClient:
    """A thin, dependency-free client for §13.1's invocation surface —
    sync or async, whichever the bridge picks. One instance per bridge base
    URL; pass a `CallbackReceiver` to let it follow a bridge that answers
    202 (its own decision, or `always_async` config)."""

    def __init__(
        self,
        base_url: str,
        *,
        auth_token: str | None = None,
        timeout: float = 600.0,
        callback: "CallbackReceiver | None" = None,
        async_wait_seconds: float = 180.0,
    ):
        self.base_url = base_url.rstrip("/")
        self.auth_token = auth_token
        self.timeout = timeout
        self.callback = callback
        self.async_wait_seconds = async_wait_seconds

    def invoke(
        self,
        *,
        run_id: str,
        instruction: str,
        repo: str,
        session_key: str | None = None,
        continuation_ref: str | None = None,
        model: str | None = None,
    ) -> InvocationOutcome:
        """POST one §13.1 invocation and return its terminal outcome,
        following an async 202 through this client's `CallbackReceiver`
        when the bridge chooses (or is configured) to answer that way.

        `continuation_ref` rides top-level in the body (ADR 0010 §1 — a
        sibling of run_id, not nested under `input`); `session_key` rides
        inside `input` (the transport field all three bridges exclude from
        Bound-inputs, task t5/t6). `input.async: false` is sent as a hint;
        a bridge configured `always_async` ignores it and answers 202
        regardless, which this method follows rather than treats as an
        error.
        """
        body: dict[str, Any] = {
            "protocol_version": PROTOCOL_VERSION,
            "run_id": run_id,
            "input": {
                "instruction": instruction,
                "repo": repo,
                "async": False,
            },
        }
        if session_key:
            body["input"]["session_key"] = session_key
        if model:
            body["input"]["model"] = model
        if continuation_ref:
            body["continuation_ref"] = continuation_ref
        if self.callback is not None:
            body["callback"] = {"url": self.callback.url, "token": self.callback.token}

        payload = json.dumps(body).encode("utf-8")
        req = urllib.request.Request(
            f"{self.base_url}/v1/invocations",
            data=payload,
            method="POST",
            headers={
                "Content-Type": "application/json",
                "Idempotency-Key": str(uuid.uuid4()),
            },
        )
        if self.auth_token:
            req.add_header("Authorization", f"Bearer {self.auth_token}")

        try:
            # nosec B310 - self.base_url is an operator-supplied CLI arg
            # (--base-url), naming the bridge to drive, not untrusted input;
            # the same pattern adapters/*/callbacks.py comments identically.
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:  # nosec B310
                status = resp.status
                raw = resp.read()
        except urllib.error.HTTPError as exc:
            status = exc.code
            raw = exc.read()
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise BridgeError(f"could not reach bridge at {self.base_url}: {exc}") from exc

        try:
            parsed = json.loads(raw) if raw else {}
        except ValueError as exc:
            raise BridgeError(f"bridge answered non-JSON body: {raw[:200]!r}") from exc

        if status == 202:
            if self.callback is None:
                raise BridgeError(
                    "bridge dispatched asynchronously (202) but this client has no "
                    "CallbackReceiver configured to follow it"
                )
            kind, event_payload = self.callback.wait_for_terminal(self.async_wait_seconds)
            return _outcome_from_terminal_event(kind, event_payload)

        return _outcome_from_response(status, parsed)


def _outcome_from_response(status: int, body: dict[str, Any]) -> InvocationOutcome:
    """Turn one parsed §13.2 synchronous response body (200, or a failure
    like 500/408) into an `InvocationOutcome`."""
    ok = status == 200 and "error" not in body
    error = None if ok else (body.get("error") or f"HTTP {status}")
    return _build_outcome(status_code=status, ok=ok, body=body, error=error)


def _outcome_from_terminal_event(kind: str, payload: dict[str, Any]) -> InvocationOutcome:
    """Turn one parsed §13.4 terminal callback payload (`kind` `completed`
    or `failed`) into the same `InvocationOutcome` shape `_outcome_from_response`
    builds for the synchronous path — so `run_cold_arm`/`run_warm_arm` never
    need to know which path a given task took.
    """
    ok = kind == "completed"
    error = None if ok else (payload.get("message") or payload.get("class") or f"callback:{kind}")
    return _build_outcome(status_code=200 if ok else 500, ok=ok, body=payload, error=error)


def _build_outcome(
    *, status_code: int, ok: bool, body: dict[str, Any], error: str | None
) -> InvocationOutcome:
    """Shared field extraction for both the synchronous response body and
    the async terminal callback payload — both carry `usage` and
    `continuation_ref` at the same shallow keys (ADR 0009/0010's whole
    point: an actor that finished late reports exactly the same shape as
    one that finished inline). Mirrors what every bridge's `mapping.py`
    already promises: `usage` rides on BOTH a success body/payload and,
    when the bridge managed to parse a terminal result before failing, the
    failure one too (issue #32) — so a failed task's real burned tokens
    still count toward the arm's totals instead of vanishing.
    """
    usage = body.get("usage") or {}
    input_tokens = usage.get("input_tokens")
    cached = usage.get("cached_input_tokens")
    thread_id = usage.get("thread_id")
    cost = usage.get("cost")
    continuation_ref = body.get("continuation_ref") if ok else None
    return InvocationOutcome(
        ok=ok,
        status_code=status_code,
        input_tokens=int(input_tokens) if isinstance(input_tokens, (int, float)) else None,
        cached_input_tokens=int(cached) if isinstance(cached, (int, float)) else None,
        thread_id=thread_id if isinstance(thread_id, str) and thread_id else None,
        continuation_ref=(
            continuation_ref if isinstance(continuation_ref, str) and continuation_ref else None
        ),
        cost_usd=float(cost) if isinstance(cost, (int, float)) else None,
        error=error,
    )
