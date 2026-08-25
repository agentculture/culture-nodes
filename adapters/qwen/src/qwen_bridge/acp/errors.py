"""The seam's exception vocabulary.

Plan task t2 of qwen-bridge-acp (module split; the classes lived in the
monolithic `qwen_cli` before). The distinction that carries the whole
refusal contract: `SpawnError` (and its two subclasses) is raised in the
BRIDGE process, before or instead of a dispatch, and `server.py` maps it
to a 503 - the serve was refused or impossible, so nothing is reported
as a turn result. `AcpPolicyError` is the pure-function refusal the gate
(gate.validate_initialize, gate.resolve_acp_mode) raises; the driver
child catches it at its own boundary and converts it to the
wire.REFUSAL_MARKER line + wire.REFUSAL_EXIT_CODE.
"""

from __future__ import annotations


class SpawnError(Exception):
    """The qwen ACP driver child (or the binary behind it) could not be
    started / could not be probed - distinct from any classification the
    turn itself might report once running. Mirrors the codex sibling's
    SpawnError role: server.py maps this to a 503."""


class QwenAgentMissingError(SpawnError):
    """The qwen binary probe found no usable install (the #113 contract
    leg is missing). Boot refusal: NO invoke is served. The message is
    distinct by contract - it names the probe paths, not the PATH."""


class QwenSeamRefusal(SpawnError):
    """The driver refused BEFORE serving (handshake gate or mode policy,
    spec c19/h15/h16): a distinct message rides in `str(exc)`; server.py
    answers 503 with it and no dispatch happened."""


class AcpPolicyError(Exception):
    """A pure-function refusal of the seam policy (the handshake gate
    `validate_initialize`, the mode policy `resolve_acp_mode`). The
    driver catches it at its own boundary and converts it to the
    REFUSAL_MARKER line + REFUSAL_EXIT_CODE; unit tests raise it
    directly."""


class DriverTerminated(Exception):
    """Raised from the driver child's SIGTERM handler (driver._Driver):
    PEP 475 would otherwise retry the interrupted readline, so raising
    hands the decision to the read loop, which knows how far the turn
    got. The only sane reaction anywhere in the driver is the
    cooperative-cancel path (the measured session/cancel notification),
    so this crosses the transport->driver boundary and lives here
    rather than in either module."""
