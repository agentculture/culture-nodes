"""t6 (spec claims c44/h37): exactly one in-flight invocation per
session_key.

`test_concurrent_same_session_key_forks_the_second_invocation` is the one
acceptance-bearing test in this module: it drives TWO genuinely concurrent
async invocations against a real, live `BridgeHTTPServer` — the first is
made to sit "in flight" (`colleague_cli.is_pid_alive`/`read_background_
result` deliberately withheld until the test explicitly releases it) while
the second is dispatched, so the second's `session_registry.acquire()`
call provably races the first's still-open slot rather than merely running
after it completed.

Backend note: colleague never resumes a session at all (issue #62 — no
upstream resume verb; `mapping.py` always reports `continuation_ref:
null`), so this test cannot observe a discarded `continuation_ref` the way
the claude-code/codex bridges' equivalent tests do. It instead proves the
guard through the two channels `session_registry.py` documents as always
available regardless of backend: the `X-Session-Fork` response header and
the registry's own `fork_events` record.

Verified manually (per task t6's own acceptance bullet) that this test
goes red without the guard: with the `cfg.session_concurrency_enabled and
session_key` gate in `server.py::_handle_invocation` short-circuited to
never acquire, the second invocation's response never carries
`X-Session-Fork`, and no `ForkEvent` is ever recorded.
"""

from __future__ import annotations

import json
import threading
import time
import urllib.error
import urllib.request

import pytest
from colleague_bridge import colleague_cli, server
from colleague_bridge.config import Config

from ._fakes import FakeCallbackReceiver


def _invocation_body(repo, session_key, callback_url):
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
def concurrency_bridge(tmp_path):
    cfg = Config(
        repo_allowlist=(str(tmp_path),),
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
        sync_max_steps=6,
        default_max_steps=6,
    )
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    yield f"http://{host}:{port}", cfg, tmp_path, srv
    srv.shutdown()
    srv.server_close()


def test_concurrent_same_session_key_forks_the_second_invocation(concurrency_bridge, monkeypatch):
    base, cfg, repo, srv = concurrency_bridge
    receiver = FakeCallbackReceiver()
    try:
        calls: list[dict] = []
        calls_lock = threading.Lock()
        alive: dict[int, bool] = {}
        results: dict[str, dict | None] = {}
        first_dispatched = threading.Event()
        release_first = threading.Event()

        def fake_spawn_background(cfg_, instruction, repo_, *, role, max_steps, mode):
            with calls_lock:
                idx = len(calls)
                handle_id = f"bg_conc_{idx}"
                pid = 900000 + idx
                calls.append({"idx": idx, "handle_id": handle_id})

            if idx == 0:
                # The FIRST invocation: held "still running" (pid alive,
                # no result available) until the test explicitly lets it
                # finish — this is what makes the second invocation's
                # dispatch a genuine overlap instead of a lucky sequential
                # race.
                alive[pid] = True
                results[handle_id] = None
                first_dispatched.set()

                def _finish_first():
                    release_first.wait(timeout=5)
                    results[handle_id] = {
                        "task_id": handle_id,
                        "status": "ok",
                        "summary": "first done",
                        "changed_files": [],
                        "usage": {},
                    }
                    alive[pid] = False

                threading.Thread(target=_finish_first, daemon=True).start()
            else:
                # Any later invocation completes immediately — the test
                # only needs the FIRST one to still be in flight when the
                # SECOND one is dispatched.
                alive[pid] = False
                results[handle_id] = {
                    "task_id": handle_id,
                    "status": "ok",
                    "summary": "later done",
                    "changed_files": [],
                    "usage": {},
                }

            return colleague_cli.BackgroundStart(
                handle_id=handle_id,
                pid=pid,
                log_dir=f".colleague/background/{handle_id}/",
                flight=handle_id,
            )

        monkeypatch.setattr(colleague_cli, "spawn_background", fake_spawn_background)
        monkeypatch.setattr(colleague_cli, "is_pid_alive", lambda pid: alive.get(pid, False))
        monkeypatch.setattr(
            colleague_cli, "read_background_result", lambda repo_, handle_id: results.get(handle_id)
        )

        headers1 = {
            "Authorization": f"Bearer {cfg.auth_token}",
            "Idempotency-Key": "att_conc_1",
        }
        headers2 = {
            "Authorization": f"Bearer {cfg.auth_token}",
            "Idempotency-Key": "att_conc_2",
        }
        body1 = _invocation_body(str(repo), "sk-concurrent", receiver.url)
        body2 = _invocation_body(str(repo), "sk-concurrent", receiver.url)

        results_by_name: dict[str, tuple] = {}

        def _fire(name, body, headers):
            results_by_name[name] = _post(base, server.INVOCATIONS_PATH, body=body, headers=headers)

        t1 = threading.Thread(target=_fire, args=("first", body1, headers1))
        t1.start()
        # Wait until the first invocation has genuinely started (its
        # spawn_background ran and it is now "held" as in-flight) before
        # firing the second — the real-overlap guarantee: the second is
        # provably dispatched WHILE the first's session_key slot is still
        # occupied, not after it was released.
        assert first_dispatched.wait(timeout=5), "first invocation never started"
        t1.join(timeout=5)
        assert not release_first.is_set()  # sanity: first is still "running"

        status2, resp_headers2, _ = _post(
            base, server.INVOCATIONS_PATH, body=body2, headers=headers2
        )

        # Let the first invocation actually finish now.
        release_first.set()

        status1, resp_headers1, _ = results_by_name["first"]

        assert _wait_for_event_count(receiver, ("completed", "failed"), 2, timeout=10)

        # Both dispatches were accepted...
        assert status1 == 202
        assert status2 == 202
        assert len(calls) == 2

        # ...but the fork is observable on the wire, not merely inferred
        # after the fact (t6's own requirement) — never both responses
        # forked, never neither.
        assert resp_headers1.get("X-Session-Fork") is None
        assert resp_headers2.get("X-Session-Fork") == "1"

        # The registry itself recorded the collision (inspectable
        # directly, not just via the wire header/log line).
        assert len(srv.bridge.session_registry.fork_events) == 1
        assert srv.bridge.session_registry.fork_events[0].session_key == "sk-concurrent"
    finally:
        receiver.close()


def test_sequential_same_session_key_never_forks(concurrency_bridge, monkeypatch):
    """Contrast case: when the first invocation genuinely finishes before
    the second is even dispatched, there is no collision. Forking only
    ever fires on an actual overlap, never merely on a repeated
    session_key."""
    base, cfg, repo, srv = concurrency_bridge
    receiver = FakeCallbackReceiver()
    try:
        handle_counter = {"n": 0}

        def fake_spawn_background(cfg_, instruction, repo_, *, role, max_steps, mode):
            idx = handle_counter["n"]
            handle_counter["n"] += 1
            handle_id = f"bg_seq_{idx}"
            return colleague_cli.BackgroundStart(
                handle_id=handle_id,
                pid=800000 + idx,
                log_dir=".colleague/background/",
                flight=handle_id,
            )

        monkeypatch.setattr(colleague_cli, "spawn_background", fake_spawn_background)
        monkeypatch.setattr(colleague_cli, "is_pid_alive", lambda pid: False)
        monkeypatch.setattr(
            colleague_cli,
            "read_background_result",
            lambda repo_, handle_id: {
                "task_id": handle_id,
                "status": "ok",
                "summary": "done",
                "changed_files": [],
                "usage": {},
            },
        )

        headers1 = {
            "Authorization": f"Bearer {cfg.auth_token}",
            "Idempotency-Key": "att_seq_1",
        }
        headers2 = {
            "Authorization": f"Bearer {cfg.auth_token}",
            "Idempotency-Key": "att_seq_2",
        }
        body1 = _invocation_body(str(repo), "sk-sequential", receiver.url)
        body2 = _invocation_body(str(repo), "sk-sequential", receiver.url)

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
