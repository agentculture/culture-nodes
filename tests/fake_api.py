"""Fake Culture Nodes control-plane API server for tests.

Stdlib-only (``http.server``), matching the runtime package's zero-dependency
constraint. A test registers routes (method + path regex -> callback) before
issuing CLI calls against ``fake_api.base_url``; the callback gets the
handler (to write a response, including a streaming SSE one), the regex
match, the parsed query dict, and the raw request body bytes.
"""

from __future__ import annotations

import json
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit


class FakeNodesAPI:
    """A minimal, single-purpose stand-in for the Go ``nodes serve`` API."""

    def __init__(self) -> None:
        self.routes: list[tuple[str, re.Pattern, object]] = []
        self.requests: list[tuple[str, str, bytes]] = []
        self._server = ThreadingHTTPServer(("127.0.0.1", 0), self._build_handler())
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)

    def route(self, method: str, pattern: str, callback) -> None:
        """Register ``callback(handler, match, query, body)`` for ``method`` + path regex."""
        self.routes.append((method.upper(), re.compile(pattern), callback))

    @property
    def base_url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://127.0.0.1:{port}"

    def start(self) -> None:
        self._thread.start()

    def stop(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)

    def _build_handler(fake):  # noqa: N805 - factory, not a bound method
        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, fmt, *args):  # noqa: A003 - stdlib signature
                pass  # keep test output quiet

            def send_json(self, status: int, payload: object) -> None:
                data = json.dumps(payload).encode("utf-8")
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def start_sse(self) -> None:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                self.end_headers()

            def send_sse_frame(self, seq: int, event_type: str, data: object) -> None:
                body = json.dumps(data).encode("utf-8")
                frame = f"id: {seq}\nevent: {event_type}\ndata: ".encode("utf-8") + body + b"\n\n"
                self.wfile.write(frame)
                self.wfile.flush()

            def close_after_response(self) -> None:
                """Force the socket closed once this handler returns.

                Mirrors how the real API's SSE stream ends (the connection
                closes once a terminal event is sent, or the client
                disconnects) — our fake server has no long-poll loop, so it
                simply closes after writing whatever frames the test wants.
                """
                self.close_connection = True

            def _dispatch(self, method: str) -> None:
                parts = urlsplit(self.path)
                path = parts.path
                query = parse_qs(parts.query)
                length = int(self.headers.get("Content-Length", 0) or 0)
                body = self.rfile.read(length) if length else b""
                fake.requests.append((method, path, body))
                for route_method, regex, callback in fake.routes:
                    if route_method != method:
                        continue
                    match = regex.fullmatch(path)
                    if match:
                        callback(self, match, query, body)
                        return
                self.send_json(
                    404,
                    {
                        "code": 1,
                        "message": f"no fake route registered for {method} {path}",
                        "remediation": "register a route in the test before calling the CLI",
                    },
                )

            def do_GET(self) -> None:  # noqa: N802 - stdlib signature
                self._dispatch("GET")

            def do_POST(self) -> None:  # noqa: N802 - stdlib signature
                self._dispatch("POST")

            def do_PATCH(self) -> None:  # noqa: N802 - stdlib signature
                self._dispatch("PATCH")

        return Handler
