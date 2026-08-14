"""t6 (spec claims c44/h37): exactly one in-flight invocation per
session_key.

`test_concurrent_same_session_key_forks_the_second_invocation` is the one
acceptance-bearing test in this module: it drives TWO genuinely concurrent
async invocations against a real, live `BridgeHTTPServer` — the first is
made to sit "in flight" (its fake `claude_cli.spawn_background` result is
deliberately withheld until the test explicitly releases it) while the
second is dispatched, so the second's `session_registry.acquire()` call
provably races the first's still-open slot rather than merely running
after it completed. This is checked by asserting real wall-clock overlap
(the second response returns before the first's simulated session is
allowed to finish), not inferred from a comment.

Verified manually (per task t6's own acceptance bullet) that this test
goes red without the guard: with the `cfg.session_concurrency_enabled and
session_key` gate in `server.py::_handle_invocation` short-circuited to
never acquire, the second invocation's captured `continuation_ref` comes
back unmodified (the original ref) instead of `None`, and the
`X-Session-Fork` header never appears — the assertions below fail exactly
where the guard would have prevented interleaving.
"""

from __future__ import annotations

import json
import threading
import time
import urllib.error
import urllib.request

import pytest

from claude_code_bridge import claude_cli, flightfiles, server
from claude_code_bridge.config import Config

from ._fakes import FakeCallbackReceiver


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
        first_dispatched = threading.Event()
        release_first = threading.Event()

        def fake_spawn_background(
            cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None
        ):
            with calls_lock:
                idx = len(calls)
                handle_id = f"cc_conc_{idx}"
                pid = 900000 + idx
                calls.append(
                    {"idx": idx, "handle_id": handle_id, "continuation_ref": continuation_ref}
                )
            feed = flightfiles.feed_path(cfg_.state_dir, handle_id)
            feed.parent.mkdir(parents=True, exist_ok=True)

            if idx == 0:
                # The FIRST invocation: held "still running" (pid alive,
                # no terminal result on the flight feed) until the test
                # explicitly lets it finish — this is what makes the
                # second invocation's dispatch a genuine overlap instead
                # of a lucky sequential race.
                alive[pid] = True
                first_dispatched.set()

                def _finish_first():
                    release_first.wait(timeout=5)
                    feed.write_text(
                        json.dumps(
                            {
                                "type": "result",
                                "subtype": "success",
                                "is_error": False,
                                "session_id": handle_id,
                                "result": "first done",
                            }
                        )
                        + "\n"
                    )
                    alive[pid] = False

                threading.Thread(target=_finish_first, daemon=True).start()
            else:
                # Any later invocation completes immediately — the test
                # only needs the FIRST one to still be in flight when the
                # SECOND one is dispatched.
                alive[pid] = False
                feed.write_text(
                    json.dumps(
                        {
                            "type": "result",
                            "subtype": "success",
                            "is_error": False,
                            "session_id": handle_id,
                            "result": "later done",
                        }
                    )
                    + "\n"
                )

            return claude_cli.BackgroundStart(handle_id=handle_id, pid=pid, log_path=str(feed))

        monkeypatch.setattr(claude_cli, "spawn_background", fake_spawn_background)
        monkeypatch.setattr(claude_cli, "is_pid_alive", lambda pid: alive.get(pid, False))

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

        results: dict[str, tuple] = {}

        def _fire(name, body, headers):
            results[name] = _post(base, server.INVOCATIONS_PATH, body=body, headers=headers)

        t1 = threading.Thread(target=_fire, args=("first", body1, headers1))
        t1.start()
        # Wait until the first invocation has genuinely started (its
        # spawn_background ran and it is now "held" as in-flight) before
        # firing the second — this is the real-overlap guarantee: the
        # second is provably dispatched WHILE the first's session_key slot
        # is still occupied, not after it was released.
        assert first_dispatched.wait(timeout=5), "first invocation never started"
        t1.join(timeout=5)
        assert not release_first.is_set()  # sanity: first is still "running"

        status2, resp_headers2, body2_resp = _post(
            base, server.INVOCATIONS_PATH, body=body2, headers=headers2
        )

        # Let the first invocation actually finish now.
        release_first.set()

        status1, resp_headers1, body1_resp = results["first"]

        assert _wait_for_event_count(receiver, ("completed", "failed"), 2, timeout=10)

        # Both dispatches were accepted...
        assert status1 == 202
        assert status2 == 202

        # ...but exactly one of the two calls to spawn_background carried
        # the ORIGINAL continuation_ref (the owner), and the other was
        # forked cold (continuation_ref discarded) — never both, and
        # never the same one twice, which is what "never interleave turns
        # on one provider thread" cashes out to at the dispatch boundary.
        assert len(calls) == 2
        refs = sorted((c["continuation_ref"] for c in calls), key=lambda r: r or "")
        assert refs == [None, "sess-original"]

        # The fork is observable on the wire, not merely inferred from a
        # None continuation_ref after the fact (t6's own requirement).
        forked_call = next(c for c in calls if c["continuation_ref"] is None)
        forked_headers = resp_headers1 if forked_call["idx"] == 0 else resp_headers2
        owner_headers = resp_headers2 if forked_call["idx"] == 0 else resp_headers1
        assert forked_headers.get("X-Session-Fork") == "1"
        assert owner_headers.get("X-Session-Fork") is None

        # The registry itself recorded the collision (inspectable
        # directly, not just via the wire header/log line).
        assert len(srv.bridge.session_registry.fork_events) == 1
        event = srv.bridge.session_registry.fork_events[0]
        assert event.session_key == "sk-concurrent"
    finally:
        receiver.close()


def test_sequential_same_session_key_never_forks(concurrency_bridge, monkeypatch):
    """Contrast case: when the first invocation genuinely finishes before
    the second is even dispatched, there is no collision — the second
    keeps its own continuation_ref unmodified. Forking only ever fires on
    an actual overlap, never merely on a repeated session_key."""
    base, cfg, repo, srv = concurrency_bridge
    receiver = FakeCallbackReceiver()
    try:
        handle_counter = {"n": 0}

        def fake_spawn_background(
            cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None
        ):
            idx = handle_counter["n"]
            handle_counter["n"] += 1
            handle_id = f"cc_seq_{idx}"
            feed = flightfiles.feed_path(cfg_.state_dir, handle_id)
            feed.parent.mkdir(parents=True, exist_ok=True)
            feed.write_text(
                json.dumps(
                    {
                        "type": "result",
                        "subtype": "success",
                        "is_error": False,
                        "session_id": handle_id,
                        "result": "done",
                        "continuation_ref_seen": continuation_ref,
                    }
                )
                + "\n"
            )
            return claude_cli.BackgroundStart(
                handle_id=handle_id, pid=800000 + idx, log_path=str(feed)
            )

        monkeypatch.setattr(claude_cli, "spawn_background", fake_spawn_background)
        monkeypatch.setattr(claude_cli, "is_pid_alive", lambda pid: False)

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
        assert _wait_for_event_count(receiver, ("completed", "failed"), 1, timeout=10)
        status2, headers_2, _ = _post(base, server.INVOCATIONS_PATH, body=body2, headers=headers2)
        assert _wait_for_event_count(receiver, ("completed", "failed"), 2, timeout=10)

        assert status1 == 202
        assert status2 == 202
        assert headers_1.get("X-Session-Fork") is None
        assert headers_2.get("X-Session-Fork") is None
        assert srv.bridge.session_registry.fork_events == []
    finally:
        receiver.close()
