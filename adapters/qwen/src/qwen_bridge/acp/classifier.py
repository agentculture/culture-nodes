"""The terminal-event classifier (spec c4/c16/h3/h13).

Plan task t2 of qwen-bridge-acp (module split; the classifier lived in
the monolithic `qwen_cli` before). `parse_session` turns the driver's
captured ACP transcript (agent-to-client JSON-RPC, one message per line
- exactly what the driver child echoes to its own stdout) into the
TaskResult-shaped dict the mapping module classifies, and it is the
ONLY thing that decides ok / error / incomplete: the process exit code
is not even an argument to it, structurally - the codex sibling's
crash-case rule, mirrored (the RULE, not its JSONL shape).

The measured shapes (spec c16/h13), each with a distinct
classification:

* **ok** — stopReason `end_turn`. A tool-level failure still ends
  `end_turn` with error null - the tool failure rides in the transcript
  (the downsampled `acp_transcript.tool_calls` statuses), never in the
  run status, exactly as measured on 2026-08-23.
* **error** — a JSON-RPC error object on the session/prompt response
  (the model/API failure shape is SYNTHESIZED in the suite per frame
  park v4 - the plan risk register requires the first live occurrence
  to be diffed against the fixture), or a terminal stopReason that is
  not a clean end_turn: `cancelled` maps to the 13.5/13.6 CANCELLATION
  outcome (termination_reason "cancelled"; the engine's durable
  cancellation record is authoritative - PRD 13.6 - and the ported
  mapping module reports this as its execution failure with that
  termination reason riding in the FailedPayload), `max_tokens` maps to
  a provider-limit failure, and an unrecognised stopReason fails closed
  as an error rather than an ok.
* **incomplete** — NO terminal response was ever seen: the agent died
  mid-turn, the bridge's own SIGTERM fired, or the handshake was
  refused. Deliberately the default/fallback classification, not a
  special case: there is no branch anywhere in this module that
  promotes an exit code of 0 to success on its own. THAT is the
  concrete mechanism behind spec c4/h3: a qwen process that exits 0
  without a terminal session/prompt response is incomplete, never ok.
"""

from __future__ import annotations

import json
from typing import Any

from qwen_bridge.acp import wire
from qwen_bridge.acp.facts import SeamFacts

_STATUS_OK = "ok"
_STATUS_ERROR = "error"
_STATUS_INCOMPLETE = "incomplete"


def _terminal_error_message(code: Any, message: Any) -> str:
    return f"JSON-RPC error on the session/prompt response: code {code}, {message}"


def _content_text(content: Any) -> str | None:
    """The text carried by a session/update `content` field, for both
    measured shapes: the single {type:"text", text} block (the
    agent_message/agent_thought chunks) and the ACP SDK's
    ToolCallContent - a LIST of content blocks (the measured tool
    output, e.g. the failed cat's stderr). None when no text is
    carried (never an invented empty)."""
    if isinstance(content, dict):
        text = content.get("text")
        return text if isinstance(text, str) else None
    if isinstance(content, list):
        parts: list[str] = []
        for block in content:
            if not isinstance(block, dict):
                continue
            inner = block.get("content")
            if isinstance(inner, dict):
                text = inner.get("text")
                if isinstance(text, str):
                    parts.append(text)
            else:
                text = block.get("text")
                if isinstance(text, str):
                    parts.append(text)
        return "".join(parts) if parts else None
    return None


def _classify_terminal(stop: Any, error: Any) -> tuple[str, str | None]:
    """The c16 table, one branch per measured terminal shape. Returns
    (status, error_text); the status vocabulary is the mapping module's
    own (ok / error / incomplete) so no codex-free core change is
    needed."""
    if stop is not None:
        if stop == wire.STOP_END_TURN:
            # Measured: even a failed tool call ends end_turn with error
            # null - the tool failure rides in the transcript, not here.
            return _STATUS_OK, None
        if stop == wire.STOP_CANCELLED:
            # The 13.5/13.6 cancellation outcome: a TERMINAL failure with
            # termination_reason "cancelled" (the ported mapping module
            # reports it as its execution failure; the engine's durable
            # cancellation record - PRD 13.6 - is authoritative, and the
            # FailedPayload carries the provider's own reason).
            return _STATUS_ERROR, (
                "qwen's session/prompt ended with stopReason: cancelled — the 13.5/13.6 "
                "cancellation outcome (terminal, never ok; the engine's durable cancellation "
                "record is authoritative per PRD 13.6)"
            )
        if stop == wire.STOP_MAX_TOKENS:
            return _STATUS_ERROR, (
                "qwen's session/prompt ended with stopReason: max_tokens — the provider's "
                "token limit ended the turn; it did not complete"
            )
        return _STATUS_ERROR, (
            f"qwen's session/prompt ended with an unrecognised stopReason {stop!r} — "
            "failing closed; only the measured end_turn completes a turn"
        )
    if error is not None:
        return _STATUS_ERROR, _terminal_error_message(error.get("code"), error.get("message"))
    # No terminal response at all: killed, crashed, or the bridge's own
    # timeout fired. Deliberately the default branch - never "ok".
    return _STATUS_INCOMPLETE, None


def parse_session(stdout: str) -> dict[str, Any] | None:
    """Turn the driver's captured ACP transcript (agent-to-client
    JSON-RPC, one message per line - exactly what the driver child
    writes to its own stdout) into the TaskResult-shaped dict the
    mapping module classifies: `{status, summary, changed_files, usage,
    task_id, error, model, termination_reason}` plus this seam's own
    downsampled facts:

    * `acp_transcript` - the c21 DOWNSAMPLED transcript: the final
      assistant text (the non-thought agent_message_chunks joined),
      the tool_call summaries with their measured statuses
      (pending / in_progress / completed / failed - the ACP SDK's
      zToolCallStatus), the usage_update totals (the context-window
      used/size counters the agent emits - NOT token counts, so the
      mapping module's §13.2 usage block correctly stays absent),
      the thought-chunk count, the total notification count, and the
      ext methods the agent sent (craft/drainMidTurnQueue). This is
      what rides in the 13 result payload's place - never the raw
      session/update firehose (measured ~72 notifications for one
      simple turn, dominated by thought chunks).
    * `seam_facts` - the SeamFacts dataclass, as a plain dict (the t3
      capability-surface contract): protocol_version, agent_info,
      auth_methods, modes_available, effective_mode, session_id,
      current_model_id - all measured from the agent's own responses.

    The classification is this module's job alone (spec c16):

    * stopReason end_turn            -> ok
    * stopReason cancelled           -> error, termination_reason
                                         "cancelled" (the 13
                                         cancellation outcome)
    * stopReason max_tokens          -> error (provider limit)
    * stopReason <unrecognised>      -> error (fail closed)
    * JSON-RPC error on the prompt
      response                       -> error (code + message)
    * no terminal response at all    -> incomplete, NEVER ok - the
                                         exit code is not an argument
                                         here, structurally, so there
                                         is no branch that promotes an
                                         exit 0 to success (spec c4/h3,
                                         the codex crash-case rule).

    Returns None only when not one line parsed as a JSON object at all
    (the driver produced no parseable transcript - mirroring the codex
    sibling's `parse_session` contract exactly).
    """
    saw_any_json = False
    initialize_result: dict[str, Any] | None = None
    session_new_result: dict[str, Any] | None = None
    terminal_stop: Any = None
    terminal_error: dict[str, Any] | None = None
    terminal_meta: dict[str, Any] | None = None

    # --- the downsampled accumulation (c21) --------------------------------
    final_text_parts: list[str] = []
    thought_chunks = 0
    notification_count = 0
    tool_calls: dict[str, dict[str, Any]] = {}
    tool_call_order: list[str] = []
    usage_totals: dict[str, Any] | None = None
    effective_mode: str | None = None
    ext_methods_seen: list[str] = []

    for line in stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except ValueError:
            continue
        if not isinstance(obj, dict):
            continue
        saw_any_json = True

        method = obj.get("method")
        if method == wire.METHOD_SESSION_UPDATE:
            params = obj.get("params") or {}
            update = params.get("update")
            if not isinstance(update, dict):
                continue
            notification_count += 1
            kind = update.get("sessionUpdate")
            content = update.get("content")
            text = _content_text(content)
            if kind == "agent_message_chunk":
                if text is not None:
                    final_text_parts.append(text)
            elif kind == "agent_thought_chunk":
                # the firehose's dominant kind - counted, never forwarded
                thought_chunks += 1
            elif kind == "usage_update":
                # the measured shape: context-window used/size counters
                usage_totals = {
                    "used": update.get("used"),
                    "size": update.get("size"),
                }
            elif kind == "current_mode_update":
                mode_id = update.get("currentModeId")
                if isinstance(mode_id, str) and mode_id:
                    effective_mode = mode_id
            elif kind == "tool_call":
                tool_call_id = update.get("toolCallId")
                if isinstance(tool_call_id, str) and tool_call_id:
                    if tool_call_id not in tool_calls:
                        tool_call_order.append(tool_call_id)
                    entry = tool_calls.setdefault(
                        tool_call_id,
                        {
                            "toolCallId": tool_call_id,
                            "title": update.get("title"),
                            "kind": update.get("kind"),
                            "status": update.get("status") or "pending",
                            "rawInput": update.get("rawInput"),
                            "output": "",
                        },
                    )
                    for field_name in ("title", "kind"):
                        if update.get(field_name) is not None:
                            entry[field_name] = update[field_name]
                    if update.get("status"):
                        entry["status"] = update["status"]
                    if update.get("rawInput") is not None:
                        entry["rawInput"] = update["rawInput"]
                    if text is not None:
                        entry["output"] += text
            elif kind == "tool_call_update":
                tool_call_id = update.get("toolCallId")
                if isinstance(tool_call_id, str) and tool_call_id in tool_calls:
                    entry = tool_calls[tool_call_id]
                    if update.get("status"):
                        entry["status"] = update["status"]
                    if text is not None:
                        entry["output"] += text
                    # a completed/failed call's content rides the summary
                    # - the tool failure that stays out of the run status
            # every other update kind (plan, user_message_chunk, ...) is
            # counted in notification_count and dropped from the payload
            continue

        if method:
            # an agent-to-client REQUEST line (the measured
            # craft/drainMidTurnQueue, id 0 - the driver answers it
            # leniently; here it is transcript evidence)
            ext_methods_seen.append(str(method))
            continue

        # a RESPONSE line
        result = obj.get("result")
        error = obj.get("error")
        if isinstance(result, dict) and "protocolVersion" in result:
            initialize_result = result
        elif isinstance(result, dict) and "modes" in result and "sessionId" in result:
            session_new_result = result
        elif isinstance(result, dict) and "stopReason" in result:
            terminal_stop = result.get("stopReason")
            meta = result.get("_meta")
            terminal_meta = meta if isinstance(meta, dict) else None
        elif isinstance(error, dict):
            # a JSON-RPC error response - on this seam (the driver
            # refuses before serving anything else) only the prompt's
            # terminal error lands here
            terminal_error = error

    if not saw_any_json:
        return None

    status, error_text = _classify_terminal(terminal_stop, terminal_error)
    summary = "".join(final_text_parts)

    session_id: str | None = None
    modes_available: tuple[str, ...] = ()
    current_model_id: str | None = None
    if isinstance(session_new_result, dict):
        sid = session_new_result.get("sessionId")
        session_id = sid if isinstance(sid, str) and sid else None
        modes = session_new_result.get("modes")
        if isinstance(modes, dict):
            for mode_entry in modes.get("availableModes") or []:
                if isinstance(mode_entry, dict) and isinstance(mode_entry.get("id"), str):
                    modes_available += (mode_entry["id"],)
        models = session_new_result.get("models")
        if isinstance(models, dict):
            model_id = models.get("currentModelId")
            if isinstance(model_id, str) and model_id:
                current_model_id = model_id

    protocol_version: int | None = None
    agent_info: dict[str, str] | None = None
    auth_methods: tuple[dict[str, Any], ...] = ()
    if isinstance(initialize_result, dict):
        pv = initialize_result.get("protocolVersion")
        protocol_version = pv if isinstance(pv, int) else None
        info = initialize_result.get("agentInfo")
        if isinstance(info, dict):
            agent_info = {
                "name": str(info.get("name")) if info.get("name") is not None else None,
                "version": str(info.get("version")) if info.get("version") is not None else None,
            }
            title = info.get("title")
            if isinstance(title, str) and title:
                agent_info["title"] = title
        raw_auth = initialize_result.get("authMethods")
        if isinstance(raw_auth, list):
            auth_methods = tuple(entry for entry in raw_auth if isinstance(entry, dict))

    branch_point = None
    if isinstance(terminal_meta, dict):
        branch_point = terminal_meta.get("qwen.branchPoint")

    seam_facts = SeamFacts(
        protocol_version=protocol_version,
        agent_info=agent_info,
        auth_methods=auth_methods,
        modes_available=modes_available,
        effective_mode=effective_mode,
        session_id=session_id,
        current_model_id=current_model_id,
    )

    return {
        "status": status,
        "summary": summary,
        # no ACP update reports file changes as a first-class fact (the
        # tool_call kind 'edit'/'write' is a claim about intent, not a
        # measured change) - never invent the list.
        "changed_files": [],
        # context-window counters are NOT token counts: the mapping
        # module's §13.2 usage block correctly stays absent.
        "usage": {},
        "task_id": session_id,
        "error": error_text,
        "model": current_model_id,
        "termination_reason": terminal_stop if isinstance(terminal_stop, str) else None,
        "acp_transcript": {
            "final_text": summary,
            "thought_chunks": thought_chunks,
            "notifications": notification_count,
            "tool_calls": [tool_calls[tool_call_id] for tool_call_id in tool_call_order],
            "usage_updates": usage_totals,
            "ext_methods_answered": ext_methods_seen,
            "branch_point": branch_point if isinstance(branch_point, dict) else None,
        },
        "seam_facts": seam_facts.to_dict(),
    }
