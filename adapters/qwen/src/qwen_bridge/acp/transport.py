"""The stdio JSON-RPC channel: the ACP client's reader/writer side.

Plan task t2 of qwen-bridge-acp (module split; this channel logic lived
in the monolithic `qwen_cli`'s `_Driver` before). One qwen --acp
process per invocation (spec c17's process model); the bridge acts as
the ACP client, Qwen Code as the agent. This module owns the wire
plumbing the driver state machine (driver._Driver) runs on:

* `TranscriptWriter` - the LOCAL transcript file retaining the FULL
  both-directions stream (spec c21's debug retention): one JSON envelope
  per message, `{"dir": "c->a" | "a->c" | "a->c raw", "ts": ..., "msg":
  ...}`. A write failure must never kill the turn - the stdout stream is
  authoritative.
* `AgentLink` - framed reads/writes on the agent's stdio, the stdout
  transcript ECHO (every agent->client line is written to the driver
  child's stdout - that one-liner-per-event stream is what
  classifier.parse_session and the async runner read), and the v1
  answers to agent-to-client requests.
* `answer_agent_request` - the v1 CLIENT OBLIGATIONS as a pure function
  (the frame's resolved decision): no fs/terminal handlers in the first
  cut (the 2026-08-23 measurements show qwen runs its own tools
  in-process and sent none), LENIENT empty-result answers to unknown
  ext methods (the measured craft/drainMidTurnQueue, id 0), and a
  fail-closed "cancelled" answer to session/request_permission - never
  a silently-granted permission.
"""

from __future__ import annotations

import json
import sys
import time
from pathlib import Path
from subprocess import Popen
from typing import Any, TextIO

from qwen_bridge.acp import errors, wire


class TranscriptWriter:
    """The local transcript file (c21): the full both-directions stream,
    one JSON envelope per line, appended as the turn runs. Debug
    retention only - a failure here never touches the turn's outcome."""

    def __init__(self, path: Path) -> None:
        self._path = path

    def log(self, direction: str, obj: Any) -> None:
        try:
            self._path.parent.mkdir(parents=True, exist_ok=True)
            with open(self._path, "a", encoding="utf-8") as fh:
                fh.write(
                    json.dumps(
                        {
                            "dir": direction,
                            "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                            "msg": obj,
                        },
                        default=str,
                    )
                    + "\n"
                )
        except OSError:
            # transcript retention is for debugging (c21); a write
            # failure must never kill the turn - the stdout stream is
            # authoritative
            pass


def is_agent_request(obj: dict[str, Any]) -> bool:
    """True for an agent-to-client REQUEST line: it carries a method and
    an id, and no result/error yet - the response to it is what the
    client (this side) must send. A response carries the id and a
    result/error; a notification (session/update, session/cancel)
    carries a method and no id."""
    return bool(obj.get("method")) and "id" in obj and "result" not in obj and "error" not in obj


def is_response(obj: dict[str, Any]) -> bool:
    """True for a JSON-RPC RESPONSE line (result or error present)."""
    return "result" in obj or "error" in obj


def answer_agent_request(req: dict[str, Any]) -> dict[str, Any]:
    """The v1 client obligations for agent-to-client requests (pure -
    the caller sends the returned line).

    * unknown ext methods (the measured craft/drainMidTurnQueue, id
      0, and anything else the agent invents): LENIENT - an empty
      result, the measured answer that lets the turn complete
      (frame v1 resolution; park v5 keeps the real contract
      unvalidated).
    * session/request_permission: FAIL CLOSED - outcome cancelled.
      The first cut never silently grants a permission the measured
      agent (in-process tools, no client requests) never asked for;
      a future constrained-approval policy (park v3) routes here.
    * fs/* and terminal/*: the JSON-RPC method-not-found error -
      the v1 decision is no fs/terminal handlers in the first cut,
      and an honest not-implemented beats a fabricated empty result
      the agent could misread.
    """
    method = str(req.get("method") or "")
    rid = req.get("id")
    if method == wire.METHOD_REQUEST_PERMISSION:
        return {
            "jsonrpc": "2.0",
            "id": rid,
            "result": {"outcome": {"outcome": "cancelled"}},
        }
    if method.startswith(wire.FS_METHOD_PREFIX) or method.startswith(wire.TERMINAL_METHOD_PREFIX):
        return {
            "jsonrpc": "2.0",
            "id": rid,
            "error": {
                "code": -32601,
                "message": (
                    f"method not implemented: {method!r} — the qwen-bridge first cut "
                    "implements no fs/terminal client handlers (measured: the agent "
                    "runs its own tools in-process)"
                ),
            },
        }
    # everything else - the lenient unknown-ext answer (the measured
    # craft/drainMidTurnQueue, id 0, and anything else the agent
    # invents): an empty result lets the turn complete normally
    return {"jsonrpc": "2.0", "id": rid, "result": {}}


class AgentLink:
    """One stdio JSON-RPC channel to one agent process: framed writes,
    the stdout transcript echo, the local transcript file (c21), and
    the v1 answers to agent-to-client requests. The driver child's
    turn state machine (driver._Driver) drives the two read loops:
    `wait_response` (request the agent must answer) and `drain`
    (the post-SIGTERM cooperative-cancel window)."""

    def __init__(self, proc: Popen, *, transcript_path: Path, stdout: TextIO = sys.stdout) -> None:
        self._proc = proc
        self._transcript = TranscriptWriter(transcript_path)
        self._stdout = stdout

    # -- framing ----------------------------------------------------------

    def send(self, obj: dict[str, Any]) -> None:
        """One client->agent line (request or notification): retained in
        the local transcript file (c21) and written to the agent's
        stdin."""
        self._transcript.log("c->a", obj)
        assert self._proc.stdin is not None
        self._proc.stdin.write(json.dumps(obj) + "\n")
        self._proc.stdin.flush()

    def echo(self, obj: Any) -> None:
        """The transcript contract: every agent->client line is written
        to the driver child's stdout, one JSON object per line - that is
        the stream classifier.parse_session reads."""
        self._stdout.write(json.dumps(obj) + "\n")
        self._stdout.flush()

    def _read_line(self) -> str:
        assert self._proc.stdout is not None
        return self._proc.stdout.readline()

    # -- the read loops ----------------------------------------------------

    def wait_response(self, want_id: int) -> dict[str, Any] | None:
        """Read lines until the response to want_id arrives. Every
        agent->client line is echoed to stdout (the transcript contract)
        and retained in the local transcript file (c21); agent-to-client
        requests get their v1 answers. Returns the response, or None on
        EOF (the agent died). Raises errors.DriverTerminated when the
        driver's SIGTERM handler fires mid-read (the caller decides)."""
        while True:
            line = self._read_line()
            if not line:
                return None
            stripped = line.strip()
            if not stripped:
                continue
            try:
                obj = json.loads(stripped)
            except ValueError:
                self._transcript.log("a->c raw", stripped[:400])
                continue
            self.echo(obj)
            if not isinstance(obj, dict):
                continue
            if is_agent_request(obj):
                # an agent-to-client REQUEST: an a->c line too (c21),
                # answered per the v1 client obligations
                self._transcript.log("a->c", obj)
                answer = answer_agent_request(obj)
                self.send(answer)
                if obj.get("method") == wire.METHOD_REQUEST_PERMISSION:
                    # Unlike ordinary client traffic, this decision is part
                    # of the driver result: the classifier must distinguish
                    # a permission-starved end_turn from completed work.
                    self.echo(answer)
                continue
            self._transcript.log("a->c", obj)
            if is_response(obj) and obj.get("id") == want_id:
                # the response for the request we are waiting on: it
                # goes to stdout (the transcript contract) AND the local
                # transcript file (c21: the FULL stream, both directions)
                return obj

    def drain(self, want_id: int) -> None:
        """After the cooperative session/cancel: keep reading until the
        cancelled terminal (or anything terminal for want_id) reaches the
        transcript, or EOF. Whatever got captured classifies honestly - a
        cancelled terminal is the cancellation outcome; nothing is 'ok'
        without the measured terminal. Mirrors the measured cancel
        sequence (the agent answers its own cancel with the terminal and
        stays quiet), so agent requests are NOT answered here."""
        try:
            while True:
                line = self._read_line()
                if not line:
                    break
                stripped = line.strip()
                if not stripped:
                    continue
                try:
                    obj = json.loads(stripped)
                except ValueError:
                    continue
                if not isinstance(obj, dict):
                    continue
                self.echo(obj)
                self._transcript.log("a->c", obj)
                if is_response(obj) and obj.get("id") == want_id:
                    break
        except errors.DriverTerminated:
            # a second SIGTERM in the drain window: nothing left to do -
            # the watchdog (_force_stop) bounds the window anyway
            pass
