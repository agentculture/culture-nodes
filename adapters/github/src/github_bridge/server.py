"""Actor protocol HTTP surface for GitHub messaging."""

from __future__ import annotations

import hmac
import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

from . import capabilities, client, mapping, preflight, stamping
from .config import Config

INVOCATIONS_PATH = "/v1/invocations"
MAX_BODY_BYTES = 1024 * 1024


class BridgeHTTPServer(HTTPServer):
    def __init__(self, address, handler, cfg: Config):
        super().__init__(address, handler)
        self.cfg = cfg


class Handler(BaseHTTPRequestHandler):
    server_version = "github-bridge/0.1"

    @property
    def cfg(self) -> Config:
        return self.server.cfg  # type: ignore[attr-defined]

    def log_message(self, fmt: str, *args: Any) -> None:
        pass

    def _write_json(self, status: int, body: dict, *, close: bool = False) -> None:
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        if close:
            self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(raw)

    def _json(self, status: int, body: dict) -> None:
        self._write_json(status, body)

    def _refuse_oversized_body(self) -> bool:
        """Answer 413 + Connection: close when the declared body exceeds
        MAX_BODY_BYTES; True means the request was refused and handled.

        Truncating at the cap left the remainder on the keep-alive socket
        (request desync). Something is drained because a socket closed with
        bytes in flight can RST the 413 out of the client's receive buffer,
        but this runs BEFORE auth, so the drain is bounded twice: at most
        MAX_BODY_BYTES (not the declared length), and 2 s per read instead
        of the request's 30 s — a dripping client cannot hold the
        single-threaded server (pre-auth read amplification, slowloris).
        """
        try:
            length = int(self.headers.get("Content-Length", "0") or "0")
        except ValueError:
            length = 0
        if length <= MAX_BODY_BYTES:
            return False
        self.connection.settimeout(2.0)
        remaining = MAX_BODY_BYTES
        while remaining > 0:
            try:
                chunk = self.rfile.read(min(remaining, 65536))
            except OSError:
                break
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

    def _authorized(self) -> bool:
        expected = self.cfg.auth_token or ""
        return not expected or hmac.compare_digest(
            self.headers.get("Authorization", ""), f"Bearer {expected}"
        )

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/healthz":
            self._json(200, {"status": "ok"})
        elif self.path == preflight.CAPABILITIES_PATH:
            if not self._authorized():
                self._json(
                    401, {"error": "a scoped workload token is required", "class": "auth_or_policy"}
                )
                return
            self._json(
                200,
                {
                    **preflight.capability_block(capabilities.host_facts(self.cfg)),
                    "stamping": {"marker": "cn1", "version": 1},
                    "verbs": list(mapping.VERBS),
                    "custody": {"repositories": list(self.cfg.repositories)},
                },
            )
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if self._refuse_oversized_body():
            return
        try:
            length = int(self.headers.get("Content-Length", "0") or "0")
        except ValueError:
            self._json(
                400, {"error": "Content-Length is not an integer", "class": "actor_rejected_input"}
            )
            return
        if length < 0:
            self._json(
                400, {"error": "Content-Length is negative", "class": "actor_rejected_input"}
            )
            return
        if length > MAX_BODY_BYTES:
            self._json(413, {"error": "request too large", "class": "actor_rejected_input"})
            return
        raw = self.rfile.read(length) if length else b""
        if self.path != INVOCATIONS_PATH:
            self._json(404, {"error": "not found"})
            return
        if not self._authorized():
            self._json(
                401, {"error": "a scoped workload token is required", "class": "auth_or_policy"}
            )
            return
        try:
            request = json.loads(raw)
        except ValueError:
            self._json(
                400, {"error": "request body is not valid JSON", "class": "actor_rejected_input"}
            )
            return
        raw_input = request.get("input") if isinstance(request, dict) else None
        marker = raw_input.get("marker") if isinstance(raw_input, dict) else None
        if marker is not None:
            try:
                stamping.validate_marker(marker)
            except ValueError:
                self._json(400, {"error": "invalid cn1 marker", "class": "actor_rejected_input"})
                return
            raw_input = {key: value for key, value in raw_input.items() if key != "marker"}
        parsed, refusal = mapping.parse(
            raw_input,
            self.cfg.repositories,
        )
        if refusal:
            self._json(400, {"error": refusal, "class": "actor_rejected_input"})
            return
        token = os.environ.get("GITHUB_TOKEN", "")
        if not token:
            self._json(
                500, {"error": "GitHub actor credential is not configured", "class": "execution"}
            )
            return
        assert parsed is not None
        comment_text = (
            stamping.stamp_text(parsed.comment, marker) if marker is not None else parsed.comment
        )
        if parsed.verb == "post_comment":
            posted = client.post_comment(parsed.repository, parsed.number, comment_text, token)
        else:
            assert parsed.comment_id is not None
            posted = client.reply_to_review_thread(
                parsed.repository, parsed.number, parsed.comment_id, comment_text, token
            )
        if not posted.ok:
            self._json(502, {"error": posted.error, "class": "execution"})
            return
        result = mapping.result(
            parsed.verb, parsed.repository, parsed.number, posted.comment_id, self.cfg.actor_id
        )
        if marker is not None:
            result["output"].update(stamping.artifact_result(posted.comment_id, marker))
        self._json(200, result)

    def do_DELETE(self) -> None:  # noqa: N802
        if self._refuse_oversized_body():
            return
        self._json(405, {"error": "method not allowed"})


def make_server(cfg: Config) -> BridgeHTTPServer:
    if not cfg.auth_token and cfg.host not in {"127.0.0.1", "localhost", "::1"}:
        raise SystemExit("refusing unauthenticated non-loopback GitHub bridge")
    return BridgeHTTPServer((cfg.host, cfg.port), Handler, cfg)


def serve_forever(cfg: Config) -> None:
    from . import dialin

    with make_server(cfg) as server:
        dialin.start("GITHUB_BRIDGE", server.server_port)
        server.serve_forever()
