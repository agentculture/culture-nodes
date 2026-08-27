"""Pinned ACP wire contract: the constants the qwen seam speaks with.

Plan task t2 of qwen-bridge-acp (module split; the constants lived in the
monolithic `qwen_cli` before). Pinned against qwen-code 0.22.0's ACP agent
(`qwen --acp`, stdio JSON-RPC), grounded on the 2026-08-23 live probes on
spark (the measured shapes are committed, replayable fixtures under
tests/fixtures/acp/ - see each fixture's own "provenance" field) and
re-pinned against the installed 0.22.0 bundle's own wire code (the ACP
SDK's method tables, the acpAgent initialize / session-new response
builders, and the stopReason / tool status vocabularies).

Spec c19 (the handshake gate, fail closed) is why this table exists at
all: qwen 0.22.0 still advertises `--acp` alongside
`--experimental-acp`, so ACP is treated as an experimental seam - the
image pins the qwen version and the conformance suite pins the handshake
shape. A disagreement refuses with a distinct message BEFORE any session
is served (see gate.validate_initialize).
"""

from __future__ import annotations

# ---------------------------------------------------------------------------
# Pinned contract constants (spec c19)
# ---------------------------------------------------------------------------

#: The protocolVersion values this bridge serves against. Measured 1 on
#: 2026-08-23 (and by the vendored ACP SDK's own PROTOCOL_VERSION in the
#: installed bundle). Anything else is a handshake refusal, before any
#: session is served.
SUPPORTED_PROTOCOL_VERSIONS = frozenset({1})

#: agentInfo the handshake gate pins. The bundle's initialize builder
#: stamps name "qwen-code" and the pinned version "0.22.0"; a mismatch is
#: a distinct refusal (c19/h16), because 0.22.0 still advertises --acp
#: alongside --experimental-acp - an experimental seam that must be
#: pinned and validated, not trusted.
EXPECTED_AGENT_NAME = "qwen-code"
EXPECTED_AGENT_VERSION = "0.22.0"

# ---------------------------------------------------------------------------
# ACP wire vocabulary (the installed 0.22.0 bundle's own constants)
# ---------------------------------------------------------------------------

METHOD_INITIALIZE = "initialize"
METHOD_SESSION_NEW = "session/new"
METHOD_SESSION_SET_MODE = "session/set_mode"
METHOD_SESSION_PROMPT = "session/prompt"
METHOD_SESSION_CANCEL = "session/cancel"
METHOD_SESSION_LOAD = "session/load"
METHOD_SESSION_UPDATE = "session/update"
METHOD_REQUEST_PERMISSION = "session/request_permission"
FS_METHOD_PREFIX = "fs/"
TERMINAL_METHOD_PREFIX = "terminal/"

#: The qwen-specific ext method measured twice on 2026-08-23 (probes 1
#: and 2): the agent asks the client to drain its mid-turn queue. The
#: first cut answers it leniently (empty result) - the measured behavior
#: that lets the prompt complete normally; frame park v5 keeps the real
#: handler contract unvalidated, so nothing may rely on mid-turn
#: queueing.
EXT_DRAIN_MID_TURN_QUEUE = "craft/drainMidTurnQueue"

#: The measured terminal stopReason vocabulary (the bundle's own
#: getAbortAwareEndTurnStopReason / controlledAbort / token-limit paths).
STOP_END_TURN = "end_turn"
STOP_CANCELLED = "cancelled"
STOP_MAX_TOKENS = "max_tokens"

# ---------------------------------------------------------------------------
# The bridge's own dispatch-surface vocabulary
# ---------------------------------------------------------------------------

#: `input.sandbox` accepts exactly these three values on the dispatch
#: surface (the t1-ported server validates against this set, field for
#: field with the codex sibling). The qwen ACP seam does NOT take a
#: --sandbox flag - the ACP session mode is the approval-policy surface
#: (spec c18) - so the sandbox value is carried to the driver only as
#: dispatch context; mapping it onto a mode is park v3's open decision,
#: and until that decision is verified per mode (h15) the bridge fails
#: closed on the mode instead of guessing (see gate.resolve_acp_mode).
SANDBOX_MODES = frozenset({"read-only", "workspace-write", "danger-full-access"})

#: The sandbox mode the handover opt-in requires (t1's server checks it).
SANDBOX_WORKSPACE_WRITE = "workspace-write"

#: The ACP session-mode vocabulary the policy may name (the measured
#: session/new availableModes ids, the four the scratch session exposed
#: on 2026-08-23 - spec c18). The h14 probe RE-MEASURED a fresh
#: non-scratch session on the same 0.22.0 on 2026-08-25 and it exposed a
#: FIFTH mode, 'yolo' (automatically approve all tools - the widest
#: grant): mode exposure is environment-dependent, and until h15
#: verifies the per-mode mapping live the vocabulary stays the four
#: measured modes and 'yolo' fails closed with a distinct refusal. The
#: selection is additionally validated against the agent's OWN measured
#: availableModes at session creation, so a mode the agent does not
#: offer fails closed too.
ACP_MODES = frozenset({"plan", "default", "auto-edit", "auto"})

# ---------------------------------------------------------------------------
# The driver child's wire markers (see driver.py / dispatch.py)
# ---------------------------------------------------------------------------

#: The driver child's exit code for a pre-serve refusal (handshake or
#: mode policy) - distinct from 0 (the turn ended one way or another)
#: and from 1 (the driver itself failed).
REFUSAL_EXIT_CODE = 2

#: The stderr marker a refusal line starts with; the parent extracts the
#: distinct message after it and raises errors.QwenSeamRefusal.
REFUSAL_MARKER = "qwen-acp-refusal:"

#: The stderr marker naming the driver's local transcript file (the full
#: both-directions stream, c21's debug retention).
TRANSCRIPT_MARKER = "qwen-acp-transcript:"

#: The SIGTERM grace the driver allows a cooperative session/cancel to
#: reach the cancelled terminal before it stops the turn anyway.
CANCEL_DRAIN_SECONDS = 5.0
