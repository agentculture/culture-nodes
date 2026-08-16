"""t6 (spec claims c44/h37): exactly one in-flight invocation per
session_key.

`test_concurrent_same_session_key_forks_the_second_invocation` drives TWO
genuinely concurrent async invocations against a real, live
`BridgeHTTPServer`, spawning REAL subprocesses (a small fake `codex`
script, mirroring `conftest.py`'s `fake_codex` fixture) rather than
monkeypatching `codex_cli.spawn` itself out of the picture — the first
invocation's subprocess is made to hold ("FAKE_CODEX_HOLD=1", waiting on a
release file this test controls) until the second invocation has actually
been dispatched, so the second's `session_registry.acquire()` call
provably races the first's still-open slot instead of merely running
after it completed. `codex_cli.spawn` is wrapped (not replaced) purely to
record the `continuation_ref` each call actually receives, in call order.

Verified manually (per task t6's own acceptance bullet) that this test
goes red without the guard: with the `cfg.session_concurrency_enabled and
session_key` gate in `server.py::_handle_invocation` short-circuited to
never acquire, the second invocation's captured `continuation_ref` comes
back unmodified (the original ref) instead of `None`, and the
`X-Session-Fork` header never appears.
"""

from __future__ import annotations

import json
import stat
import threading
import time
import urllib.error
import urllib.request

import pytest
from codex_bridge import codex_cli, server
from codex_bridge.config import Config

from ._fakes import FakeCallbackReceiver

#: A fake `codex exec` that emits a normal ok transcript, but — when
#: FAKE_CODEX_HOLD=1 is set in its environment — blocks right after
#: `turn.started` until FAKE_CODEX_RELEASE_FILE appears. Mirrors
#: `conftest.py`'s `fake_codex` "ok" behavior in shape; the hold is new.
_FAKE_CODEX_HOLD_SCRIPT = """#!/usr/bin/env python3
import json
import os
import sys
import time


def emit(obj):
    print(json.dumps(obj), flush=True)


emit({"type": "thread.started", "thread_id": "fake-thread-" + os.environ.get("FAKE_CODEX_ID", "x")})
emit({"type": "turn.started"})

if os.environ.get("FAKE_CODEX_HOLD") == "1":
    release = os.environ["FAKE_CODEX_RELEASE_FILE"]
    deadline = time.time() + 10
    while not os.path.exists(release) and time.time() < deadline:
        time.sleep(0.02)

msg_item = {"id": "item_0", "type": "agent_message", "text": "OK"}
emit({"type": "item.completed", "item": msg_item})
emit({"type": "turn.completed", "usage": {"input_tokens": 1, "output_tokens": 1}})
sys.exit(0)
"""


@pytest.fixture()
def fake_codex_hold(tmp_path):
    script = tmp_path / "fake-codex-hold"
    script.write_text(_FAKE_CODEX_HOLD_SCRIPT, encoding="utf-8")
    script.chmod(script.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    return script


def _invocation_body(repo, session_key, continuation_ref, callback_url):
    return {
        "protocol_version": "1.0",
        "run_id": "run_1",
        "token_id": "tok_1",
        "node_run_id": "nr_1",
        "attempt_id": "att_1",
        "attempt": 1,
        "workflow": {"name": "wf", "version_digest": "sha256:0"},
        "node": {"id": "n1", "contract_digest": "sha256:1"},
        "input": {
            "instruction": "say hello",
            "repo": repo,
            "async": True,
            "session_key": session_key,
        },
        "continuation_ref": continuation_ref,
        "artifact_refs": [],
        "context_refs": [],
        "callback": {"url": callback_url, "token": "cbtok"},
    }


def _post(base, path, *, body, headers):
    req = urllib.request.Request(
        base + path,
        data=json.dumps(body).encode("utf-8"),
        method="POST",
        headers=headers,
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, resp.headers, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, exc.headers, json.loads(exc.read().decode("utf-8"))


def _wait_for_event_count(receiver, kinds, count, timeout=10.0):
    """Poll *receiver* until at least *count* events of one of *kinds* have
    landed. Distinct from `FakeCallbackReceiver.wait_for_kind`, which always
    returns the FIRST match — calling it twice in a row for two separate
    terminal events would just find the same one twice."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        with receiver._lock:  # noqa: SLF001 - test-only introspection
            seen = sum(1 for ev in receiver.events if ev["kind"] in kinds)
        if seen >= count:
            return True
        time.sleep(0.02)
    return False


@pytest.fixture()
def concurrency_bridge(tmp_path, fake_codex_hold):
    release_file = tmp_path / "release"
    cfg = Config(
        codex_bin=str(fake_codex_hold),
        codex_env={"FAKE_CODEX_RELEASE_FILE": str(release_file)},
        repo_allowlist=(str(tmp_path),),
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
        sync_max_steps=6,
        default_max_steps=6,
        async_wait_seconds=15.0,
    )
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    yield f"http://{host}:{port}", cfg, tmp_path, srv, release_file
    srv.shutdown()
    srv.server_close()


def test_concurrent_same_session_key_forks_the_second_invocation(concurrency_bridge, monkeypatch):
    base, cfg, repo, srv, release_file = concurrency_bridge
    receiver = FakeCallbackReceiver()
    try:
        calls: list[str | None] = []
        calls_lock = threading.Lock()
        real_spawn = codex_cli.spawn

        def spy_spawn(
            cfg_,
            instruction,
            repo_,
            *,
            model=None,
            sandbox=None,
            continuation_ref=None,
            writable_git=False,
        ):
            with calls_lock:
                idx = len(calls)
                calls.append(continuation_ref)
            # Only the FIRST spawn call holds — the env mutation is safe
            # without extra locking because each HTTP request handler runs
            # this synchronously to completion (Popen included) before the
            # next request is even fired by the test below.
            if idx == 0:
                cfg_.codex_env["FAKE_CODEX_HOLD"] = "1"
                cfg_.codex_env["FAKE_CODEX_ID"] = "first"
            else:
                cfg_.codex_env.pop("FAKE_CODEX_HOLD", None)
                cfg_.codex_env["FAKE_CODEX_ID"] = "second"
            return real_spawn(
                cfg_,
                instruction,
                repo_,
                model=model,
                sandbox=sandbox,
                continuation_ref=continuation_ref,
            )

        monkeypatch.setattr(codex_cli, "spawn", spy_spawn)

        headers1 = {
            "Authorization": f"Bearer {cfg.auth_token}",
            "Idempotency-Key": "att_conc_1",
        }
        headers2 = {
            "Authorization": f"Bearer {cfg.auth_token}",
            "Idempotency-Key": "att_conc_2",
        }
        body1 = _invocation_body(str(repo), "sk-concurrent", "sess-original", receiver.url)
        body2 = _invocation_body(str(repo), "sk-concurrent", "sess-original", receiver.url)

        status1, resp_headers1, _ = _post(
            base, server.INVOCATIONS_PATH, body=body1, headers=headers1
        )
        assert status1 == 202

        # Wait for the FIRST invocation's real subprocess to actually
        # reach its hold point (it emits turn.started, relayed as a
        # "progress" callback, immediately before blocking on the
        # release file) — this is the genuine-overlap guarantee: the
        # second request below is provably dispatched WHILE the
        # first's session_key slot is still occupied.
        assert _wait_for_event_count(receiver, ("progress",), 1, timeout=10)
        assert not release_file.exists()

        status2, resp_headers2, _ = _post(
            base, server.INVOCATIONS_PATH, body=body2, headers=headers2
        )
        assert status2 == 202

        # Let the first invocation's subprocess actually finish now.
        release_file.write_text("go")

        assert _wait_for_event_count(receiver, ("completed", "failed"), 2, timeout=10)

        # Exactly one of the two spawn calls carried the ORIGINAL
        # continuation_ref (the owner); the other was forked cold
        # (continuation_ref discarded) — never both, never neither, which
        # is what "never interleave turns on one provider thread" cashes
        # out to at the dispatch boundary.
        assert len(calls) == 2
        assert calls[0] == "sess-original"
        assert calls[1] is None

        # The fork is observable on the wire, not merely inferred from a
        # None continuation_ref after the fact (t6's own requirement).
        assert resp_headers1.get("X-Session-Fork") is None
        assert resp_headers2.get("X-Session-Fork") == "1"

        # The registry itself recorded the collision (inspectable
        # directly, not just via the wire header/log line).
        assert len(srv.bridge.session_registry.fork_events) == 1
        assert srv.bridge.session_registry.fork_events[0].session_key == "sk-concurrent"
    finally:
        receiver.close()


def test_sequential_same_session_key_never_forks(concurrency_bridge):
    """Contrast case: when the first invocation genuinely finishes before
    the second is even dispatched, there is no collision — the second
    keeps its own continuation_ref unmodified. Forking only ever fires on
    an actual overlap, never merely on a repeated session_key."""
    base, cfg, repo, srv, _release_file = concurrency_bridge
    receiver = FakeCallbackReceiver()
    try:
        headers1 = {
            "Authorization": f"Bearer {cfg.auth_token}",
            "Idempotency-Key": "att_seq_1",
        }
        headers2 = {
            "Authorization": f"Bearer {cfg.auth_token}",
            "Idempotency-Key": "att_seq_2",
        }
        body1 = _invocation_body(str(repo), "sk-sequential", "sess-a", receiver.url)
        body2 = _invocation_body(str(repo), "sk-sequential", "sess-b", receiver.url)

        status1, headers_1, _ = _post(base, server.INVOCATIONS_PATH, body=body1, headers=headers1)
        assert status1 == 202
        assert _wait_for_event_count(receiver, ("completed", "failed"), 1, timeout=10)

        status2, headers_2, _ = _post(base, server.INVOCATIONS_PATH, body=body2, headers=headers2)
        assert status2 == 202
        assert _wait_for_event_count(receiver, ("completed", "failed"), 2, timeout=10)

        assert headers_1.get("X-Session-Fork") is None
        assert headers_2.get("X-Session-Fork") is None
        assert srv.bridge.session_registry.fork_events == []
    finally:
        receiver.close()
