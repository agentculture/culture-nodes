"""Shared test doubles.

`FakeCallbackReceiver` is a tiny stdlib HTTP receiver, reused by the server
unit tests and the fake-subprocess integration tests so both speak exactly
the same fake §13.4 receiver shape — mirrors
`adapters/colleague/tests/_fakes.py::FakeCallbackReceiver` field for field
(protocol-level, not backend-specific, so there is nothing claude-specific
to change here).

`fake_claude_path()` resolves the fake `claude` binary
(`tests/fake_claude.py`) this whole suite dispatches against in place of the
real, billed, network-dependent CLI — see that script's own docstring for
why and what it understands.
"""

from __future__ import annotations

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path


def fake_claude_path() -> str:
    """A directly-executable `Config.claude_bin` value: `tests/fake_claude.py`,
    invoked via its own shebang + executable bit — the same way
    `claude_cli.py` invokes `cfg.claude_bin` as a single argv[0] it would
    use for the real `claude` binary."""
    return str(Path(__file__).parent / "fake_claude.py")


class FakeCallbackReceiver:
    """Records every §13.4 callback event POSTed to it, with an optional
    "refuse the next N deliveries of this event id" knob (mirrors
    `tests/conformance/receiver.go`'s `RefuseNextTerminal`, used to prove
    the bridge's own redelivery-with-the-same-identity behaviour)."""

    def __init__(self) -> None:
        self.events: list[dict] = []
        self.tokens: list[str] = []
        self._refuse_remaining: dict[str, int] = {}
        self._lock = threading.Lock()
        receiver = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):  # noqa: D401 - quiet test output
                pass

            def do_POST(self):  # noqa: N802
                length = int(self.headers.get("Content-Length", "0") or "0")
                body = self.rfile.read(length)
                event = json.loads(body)
                with receiver._lock:
                    receiver.tokens.append(self.headers.get("Authorization", ""))
                    remaining = receiver._refuse_remaining.get(event["event_id"], 0)
                    if remaining > 0:
                        receiver._refuse_remaining[event["event_id"]] = remaining - 1
                        self.send_response(503)
                        self.send_header("Content-Length", "0")
                        self.end_headers()
                        return
                    receiver.events.append(event)
                self.send_response(202)
                self.send_header("Content-Length", "0")
                self.end_headers()

        self._server = HTTPServer(("127.0.0.1", 0), Handler)
        self._thread = threading.Thread(
            target=self._server.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True
        )
        self._thread.start()

    @property
    def url(self) -> str:
        host, port = self._server.server_address
        return f"http://{host}:{port}/events"

    def refuse_next(self, event_id: str, n: int) -> None:
        with self._lock:
            self._refuse_remaining[event_id] = n

    def wait_for_kind(self, kind: str, timeout: float = 20.0) -> dict | None:
        return self.wait_for_any_kind((kind,), timeout=timeout)

    def wait_for_any_kind(self, kinds: tuple[str, ...], timeout: float = 20.0) -> dict | None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            with self._lock:
                for ev in self.events:
                    if ev["kind"] in kinds:
                        return ev
            time.sleep(0.02)
        return None

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()
