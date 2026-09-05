"""The bounded oversized-body refusal, proven over a real socket (t1, #302).

Two facts no in-process handler test can show, because both are about the
connection rather than the response body:

* a declared body over ``MAX_BODY_BYTES`` is answered 413 and the socket is
  hung up, so the unread remainder can never be parsed as the *next*
  request on a keep-alive connection (the desync #302 item 1 names) — and
  the server is still serving afterwards, which a fresh connection proves;
* the drain that precedes the 413 stops at ``MAX_BODY_BYTES`` rather than at
  the declared length, so a client that declares far more than it sends
  still gets its 413 promptly instead of holding this single-threaded
  server reading. The refusal runs before auth (these requests carry no
  ``Authorization`` header at all), which is exactly why it must be bounded.
"""

from __future__ import annotations

import json
import socket
import time

import pytest

from claude_code_bridge import server
from claude_code_bridge.config import Config

#: Small enough that the tests move real bytes without moving 8 MiB.
CAP = 1024


@pytest.fixture()
def bridge(tmp_path, monkeypatch):
    monkeypatch.setattr(server, "MAX_BODY_BYTES", CAP)
    cfg = Config(
        repo_allowlist=(str(tmp_path),),
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
    )
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    yield host, port
    srv.shutdown()
    srv.server_close()


def _connect(address, timeout=5.0):
    sock = socket.create_connection(address, timeout=timeout)
    sock.settimeout(timeout)
    return sock


def _read_response(sock):
    """Read one HTTP response: status line, headers, then the declared body."""
    buf = b""
    while b"\r\n\r\n" not in buf:
        chunk = sock.recv(4096)
        if not chunk:
            break
        buf += chunk
    head, _, rest = buf.partition(b"\r\n\r\n")
    lines = head.decode("latin-1").split("\r\n")
    status = int(lines[0].split(" ")[1])
    headers = {}
    for line in lines[1:]:
        name, _, value = line.partition(":")
        headers[name.strip().lower()] = value.strip()
    length = int(headers.get("content-length", "0") or "0")
    while len(rest) < length:
        chunk = sock.recv(4096)
        if not chunk:
            break
        rest += chunk
    return status, headers, rest[:length]


def _post(sock, path, *, declared, body):
    request = (
        f"POST {path} HTTP/1.1\r\n"
        "Host: 127.0.0.1\r\n"
        "Content-Type: application/json\r\n"
        "Idempotency-Key: k\r\n"
        f"Content-Length: {declared}\r\n"
        "\r\n"
    ).encode("latin-1")
    sock.sendall(request + body)


def test_oversized_body_is_refused_and_the_connection_is_closed(bridge):
    sock = _connect(bridge)
    try:
        body = b"x" * (CAP * 4)
        _post(sock, server.INVOCATIONS_PATH, declared=len(body), body=body)
        status, headers, payload = _read_response(sock)
        assert status == 413
        assert headers.get("connection") == "close"
        assert json.loads(payload)["class"] == "actor_rejected_input"
    finally:
        sock.close()

    # The server survived the refusal and answers cleanly on a fresh
    # connection: nothing of the rejected body was left to be parsed as a
    # request, and the handler thread is not stuck.
    healthz = _connect(bridge)
    try:
        healthz.sendall(b"GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
        status, _headers, payload = _read_response(healthz)
        assert status == 200
        assert json.loads(payload)["status"] == "ok"
    finally:
        healthz.close()


def test_the_drain_stops_at_the_cap_instead_of_the_declared_length(bridge):
    """Declare ten times the cap, send the cap: the 413 still arrives."""
    sock = _connect(bridge, timeout=5.0)
    try:
        started = time.monotonic()
        _post(sock, server.INVOCATIONS_PATH, declared=CAP * 10, body=b"x" * CAP)
        status, headers, _payload = _read_response(sock)
        elapsed = time.monotonic() - started
        assert status == 413
        assert headers.get("connection") == "close"
        assert elapsed < 5.0
    finally:
        sock.close()
