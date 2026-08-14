"""Transport-layer tests, mirroring `internal/notify/webhook_test.go`'s
coverage: env resolution (primary/fallback/blank-counts-as-unset),
scheme/host classification, and `post()`'s behavior for disabled, success,
non-2xx, redirect, and unreachable cases -- all against a local HTTP
server, never a real network call."""

from __future__ import annotations

import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from notify_bridge import webhook

# -- resolve_webhook ----------------------------------------------------------


def test_resolve_webhook_disabled_when_neither_var_set():
    url, enabled = webhook.resolve_webhook(env={})
    assert enabled is False
    assert url == ""


def test_resolve_webhook_primary_wins():
    env = {
        webhook.ENV_PRIMARY: "https://discord.com/api/webhooks/1/aaa",
        webhook.ENV_FALLBACK: "https://discord.com/api/webhooks/2/bbb",
    }
    url, enabled = webhook.resolve_webhook(env=env)
    assert enabled is True
    assert url == "https://discord.com/api/webhooks/1/aaa"


def test_resolve_webhook_falls_back_when_primary_absent():
    env = {webhook.ENV_FALLBACK: "https://discord.com/api/webhooks/2/bbb"}
    url, enabled = webhook.resolve_webhook(env=env)
    assert enabled is True
    assert url == "https://discord.com/api/webhooks/2/bbb"


def test_resolve_webhook_blank_primary_falls_through_to_fallback():
    """An env file setting `CULTURE_NODES_WEBHOOK_URL=""` must not disable
    the webhook outright -- blank counts as unset, exactly like the var
    never being set at all (mirrors internal/notify/webhook.go's own
    documented behavior)."""
    env = {
        webhook.ENV_PRIMARY: "   ",
        webhook.ENV_FALLBACK: "https://discord.com/api/webhooks/2/bbb",
    }
    url, enabled = webhook.resolve_webhook(env=env)
    assert enabled is True
    assert url == "https://discord.com/api/webhooks/2/bbb"


def test_resolve_webhook_reads_process_environ_by_default(monkeypatch):
    monkeypatch.setenv(webhook.ENV_PRIMARY, "https://discord.com/api/webhooks/9/zzz")
    url, enabled = webhook.resolve_webhook()
    assert enabled is True
    assert url == "https://discord.com/api/webhooks/9/zzz"


# -- is_http_url / is_discord_url ---------------------------------------------


@pytest.mark.parametrize(
    "url,expected",
    [
        ("https://discord.com/api/webhooks/1/a", True),
        ("http://discord.com/api/webhooks/1/a", True),
        ("ftp://discord.com/api/webhooks/1/a", False),
        ("not a url at all", False),
        ("", False),
    ],
)
def test_is_http_url(url, expected):
    assert webhook.is_http_url(url) is expected


@pytest.mark.parametrize(
    "url,expected",
    [
        ("https://discord.com/api/webhooks/1/aaa", True),
        ("https://discordapp.com/api/webhooks/1/aaa", True),
        ("https://ptb.discord.com/api/webhooks/1/aaa", True),
        ("https://canary.discord.com/api/webhooks/1/aaa", True),
        # A bare discord.com link that is not a webhook path is not one.
        ("https://discord.com/channels/1/2", False),
        # discord-looking host but not in the sanctioned set.
        ("https://evil-discord.com/api/webhooks/1/aaa", False),
        ("https://example.com/api/webhooks/1/aaa", False),
    ],
)
def test_is_discord_url(url, expected):
    assert webhook.is_discord_url(url) is expected


# -- post() ---------------------------------------------------------------


class _FixedResponseHandler(BaseHTTPRequestHandler):
    status_code = 204

    def log_message(self, *a):  # noqa: D401 - quiet test output
        pass

    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length", "0") or "0")
        self.rfile.read(length)
        self.send_response(self.status_code)
        self.send_header("Content-Length", "0")
        self.end_headers()


def _serve(status_code: int):
    handler_cls = type("Handler", (_FixedResponseHandler,), {"status_code": status_code})
    server = HTTPServer(("127.0.0.1", 0), handler_cls)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    host, port = server.server_address
    return server, f"http://{host}:{port}/api/webhooks/1/token"


def test_post_disabled_when_url_blank():
    result, status = webhook.post("", b"{}")
    assert result is webhook.PostResult.DISABLED
    assert status is None


def test_post_disabled_when_url_all_whitespace():
    result, status = webhook.post("   ", b"{}")
    assert result is webhook.PostResult.DISABLED
    assert status is None


def test_post_failed_on_non_http_scheme():
    result, status = webhook.post("ftp://example.com/x", b"{}")
    assert result is webhook.PostResult.FAILED
    assert status is None


def test_post_posted_on_2xx():
    server, url = _serve(204)
    try:
        result, status = webhook.post(url, b'{"content":"hi"}')
        assert result is webhook.PostResult.POSTED
        assert status == 204
    finally:
        server.shutdown()
        server.server_close()


def test_post_failed_on_non_2xx():
    server, url = _serve(400)
    try:
        result, status = webhook.post(url, b"{}")
        assert result is webhook.PostResult.FAILED
        assert status == 400
    finally:
        server.shutdown()
        server.server_close()


def test_post_failed_on_5xx():
    server, url = _serve(503)
    try:
        result, status = webhook.post(url, b"{}")
        assert result is webhook.PostResult.FAILED
        assert status == 503
    finally:
        server.shutdown()
        server.server_close()


class _RedirectHandler(BaseHTTPRequestHandler):
    def log_message(self, *a):  # noqa: D401 - quiet test output
        pass

    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length", "0") or "0")
        self.rfile.read(length)
        self.send_response(302)
        self.send_header("Location", "http://127.0.0.1:1/elsewhere")
        self.send_header("Content-Length", "0")
        self.end_headers()


def test_post_never_follows_a_redirect():
    """A 3xx response must be treated exactly like any other non-2xx
    result -- never followed. Proven by pointing the Location at a
    deliberately dead address (127.0.0.1:1): if `post()` followed it, the
    call would fail with a connection-refused error instead of surfacing
    the 302 status."""
    server = HTTPServer(("127.0.0.1", 0), _RedirectHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    host, port = server.server_address
    url = f"http://{host}:{port}/api/webhooks/1/token"
    try:
        result, status = webhook.post(url, b"{}")
        assert result is webhook.PostResult.FAILED
        assert status == 302
    finally:
        server.shutdown()
        server.server_close()


def test_post_failed_on_connection_refused():
    # A dead loopback port: nothing is listening.
    result, status = webhook.post("http://127.0.0.1:1/api/webhooks/1/token", b"{}")
    assert result is webhook.PostResult.FAILED
    assert status is None
