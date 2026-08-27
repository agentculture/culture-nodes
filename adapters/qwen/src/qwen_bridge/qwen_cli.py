"""qwen_cli — the ACP client seam and the terminal-event classifier.

Plan task t2 (retry) of qwen-bridge-acp. This module is the THIN FACADE
over the `qwen_bridge.acp` package - the SINGLE importable public
surface: the tests and plan t5's entrypoint import `qwen_bridge.qwen_cli`
and nothing else (the package submodules are implementation detail).
`python -m qwen_bridge.qwen_cli` runs the driver child (the __main__
block below dispatches to `acp.driver._driver_main`).

The split (this module re-exports each name from its home):

    wire        pinned ACP wire contract (c19 constants + vocabulary)
    errors      SpawnError / QwenAgentMissingError / QwenSeamRefusal /
                AcpPolicyError / DriverTerminated
    facts       SeamFacts (the plain-data t3 capability-surface contract)
    probe       locate_qwen_bin (binary probe + boot refusal, #113 leg)
    gate        validate_initialize (c19/h16) + resolve_acp_mode (c18/h15)
    classifier  parse_session (c4/c16/h3/h13 + the c21 downsample)
    transport   AgentLink / TranscriptWriter / answer_agent_request
    driver      _Driver + _driver_main (the driver child)
    dispatch    spawn / run_sync / SyncRunResult / _driver_argv

Pinned against qwen-code 0.22.0's ACP agent (`qwen --acp`, stdio
JSON-RPC), grounded on the 2026-08-23 live probes (the measured shapes
are committed, replayable fixtures under tests/fixtures/acp/ - see each
fixture's own "provenance" field). The seam architecture mirrors the
codex sibling's SUBPROCESS DISCIPLINE, not its transcript shape: the
bridge never trusts the process exit code - it classifies the TERMINAL
EVENT the session produced (the session/prompt response's
{stopReason, error}). A qwen process that exits 0 without a terminal
session/prompt response is incomplete, never ok (spec c4/h3).

`continuation_ref` is NOT implemented in the first cut (frame park v2):
the parameters are accepted for the t1-ported core's API compatibility
and the dispatch cold-starts; the cross-process session/load behavior
is measured by the named h14 probe (scripts/session_load_probe.py)
alone.
"""

from __future__ import annotations

import sys

from qwen_bridge.acp import driver, transport
from qwen_bridge.acp.classifier import parse_session
from qwen_bridge.acp.dispatch import (
    SyncRunResult,
    _driver_argv,
    git_writable_override,
    refusal_detail,
    run_sync,
    spawn,
)
from qwen_bridge.acp.errors import (
    AcpPolicyError,
    DriverTerminated,
    QwenAgentMissingError,
    QwenSeamRefusal,
    SpawnError,
)
from qwen_bridge.acp.facts import SeamFacts
from qwen_bridge.acp.gate import resolve_acp_mode, validate_initialize
from qwen_bridge.acp.probe import QWEN_PROBE_PATHS, locate_qwen_bin
from qwen_bridge.acp.wire import (
    ACP_MODES,
    CANCEL_DRAIN_SECONDS,
    EXPECTED_AGENT_NAME,
    EXPECTED_AGENT_VERSION,
    EXT_DRAIN_MID_TURN_QUEUE,
    FS_METHOD_PREFIX,
    METHOD_INITIALIZE,
    METHOD_REQUEST_PERMISSION,
    METHOD_SESSION_CANCEL,
    METHOD_SESSION_LOAD,
    METHOD_SESSION_NEW,
    METHOD_SESSION_PROMPT,
    METHOD_SESSION_SET_MODE,
    METHOD_SESSION_UPDATE,
    REFUSAL_EXIT_CODE,
    REFUSAL_MARKER,
    SANDBOX_MODES,
    SANDBOX_WORKSPACE_WRITE,
    STOP_CANCELLED,
    STOP_END_TURN,
    STOP_MAX_TOKENS,
    SUPPORTED_PROTOCOL_VERSIONS,
    TERMINAL_METHOD_PREFIX,
    TRANSCRIPT_MARKER,
)

__all__ = [
    # the dispatch API (the t1-ported core's read boundary)
    "spawn",
    "run_sync",
    "SyncRunResult",
    "_driver_argv",
    "git_writable_override",
    # the classifier (c4/c16/h3/h13/c21)
    "parse_session",
    # the pre-serve gates (c19/h16, c18/h15) and the binary probe (#113/h5)
    "validate_initialize",
    "resolve_acp_mode",
    "locate_qwen_bin",
    "QWEN_PROBE_PATHS",
    # the seam facts (the t3 capability-surface contract, plain data)
    "SeamFacts",
    # the exception vocabulary (server.py's 503 mapping)
    "SpawnError",
    "QwenAgentMissingError",
    "QwenSeamRefusal",
    "AcpPolicyError",
    "DriverTerminated",
    # the pinned wire contract (c19) + dispatch-surface vocabulary
    "SUPPORTED_PROTOCOL_VERSIONS",
    "EXPECTED_AGENT_NAME",
    "EXPECTED_AGENT_VERSION",
    "ACP_MODES",
    "SANDBOX_MODES",
    "SANDBOX_WORKSPACE_WRITE",
    "REFUSAL_EXIT_CODE",
    "refusal_detail",
    "REFUSAL_MARKER",
    "TRANSCRIPT_MARKER",
    "CANCEL_DRAIN_SECONDS",
    "METHOD_INITIALIZE",
    "METHOD_SESSION_NEW",
    "METHOD_SESSION_SET_MODE",
    "METHOD_SESSION_PROMPT",
    "METHOD_SESSION_CANCEL",
    "METHOD_SESSION_LOAD",
    "METHOD_SESSION_UPDATE",
    "METHOD_REQUEST_PERMISSION",
    "FS_METHOD_PREFIX",
    "TERMINAL_METHOD_PREFIX",
    "EXT_DRAIN_MID_TURN_QUEUE",
    "STOP_END_TURN",
    "STOP_CANCELLED",
    "STOP_MAX_TOKENS",
    # the transport + driver children (re-exported for introspection;
    # the public entry points are the names above)
    "transport",
    "driver",
]

if __name__ == "__main__":  # python -m qwen_bridge.qwen_cli (the driver child)
    sys.exit(driver._driver_main(sys.argv[1:]))
