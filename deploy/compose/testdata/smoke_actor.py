#!/usr/bin/env python3
"""A minimal PRD §13 actor, for the local compose profile only (task t13).

Why this exists
---------------

`deploy/compose/otel-smoke.sh` has to produce a REAL run that reaches all
three seams `internal/telemetry` instruments — the worker's actor dispatch,
the actor callback ingest, and the engine's completion transaction — because
issue #5 closes on a live trace rather than on code. Reaching the callback
seam needs an actor that answers 202 and reports back; a workflow whose
`uses:` reference resolves to nothing never gets past dispatch.

It is deliberately the protocol and nothing else: no work, no state beyond an
idempotency map, no auth. The AUTHORITATIVE reference implementation is
`tests/conformance/reference.go`, which the conformance kit runs against on
every `go test ./...`; this is its throwaway sibling, small enough to read in
one screen and containerised so the compose network can reach it. Anything
that needs to be correct about the protocol belongs there, not here.

Local profile only
------------------

It accepts every invocation unauthenticated, so it is registered without an
`auth_token_env` and must never be exposed off the compose network. Real
adapters (`adapters/codex`, `adapters/claude-code`, `adapters/colleague`)
refuse a non-loopback bind without a token; this one has no token concept at
all, which is exactly why it stays in `deploy/compose/testdata/`.
"""

from __future__ import annotations

import json
import os
import threading
import urllib.error
import urllib.request
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("SMOKE_ACTOR_PORT", "8099"))

#: The domain outcome this actor always reports, and the output shape the
#: fixture workflow's node contract requires of it
#: (deploy/compose/testdata/smoke.workflow.yaml: outcomes.completed).
OUTCOME = "completed"

#: How long to wait before reporting. Long enough that the worker has parked
#: the attempt (`waiting_external`) before the callback arrives, which is the
#: ordering the async path is actually about.
REPORT_DELAY_SECONDS = float(os.environ.get("SMOKE_ACTOR_DELAY_SECONDS", "0.5"))


def _post_json(url: str, token: str, body: dict) -> tuple[int, str]:
    payload = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=payload,
        method="POST",
        headers={"content-type": "application/json", "authorization": f"Bearer {token}"},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode("utf-8", "replace")
    except OSError as exc:  # unreachable callback endpoint
        return 0, str(exc)


def _report(invocation: dict) -> None:
    """Deliver the §13.4 event stream for one invocation: `accepted`, then a
    terminal `completed`.

    Both events carry a stable event id and a monotonic sequence, because the
    control plane checks both: the id makes a redelivery recognisable, the
    sequence makes a reordering recognisable.
    """
    callback = invocation.get("callback") or {}
    url = callback.get("url")
    token = callback.get("token")
    if not url or not token:
        print("smoke-actor: invocation carried no callback url/token; nothing to report", flush=True)
        return

    attempt = invocation.get("attempt_id", "unknown")
    subject = ""
    raw_input = invocation.get("input")
    if isinstance(raw_input, dict):
        subject = str(raw_input.get("subject", ""))

    status, body = _post_json(
        url,
        token,
        {"event_id": f"{attempt}-accepted", "sequence": 1, "kind": "accepted"},
    )
    print(f"smoke-actor: accepted event -> {status} {body[:200]}", flush=True)

    threading.Event().wait(REPORT_DELAY_SECONDS)

    status, body = _post_json(
        url,
        token,
        {
            "event_id": f"{attempt}-completed",
            "sequence": 2,
            "kind": "completed",
            "payload": {
                "outcome": OUTCOME,
                "output": {"summary": f"smoke actor handled {subject or 'an empty subject'}"},
                "ledger_delta": None,
                "artifact_refs": [],
                "usage": None,
            },
        },
    )
    print(f"smoke-actor: completed event -> {status} {body[:200]}", flush=True)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _write(self, status: int, body: dict) -> None:
        payload = json.dumps(body).encode("utf-8")
        self.send_response(status)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self) -> None:  # noqa: N802 (BaseHTTPRequestHandler's casing)
        if self.path == "/healthz":
            self._write(200, {"status": "ok"})
            return
        self._write(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if not self.path.startswith("/v1/invocations"):
            self._write(404, {"error": "not found"})
            return
        length = int(self.headers.get("content-length") or 0)
        try:
            invocation = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError as exc:
            self._write(400, {"error": f"malformed invocation: {exc}", "class": "actor_rejected_input"})
            return

        invocation_id = str(uuid.uuid4())
        print(
            "smoke-actor: invocation run={} node_run={} attempt={}".format(
                invocation.get("run_id"), invocation.get("node_run_id"), invocation.get("attempt_id")
            ),
            flush=True,
        )
        # 202 first, report after: the worker parks the attempt rather than
        # holding a lease for the duration, and the callback seam only exists
        # because of that split.
        self._write(
            202,
            {
                "invocation_id": invocation_id,
                "heartbeat_after_seconds": 30,
                "supports_cancellation": False,
            },
        )
        threading.Thread(target=_report, args=(invocation,), daemon=True).start()

    def log_message(self, fmt: str, *args) -> None:
        print("smoke-actor: " + (fmt % args), flush=True)


def main() -> None:
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)  # noqa: S104 (compose network only)
    print(f"smoke-actor: listening on :{PORT}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
