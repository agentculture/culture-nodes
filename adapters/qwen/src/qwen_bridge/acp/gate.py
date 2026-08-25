"""The pre-serve gates: the initialize handshake (c19/h16) and the
session mode policy (c18/h15).

Plan task t2 of qwen-bridge-acp (module split; both gates lived in the
monolithic `qwen_cli` before). Both are PURE functions: the driver
(driver._Driver) calls them at its own boundary and converts an
errors.AcpPolicyError into the wire.REFUSAL_MARKER line +
wire.REFUSAL_EXIT_CODE; the unit tests feed them the measured shapes
straight in (the acceptance criterion: the measured initialize response
with protocolVersion 2 refused with a distinct message before any
session is served).
"""

from __future__ import annotations

from typing import Any, Sequence

from qwen_bridge.acp import errors, wire


def validate_initialize(result: dict[str, Any]) -> None:
    """Validate the initialize handshake response. Refuses with DISTINCT
    messages (an unsupported protocolVersion and an agentInfo mismatch
    are different failures and name themselves differently) before any
    session is served. This is the pure function the driver calls and
    the unit test feeds the measured initialize response straight into.
    """
    protocol_version = result.get("protocolVersion")
    if protocol_version not in wire.SUPPORTED_PROTOCOL_VERSIONS:
        raise errors.AcpPolicyError(
            f"qwen-acp-handshake: unsupported protocolVersion {protocol_version!r} "
            f"(supported: {sorted(wire.SUPPORTED_PROTOCOL_VERSIONS)}) — refusing to serve "
            "before any session (c19: ACP is an experimental seam in qwen 0.22.0 and the "
            "handshake shape is pinned)"
        )
    agent_info = result.get("agentInfo")
    if not isinstance(agent_info, dict):
        raise errors.AcpPolicyError(
            "qwen-acp-handshake: the initialize response carried no agentInfo — refusing to serve "
            "before any session"
        )
    name = agent_info.get("name")
    version = agent_info.get("version")
    if name != wire.EXPECTED_AGENT_NAME or version != wire.EXPECTED_AGENT_VERSION:
        raise errors.AcpPolicyError(
            f"qwen-acp-handshake: agentInfo mismatch: measured name={name!r} version={version!r}, "
            f"pinned name={wire.EXPECTED_AGENT_NAME!r} version={wire.EXPECTED_AGENT_VERSION!r} — "
            "refusing to serve before any session (c19: the image pins the qwen version; a "
            "disagreement fails closed)"
        )


def resolve_acp_mode(requested: str | None, available_modes: Sequence[str]) -> str:
    """Pick the ACP session mode from the input/preflight policy, failing
    closed. The measured scratch-session default is `auto` (spec c18)
    and h15 is explicit: UNTIL the per-mode mapping is verified live,
    the bridge sets the mode from the policy and NEVER falls back to
    that default - a session that runs in a mode nobody chose would be
    a silently-wider (or silently-different) grant. Three distinct
    refusals, one per failure kind:

    * no policy mode given           -> AcpPolicyError naming the gap
    * policy mode outside the ACP
      vocabulary                     -> AcpPolicyError naming the vocabulary
    * policy mode the agent does not
      offer (its measured
      availableModes)                -> AcpPolicyError naming what was offered
    """
    if requested is None or not isinstance(requested, str) or not requested.strip():
        raise errors.AcpPolicyError(
            "qwen-acp-mode: no session mode given — the bridge sets the mode from the "
            "input/preflight policy and never falls back to the agent's measured default "
            f"(auto); supply input.mode ({' | '.join(sorted(wire.ACP_MODES))})"
        )
    if requested not in wire.ACP_MODES:
        raise errors.AcpPolicyError(
            f"qwen-acp-mode: mode {requested!r} is not in the ACP mode vocabulary "
            f"{sorted(wire.ACP_MODES)} — failing closed rather than silently picking a different "
            "(possibly wider) mode"
        )
    if requested not in available_modes:
        raise errors.AcpPolicyError(
            f"qwen-acp-mode: the agent does not offer mode {requested!r} (measured available: "
            f"{list(available_modes)}) — failing closed rather than silently substituting a "
            "different mode"
        )
    return requested
