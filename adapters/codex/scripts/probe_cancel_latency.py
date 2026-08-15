#!/usr/bin/env python3
"""Measure where the §13.6 cancel path's latency actually goes (issue #21).

    cd adapters/codex && uv run python scripts/probe_cancel_latency.py

Needs no real `codex` binary and no network: everything runs against this
bridge's own HTTP surface on loopback, with a fake executable standing in for
the provider. It is committed rather than pasted into an issue so ADR 0013's
numbers can be re-derived — and re-challenged — on any host, including thor,
where the reported ~2.2s idle cancel was measured across the network.

Four numbers, in the order the argument needs them:

1. idle cancel, fresh connection each time (what internal/actors.Client does);
2. async dispatch to 202 — how long the single-threaded accept loop is held
   by the dispatch that starts a long session;
3. cancel MID-SESSION against that live async invocation, with a real
   subprocess running;
4. cancel arriving while an unrelated SYNCHRONOUS invocation occupies the
   server — the one case where single-threading really does bite.
"""

from __future__ import annotations

import json
import os
import stat
import statistics
import tempfile
import threading
import time
import urllib.request
from pathlib import Path

from codex_bridge import codex_cli, server
from codex_bridge.config import Config

AUTH = {"Authorization": "Bearer s3cr3t"}

FAKE_CODEX = """#!/usr/bin/env python3
import json, signal, sys, time
print(json.dumps({"type": "thread.started", "thread_id": "probe"}), flush=True)
print(json.dumps({"type": "turn.started"}), flush=True)
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
time.sleep(120)
"""


def _post(base, path, body=None, headers=None, timeout=60):
    data = json.dumps(body).encode() if body is not None else b""
    req = urllib.request.Request(
        base + path, data=data, method="POST", headers={**AUTH, **(headers or {})}
    )
    started = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            payload, code = resp.read(), resp.status
    except Exception as exc:  # noqa: BLE001 - a probe reports, it does not raise
        payload, code = b"", getattr(exc, "code", repr(exc))
    return time.monotonic() - started, code, payload


def _invocation(repo):
    return {
        "protocol_version": "1.0",
        "run_id": "probe",
        "token_id": "t",
        "node_run_id": "nr",
        "attempt_id": "a",
        "attempt": 1,
        "workflow": {"name": "wf", "version_digest": "sha256:0"},
        "node": {"id": "n1", "contract_digest": "sha256:1"},
        "input": {"instruction": "hi", "repo": str(repo)},
        "artifact_refs": [],
        "context_refs": [],
        "callback": {"url": "http://127.0.0.1:1/cb", "token": "c"},
    }


def _serve(cfg):
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    return srv, f"http://{host}:{port}"


def probe_async_paths(tmp: Path) -> None:
    """Rows 1-3: the paths a real cancel takes."""
    fake = tmp / "fake_codex"
    fake.write_text(FAKE_CODEX)
    fake.chmod(fake.stat().st_mode | stat.S_IEXEC)

    srv, base = _serve(
        Config(
            codex_bin=str(fake),
            repo_allowlist=(str(tmp),),
            state_dir=str(tmp / "state-async"),
            host="127.0.0.1",
            port=0,
            auth_token="s3cr3t",
            always_async=True,
            poll_interval_seconds=0.15,
            heartbeat_after_seconds=1,
            async_wait_seconds=300.0,
        )
    )
    try:
        idle = [_post(base, "/v1/invocations/inv_nonexistent/cancel")[0] for _ in range(20)]
        print(
            f"idle cancel  n=20  median={statistics.median(idle) * 1000:.2f}ms  "
            f"max={max(idle) * 1000:.2f}ms"
        )

        took, code, payload = _post(
            base, "/v1/invocations", _invocation(tmp), {"Idempotency-Key": "probe-async"}
        )
        print(f"async dispatch 202 in {took * 1000:.0f}ms (status {code})")
        invocation_id = json.loads(payload)["invocation_id"]
        time.sleep(1.0)  # let the subprocess get going

        mid = [_post(base, f"/v1/invocations/{invocation_id}/cancel")[0] for _ in range(10)]
        print(
            f"cancel MID-SESSION (async, real subprocess alive) n=10 "
            f"median={statistics.median(mid) * 1000:.2f}ms max={max(mid) * 1000:.2f}ms"
        )
    finally:
        srv.shutdown()


def probe_sync_serialization(tmp: Path) -> None:
    """Row 4: the case single-threading really does serialize."""
    started = threading.Event()

    def slow_run_sync(cfg_, instruction, repo_, **kwargs):
        started.set()
        time.sleep(3.0)
        return codex_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "task_id": "probe",
                "status": "ok",
                "summary": "s",
                "changed_files": [],
                "usage": {},
                "error": None,
            },
            timed_out=False,
        )

    original = codex_cli.run_sync
    codex_cli.run_sync = slow_run_sync
    srv, base = _serve(
        Config(
            repo_allowlist=(str(tmp),),
            state_dir=str(tmp / "state-sync"),
            host="127.0.0.1",
            port=0,
            auth_token="s3cr3t",
            sync_max_steps=6,
            default_max_steps=6,
        )
    )
    try:
        threading.Thread(
            target=_post,
            args=(base, "/v1/invocations", _invocation(tmp), {"Idempotency-Key": "probe-sync"}),
            daemon=True,
        ).start()
        started.wait(10)
        time.sleep(0.2)
        took, code, _ = _post(base, "/v1/invocations/inv_x/cancel")
        print(f"cancel during a 3.0s sync invocation: {took * 1000:.0f}ms (status {code})")
    finally:
        srv.shutdown()
        codex_cli.run_sync = original


def main() -> None:
    tmp = Path(tempfile.mkdtemp(prefix="probe21-"))
    os.makedirs(tmp, exist_ok=True)
    probe_async_paths(tmp)
    probe_sync_serialization(tmp)


if __name__ == "__main__":
    main()
