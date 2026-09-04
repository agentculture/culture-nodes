#!/usr/bin/env python3
"""The h14 named probe: does a qwen session survive the death of its
process, and can a fresh process session/load the recorded sessionId?

Plan qwen-bridge-acp, task t2. h14 is a CLAIM-SIDE VERIFICATION, not a
plan target (spec: "the h14 named probe (kill process, session/load the
recorded sessionId) is a committed script whose result is recorded"),
and continuation_ref is NOT implemented in the first cut (frame park
v2) - nothing in the dispatch path consumes this probe's answer; it
measures the backend's durable-session behavior so the claim has a
recorded fact behind it.

The probe speaks ACP directly (a small stdlib JSON-RPC client),
because its sequence is not the bridge's dispatch sequence:

  generation 1:  initialize -> session/new -> record the sessionId
                 -> SIGKILL the agent process (the process that created
                 the session dies; on-disk state, if any, is the agent's)
  generation 2:  initialize -> session/load {sessionId}
                 -> record the response

Backends (the probe reports which one it used, in the recorded result):

  * real - the ACTUAL `qwen --acp` binary, located by the seam's own
    probe (qwen_cli.locate_qwen_bin: the KNOWN INSTALL PATHS, never the
    non-interactive PATH). This is the measurement h14 asks for;
    anything the real backend does - including refusing, timing out,
    or not surviving the kill - is the recorded result.
  * fake - the committed fake ACP agent
    (tests/fixtures/acp/fake_acp_agent.py, FAKE_ACP_BEHAVIOR=load):
    session/new persists a marker file and a FRESH process can
    session/load the same sessionId from it. This exercises the probe's
    own machinery end-to-end when a real qwen is unavailable (no
    binary, no importable seam package, spawn blocked) - it is a
    mechanism check, NOT the durable-state measurement, and the
    recorded result says so plainly.

The recorded result is written next to this script
(session_load_probe_result.json) and committed with it: the fact is
that the probe was RUN and what it found, on which host and backend,
when - not any expectation about the answer.

Usage:
    uv run --project adapters/qwen python adapters/qwen/scripts/session_load_probe.py \
        [--backend auto|real|fake] [--timeout SECONDS]
"""

from __future__ import annotations

import argparse
import json
import os
import queue
import subprocess
import sys
import tempfile
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

HERE = Path(__file__).resolve().parent
RESULT_PATH = HERE / "session_load_probe_result.json"
FAKE_AGENT = HERE.parent / "tests" / "fixtures" / "acp" / "fake_acp_agent.py"


# ---------------------------------------------------------------------------
# a small stdio JSON-RPC client (stdlib only; agent requests answered
# leniently the way the seam's first cut answers them)
# ---------------------------------------------------------------------------


class JsonRpc:
    def __init__(self, proc, *, timeout: float):
        self._proc = proc
        self._timeout = timeout
        self._id = 0
        self._queue: queue.Queue = queue.Queue()
        threading.Thread(target=self._reader, daemon=True).start()

    def _reader(self) -> None:
        assert self._proc.stdout is not None
        for line in self._proc.stdout:
            self._queue.put(line)

    def _send(self, obj: dict) -> None:
        assert self._proc.stdin is not None
        self._proc.stdin.write(json.dumps(obj) + "\n")
        self._proc.stdin.flush()

    def request(self, method: str, params: dict) -> dict:
        """Send a request and wait for ITS response, skipping
        session/update notifications and answering agent-to-client
        requests leniently ({} - the seam's v1 unknown-ext behavior).
        Returns the response object; raises TimeoutError when the
        deadline passes first."""
        self._id += 1
        rid = self._id
        self._send({"jsonrpc": "2.0", "id": rid, "method": method, "params": params})
        deadline = time.monotonic() + self._timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError(f"{method} timed out after {self._timeout:.0f}s")
            try:
                line = self._queue.get(timeout=remaining)
            except queue.Empty:
                if self._proc.poll() is not None:
                    code = self._proc.returncode
                    raise TimeoutError(
                        f"{method}: the agent process exited (code {code}) before answering"
                    )
                raise
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except ValueError:
                continue
            if not isinstance(obj, dict):
                continue
            if obj.get("id") == rid and ("result" in obj or "error" in obj):
                return obj
            if obj.get("method") and "id" in obj and "result" not in obj and "error" not in obj:
                # an agent-to-client request (the seam's lenient answer)
                self._send({"jsonrpc": "2.0", "id": obj["id"], "result": {}})
            # notifications (session/update): skipped here, not recorded -
            # the probe's question is the load, not the turn stream

    def kill(self) -> int:
        """SIGKILL the agent process (h14's own 'kill process')."""
        self._proc.kill()
        try:
            return self._proc.wait(timeout=5.0)
        except Exception:
            return -9

    def close(self) -> None:
        try:
            if self._proc.poll() is None:
                self._proc.terminate()
                self._proc.wait(timeout=5.0)
        except Exception:
            pass


# ---------------------------------------------------------------------------
# the probe
# ---------------------------------------------------------------------------


def _spawn(backend: str, state_dir: Path, cwd: str):
    """One agent process, per backend. Returns (Popen, note)."""
    if backend == "fake":
        if not FAKE_AGENT.is_file():
            raise RuntimeError(f"the fake agent fixture is missing: {FAKE_AGENT}")
        os.chmod(FAKE_AGENT, 0o755)
        env = dict(os.environ)
        env["FAKE_ACP_BEHAVIOR"] = "load"
        env["FAKE_ACP_STATE_DIR"] = str(state_dir)
        note = (
            "fake_acp_agent.py (FAKE_ACP_BEHAVIOR=load; durable state simulated by its marker file)"
        )
        return (
            subprocess.Popen(  # noqa: S603 - the probe's own boundary
                [sys.executable, str(FAKE_AGENT), "--acp"],
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                text=True,
                bufsize=1,
                cwd=cwd,
                env=env,
            ),
            note,
        )
    # real: the seam package is needed for the binary probe (the known
    # install paths, never the non-interactive PATH)
    from qwen_bridge import qwen_cli
    from qwen_bridge.config import Config

    qwen_bin = qwen_cli.locate_qwen_bin(Config())
    note = (
        f"real qwen binary at {qwen_bin} (located by qwen_cli.locate_qwen_bin - "
        "the known install paths, never PATH)"
    )
    proc = subprocess.Popen(  # noqa: S603 - the probe's own boundary
        [qwen_bin, "--acp"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        text=True,
        bufsize=1,
        cwd=cwd,
        env=dict(os.environ),
    )
    return proc, note


def run_probe(backend: str, timeout: float) -> dict[str, Any]:
    started = datetime.now(timezone.utc).isoformat()
    result: dict[str, Any] = {
        "probe": "h14: kill process, session/load the recorded sessionId",
        "backend": backend,
        "started_at": started,
        "host": os.uname().nodename if hasattr(os, "uname") else str(sys.platform),
        "python": sys.version.split()[0],
        "timeout_seconds": timeout,
        "sequence": [],
        "outcome": "incomplete",
    }
    with tempfile.TemporaryDirectory(prefix="h14-probe-") as tmp:
        tmp_path = Path(tmp)
        cwd = str(tmp_path / "scratch-cwd")
        os.makedirs(cwd)
        state_dir = tmp_path / "agent-state"
        state_dir.mkdir()

        # -- generation 1: create the session, record its id, kill it ----
        proc1, spawn_note = _spawn(backend, state_dir, cwd)
        result["sequence"].append({"gen": 1, "step": "spawn", "note": spawn_note})
        rpc1 = JsonRpc(proc1, timeout=timeout)
        try:
            init1 = rpc1.request(
                "initialize",
                {
                    "protocolVersion": 1,
                    "clientCapabilities": {
                        "fs": {"readTextFile": False, "writeTextFile": False},
                        "terminal": False,
                    },
                },
            )
            result["sequence"].append({"gen": 1, "step": "initialize", "response": init1})
            agent_info = (init1.get("result") or {}).get("agentInfo")
            if agent_info:
                result["agent_info"] = agent_info
            new1 = rpc1.request("session/new", {"cwd": cwd, "mcpServers": []})
            result["sequence"].append({"gen": 1, "step": "session/new", "response": new1})
            if "error" in new1:
                result["outcome"] = "incomplete: session/new refused by the agent"
                return result
            session_id = (new1.get("result") or {}).get("sessionId")
            if not isinstance(session_id, str) or not session_id:
                result["outcome"] = "incomplete: session/new returned no sessionId"
                return result
            result["session_id"] = session_id
            killed_code = rpc1.kill()
            result["sequence"].append(
                {"gen": 1, "step": "kill", "signal": "SIGKILL", "exit_code": killed_code}
            )
        except TimeoutError as exc:
            result["outcome"] = f"incomplete: {exc}"
            rpc1.kill()
            return result
        finally:
            rpc1.close()

        # -- generation 2: fresh process, load the recorded id -----------
        proc2, _ = _spawn(backend, state_dir, cwd)
        result["sequence"].append(
            {"gen": 2, "step": "spawn", "note": "a FRESH process, same state dir"}
        )
        rpc2 = JsonRpc(proc2, timeout=timeout)
        try:
            init2 = rpc2.request(
                "initialize",
                {
                    "protocolVersion": 1,
                    "clientCapabilities": {
                        "fs": {"readTextFile": False, "writeTextFile": False},
                        "terminal": False,
                    },
                },
            )
            result["sequence"].append({"gen": 2, "step": "initialize", "response": init2})
            load2 = rpc2.request(
                "session/load", {"cwd": cwd, "mcpServers": [], "sessionId": session_id}
            )
            result["sequence"].append({"gen": 2, "step": "session/load", "response": load2})
            if "result" in load2:
                load_result = load2["result"] or {}
                result["load_result_keys"] = sorted(load_result.keys())
                result["outcome"] = (
                    "load-succeeded: the fresh process's session/load returned a result for the "
                    "recorded sessionId after the creating process was SIGKILL'd"
                )
            else:
                error = load2.get("error") or {}
                result["load_error"] = error
                result["outcome"] = (
                    "load-refused: the fresh process's session/load answered with a "
                    f"JSON-RPC error (code {error.get('code')}, "
                    f"{error.get('message')!r}) - the session did not survive "
                    "the process kill on this backend"
                )
        except TimeoutError as exc:
            result["outcome"] = f"incomplete: {exc}"
            rpc2.kill()
        finally:
            rpc2.close()
    return result


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument(
        "--backend",
        choices=["auto", "real", "fake"],
        default="auto",
        help="auto (default): the real qwen if the seam's probe finds it, else the fake agent",
    )
    parser.add_argument("--timeout", type=float, default=30.0, help="per-step deadline (seconds)")
    parser.add_argument(
        "--result",
        default=str(RESULT_PATH),
        help="where the recorded result JSON is written (default: beside this script, committed)",
    )
    args = parser.parse_args(argv)

    backend = args.backend
    if backend == "auto":
        try:
            from qwen_bridge import qwen_cli
            from qwen_bridge.config import Config

            qwen_bin = qwen_cli.locate_qwen_bin(Config())
            backend = "real"
            print(f"h14 probe: using the real qwen binary at {qwen_bin}", file=sys.stderr)
        except Exception as exc:
            backend = "fake"
            print(
                f"h14 probe: real qwen unavailable ({exc}); using the fake agent backend",
                file=sys.stderr,
            )

    result = run_probe(backend, args.timeout)

    if backend == "fake":
        result["provenance_note"] = (
            "FAKE backend: this exercised the probe's machinery (two generations, the kill, the "
            "load) against the committed fixture's marker-file simulation of durable state. It is "
            "NOT the durable-session measurement h14 asks for; a run against the real qwen binary "
            "supersedes it. The first-cut dispatch path does not consume continuation_ref either "
            "way (frame park v2)."
        )
    result["finished_at"] = datetime.now(timezone.utc).isoformat()
    out_path = Path(args.result)
    out_path.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"outcome": result["outcome"], "result": str(out_path)}, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
