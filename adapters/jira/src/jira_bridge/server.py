"""Actor protocol HTTP surface for the narrow Jira verbs."""

from __future__ import annotations

import hmac
import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

from . import capabilities, client, create_issue, mapping, preflight, read_issue, transition_issue
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

    def _authorized(self) -> bool:
        expected = self.cfg.auth_token or ""
        if not expected:
            return True
        presented = self.headers.get("Authorization", "")
        return hmac.compare_digest(presented, f"Bearer {expected}")

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/healthz":
            self._json(200, {"status": "ok"})
        elif self.path == preflight.CAPABILITIES_PATH:
            # The advertisement names the verb surface AND the custody
            # configuration behind it (all non-secret): a reader learns not
            # just that create_issue exists but exactly which project keys
            # this deployment allows it to target (task t9). Authenticated
            # like invocations -- the config values are policy, not public.
            if not self._authorized():
                self._json(
                    401,
                    {"error": "a scoped workload token is required", "class": "auth_or_policy"},
                )
                return
            self._json(
                200,
                {
                    **preflight.capability_block(capabilities.host_facts(self.cfg)),
                    "verbs": [
                        mapping.VERB,
                        transition_issue.VERB,
                        create_issue.VERB,
                        read_issue.VERB,
                    ],
                    "custody": {
                        "transition_project_prefix": self.cfg.transition_project_prefix,
                        # Both names: `transition_target` is what this
                        # advertisement has published since the verb shipped
                        # and a reader may still be keyed on it;
                        # `transition_targets` is the whole allowlist, which
                        # is the fact since task t11 made it a list.
                        "transition_target": self.cfg.transition_target,
                        "transition_targets": list(self.cfg.transition_targets),
                        "create_projects": list(self.cfg.create_projects),
                        "read_project_prefix": self.cfg.transition_project_prefix,
                        "read_comment_limit": self.cfg.read_comment_limit,
                    },
                },
            )
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        # Defensive header parse BEFORE any read: a non-numeric value must be
        # a controlled 400, a negative one must never become read(-1) (which
        # blocks this single-threaded server until client EOF, pre-auth), and
        # the size cap applies before a byte is accepted (PR #180 review
        # finding).
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
        input_ = request.get("input") if isinstance(request, dict) else None
        if isinstance(input_, dict) and input_.get("verb") == transition_issue.VERB:
            parsed, refusal = transition_issue.parse(
                input_,
                project_prefix=self.cfg.transition_project_prefix,
                allowed_targets=self.cfg.transition_targets,
            )
        elif isinstance(input_, dict) and input_.get("verb") == create_issue.VERB:
            parsed, refusal = create_issue.parse(
                input_,
                allowed_projects=self.cfg.create_projects,
                allowed_issue_types=self.cfg.create_issue_types,
            )
        elif isinstance(input_, dict) and input_.get("verb") == read_issue.VERB:
            parsed, refusal = read_issue.parse(
                input_,
                project_prefix=self.cfg.transition_project_prefix,
                comment_limit=self.cfg.read_comment_limit,
            )
        else:
            parsed, refusal = mapping.parse(input_)
        if refusal:
            self._json(400, {"error": refusal, "class": "actor_rejected_input"})
            return
        email, token = os.environ.get("JIRA_ACCOUNT_EMAIL", ""), os.environ.get(
            "JIRA_API_TOKEN", ""
        )
        if not email or not token:
            self._json(
                500, {"error": "Jira actor credential is not configured", "class": "execution"}
            )
            return
        assert parsed is not None
        if isinstance(parsed, read_issue.ReadIssue):
            fetched = read_issue.read(
                self.cfg.jira_site, parsed, email, token, api_base=self.cfg.api_base
            )
            if not fetched.ok:
                self._json(502, {"error": fetched.error, "class": "execution"})
                return
            assert fetched.output is not None
            self._json(200, read_issue.result(fetched.output, self.cfg.actor_id))
            return
        if isinstance(parsed, create_issue.CreateIssue):
            created = create_issue.create(
                self.cfg.jira_site, parsed, email, token, api_base=self.cfg.api_base
            )
            if not created.ok:
                self._json(502, {"error": created.error, "class": "execution"})
                return
            self._json(200, create_issue.result(created.key, created.issue_id, self.cfg.actor_id))
            return
        if isinstance(parsed, transition_issue.Transition):
            posted = transition_issue.transition(
                self.cfg.jira_site,
                parsed.issue,
                parsed.target,
                email,
                token,
                api_base=self.cfg.api_base,
            )
            if not posted.ok:
                self._json(502, {"error": posted.error, "class": "execution"})
                return
            self._json(200, transition_issue.result(parsed.issue, parsed.target, self.cfg.actor_id))
            return
        posted = client.post_comment(
            self.cfg.jira_site,
            parsed.issue,
            parsed.marked_text,
            email,
            token,
            api_base=self.cfg.api_base,
        )
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
