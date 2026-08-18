"""Actor protocol HTTP surface for the single Jira verb."""

from __future__ import annotations

import hmac
import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

from . import client, mapping
from .config import Config

INVOCATIONS_PATH = "/v1/invocations"
MAX_BODY_BYTES = 1024 * 1024


class BridgeHTTPServer(HTTPServer):
    def __init__(self, address, handler, cfg: Config):
        super().__init__(address, handler)
        self.cfg = cfg


class Handler(BaseHTTPRequestHandler):
    server_version = "jira-bridge/0.1"

    @property
    def cfg(self) -> Config:
        return self.server.cfg  # type: ignore[attr-defined]

    def log_message(self, fmt: str, *args: Any) -> None:
        pass

    def _json(self, status: int, body: dict) -> None:
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/healthz":
            self._json(200, {"status": "ok"})
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        length = int(self.headers.get("Content-Length", "0") or "0")
        if length > MAX_BODY_BYTES:
            self._json(413, {"error": "request too large", "class": "actor_rejected_input"})
            return
        raw = self.rfile.read(length) if length else b""
        if self.path != INVOCATIONS_PATH:
            self._json(404, {"error": "not found"})
            return
        expected = self.cfg.auth_token or ""
        presented = self.headers.get("Authorization", "")
        if expected and not hmac.compare_digest(presented, f"Bearer {expected}"):
            self._json(401, {"error": "a scoped workload token is required", "class": "auth_or_policy"})
            return
        try:
            request = json.loads(raw)
        except ValueError:
            self._json(400, {"error": "request body is not valid JSON", "class": "actor_rejected_input"})
            return
        parsed, refusal = mapping.parse(request.get("input") if isinstance(request, dict) else None)
        if refusal:
            self._json(400, {"error": refusal, "class": "actor_rejected_input"})
            return
        email, token = os.environ.get("JIRA_ACCOUNT_EMAIL", ""), os.environ.get("JIRA_API_TOKEN", "")
        if not email or not token:
            self._json(500, {"error": "Jira actor credential is not configured", "class": "execution"})
            return
        assert parsed is not None
        posted = client.post_comment(self.cfg.jira_site, parsed.issue, parsed.text, email, token)
        if not posted.ok:
            self._json(502, {"error": posted.error, "class": "execution"})
            return
        self._json(200, mapping.result(parsed.issue, posted.comment_id, self.cfg.actor_id))


def make_server(cfg: Config) -> BridgeHTTPServer:
    if not cfg.auth_token and cfg.host not in {"127.0.0.1", "localhost", "::1"}:
        raise SystemExit("refusing unauthenticated non-loopback Jira bridge")
    return BridgeHTTPServer((cfg.host, cfg.port), Handler, cfg)


def serve_forever(cfg: Config) -> None:
    with make_server(cfg) as server:
        server.serve_forever()
