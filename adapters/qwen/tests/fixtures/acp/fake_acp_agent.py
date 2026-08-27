#!/usr/bin/env python3
"""Fake ACP agent: a stdlib-only stdio JSON-RPC replayer of the measured
qwen 0.22.0 wire shapes, for the qwen-bridge unit suite.

It replaces `qwen --acp` (the bridge's driver spawns it exactly like the
real binary: `[fake_acp_agent.py, "--acp"]`) and replays the committed
fixtures next to it, so the suite exercises the REAL subprocess boundary
(Popen, pipes, SIGTERM, EOF) without a real qwen install or a live model
endpoint - the same discipline as the codex sibling's `fake_codex`
fixture (adapters/codex/tests/conftest.py), with the fixture promoted to
a committed, versioned artifact because its content IS the measured
grounding (see each fixture's own "provenance" field).

Behavior is selected with the FAKE_ACP_BEHAVIOR env var (the driver's
parent sets it the way the codex conftest sets FAKE_CODEX_BEHAVIOR):

  ok                     firehose_72_updates.json; terminal stopReason end_turn
  failed-tool            end_turn_with_failed_tool.json; terminal stopReason
                         end_turn (the failed tool call rides in the stream)
  cancelled              cancelled.json; the terminal (stopReason cancelled)
                         is emitted ONLY after a session/cancel notification
  prompt-error           prompt_error.json; a JSON-RPC error object on the
                         session/prompt response (synthesized, park v4)
  crash                  crash.json; dies (exit 0) with no terminal at all
  dies-before-handshake  exits 0 before answering anything
  protocol-version-2     the measured initialize response with
                         protocolVersion 2 (the c19/h16 refusal case)
  agent-version-mismatch the measured initialize response with
                         agentInfo.version "9.9.9" (the c19/h16 refusal case)
  load                   session/new persists a marker in FAKE_ACP_STATE_DIR
                         so a FRESH process can session/load the same
                         sessionId (the h14 resume probe's fake backend)

Sidecar files (under FAKE_ACP_STATE_DIR, a tempfile dir by default) let a
test assert what the driver actually sent: set_mode.json (the mode the
driver chose - the h15 never-fall-back assertion) and ext_answer.json (the
driver's lenient answer to craft/drainMidTurnQueue - the v1 decision).
"""

from __future__ import annotations

import json
import os
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
BEHAVIOR = os.environ.get("FAKE_ACP_BEHAVIOR", "ok")
STATE_DIR = os.environ.get("FAKE_ACP_STATE_DIR") or tempfile.mkdtemp(prefix="fake-acp-agent-")
SESSION_ID = "8c9f1b2e-4a6d-4e7f-9b3c-1d2e3f4a5b6c"  # the fixtures' fixed session


def _fixture(name: str) -> dict:
    with open(os.path.join(HERE, name), encoding="utf-8") as fh:
        return json.load(fh)


def _emit(obj) -> None:
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def _respond(rid, result=None, error=None) -> None:
    obj = {"jsonrpc": "2.0", "id": rid}
    if error is not None:
        obj["error"] = error
    else:
        obj["result"] = result if result is not None else {}
    _emit(obj)


def _notify_update(session_id: str, update: dict) -> None:
    _emit(
        {
            "jsonrpc": "2.0",
            "method": "session/update",
            "params": {"sessionId": session_id, "update": update},
        }
    )


def _remember(path: str, obj) -> None:
    with open(os.path.join(STATE_DIR, path), "w", encoding="utf-8") as fh:
        json.dump(obj, fh)


def _initialize_result() -> dict:
    result = dict(_fixture("initialize_measured.json")["response"]["result"])
    if BEHAVIOR == "protocol-version-2":
        result["protocolVersion"] = 2
    elif BEHAVIOR == "agent-version-mismatch":
        info = dict(result["agentInfo"])
        info["version"] = "9.9.9"
        result["agentInfo"] = info
    return result


def _replay_turn(session_id: str) -> None:
    """Replay one session/prompt's stream per the behavior's transcript
    fixture: the session/update notifications (with the measured
    craft/drainMidTurnQueue ext request injected mid-stream), then the
    terminal - or no terminal at all (crash)."""
    doc = {
        "ok": "firehose_72_updates.json",
        "failed-tool": "end_turn_with_failed_tool.json",
        "cancelled": "cancelled.json",
        "prompt-error": "prompt_error.json",
        "crash": "crash.json",
    }[BEHAVIOR]
    transcript = _fixture(doc)
    ext = transcript.get("ext_request")
    ext_after = transcript.get("ext_after")
    updates = transcript.get("updates") or []
    terminal = transcript.get("terminal")
    waiting_for_cancel = bool(transcript.get("terminal_only_after_cancel"))

    for i, update in enumerate(updates, start=1):
        _notify_update(session_id, update)
        if ext is not None and i == ext_after:
            _emit(ext)  # agent->client request; the measured id is 0
            line = sys.stdin.readline()
            if not line:
                return  # the client died before answering; stop replaying
            try:
                answer = json.loads(line)
            except ValueError:
                continue
            _remember("ext_answer.json", answer)
    if BEHAVIOR == "crash":
        # the measured crash case (spec c4/h3): the agent process DIES
        # mid-turn - exit 0 (a clean exit, mirroring the SIGTERM'd codex
        # session the rule was grounded on) and NO terminal response.
        sys.exit(0)
    if waiting_for_cancel:
        # Block on the client: a session/cancel notification (no id) ends
        # the turn with the measured cancelled terminal; EOF means the
        # client went away first - die without a terminal (that is the
        # crash classification the driver must get).
        while True:
            line = sys.stdin.readline()
            if not line:
                return
            try:
                obj = json.loads(line)
            except ValueError:
                continue
            if obj.get("method") == "session/cancel":
                if terminal:
                    rid = _PROMPT_RID[0]
                    _respond(rid, result=terminal.get("result"), error=terminal.get("error"))
                return
    if terminal:
        rid = _PROMPT_RID[0]
        _respond(rid, result=terminal.get("result"), error=terminal.get("error"))


_PROMPT_RID: list = []


def main() -> None:
    if BEHAVIOR == "dies-before-handshake":
        sys.exit(0)

    while True:
        line = sys.stdin.readline()
        if not line:
            return
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except ValueError:
            continue
        method = obj.get("method")
        rid = obj.get("id")
        params = obj.get("params") or {}

        if method == "initialize":
            _respond(rid, result=_initialize_result())
        elif method == "session/new":
            result = dict(_fixture("session_new_measured.json")["response"]["result"])
            _remember("session_new.json", {"sessionId": result["sessionId"]})
            if BEHAVIOR == "load":
                marker = os.path.join(STATE_DIR, "sessions.json")
                known: dict = {}
                if os.path.exists(marker):
                    with open(marker, encoding="utf-8") as fh:
                        known = json.load(fh)
                known[result["sessionId"]] = True
                _remember("sessions.json", known)
            _respond(rid, result=result)
        elif method == "session/load":
            sid = params.get("sessionId")
            known = {}
            marker = os.path.join(STATE_DIR, "sessions.json")
            if os.path.exists(marker):
                with open(marker, encoding="utf-8") as fh:
                    known = json.load(fh)
            if sid in known:
                _respond(rid, result=_fixture("session_load_measured.json")["response"]["result"])
            else:
                _respond(rid, error={"code": -32002, "message": f"session not found: {sid}"})
        elif method == "session/set_mode":
            _remember("set_mode.json", params)
            # the measured post-set_mode notification (the bundle's
            # sendCurrentModeUpdateNotification): the effective mode, echoed
            _notify_update(
                params.get("sessionId", SESSION_ID),
                {
                    "sessionUpdate": "current_mode_update",
                    "currentModeId": params.get("modeId"),
                },
            )
            _respond(rid)
        elif method == "session/prompt":
            _PROMPT_RID.append(rid)
            _replay_turn(params.get("sessionId", SESSION_ID))
        elif method == "session/cancel":
            # a notification (no id) - handled inside _replay_turn's wait
            # loop; if the turn already finished there is nothing to do.
            continue
        else:
            # unknown client request: answer empty (the fake is lenient the
            # way the measured agent's host side was lenient).
            if "id" in obj:
                _respond(rid)


if __name__ == "__main__":
    main()
