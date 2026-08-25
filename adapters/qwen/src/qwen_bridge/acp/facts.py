"""Seam facts: the structured data t3's capability surface consumes.

Plan task t2 of qwen-bridge-acp (module split; the dataclass lived in the
monolithic `qwen_cli` before). The interface contract with plan t3's
capabilities.py: it consumes these fields BY VALUE, as plain data - it
must never import this package. Every field is MEASURED from the agent's
own responses, never assumed: a field the transcript did not carry is
None / empty.
"""

from __future__ import annotations

from dataclasses import asdict, dataclass
from typing import Any


@dataclass(frozen=True)
class SeamFacts:
    """The handshake/session facts this seam measured on the wire, as
    plain data (the interface contract with plan t3's capabilities.py,
    which consumes these fields by value - it must never import this
    module). Every field is MEASURED from the agent's own responses,
    never assumed: a field the transcript did not carry is None.

    * protocol_version  - the initialize response's negotiated
      protocolVersion (the gate's job, c19).
    * agent_info        - the initialize response's agentInfo
      {name, version} (+ title when the agent sent one); the pinned
      0.22.0 fact the capability surface reports.
    * auth_methods      - the initialize response's authMethods, verbatim
      (the config-source facts, c20 - the OPENAI_API_KEY env path, with
      the bundle's own _meta; values, never secrets).
    * modes_available   - the session/new response's
      modes.availableModes ids, in the agent's own order (the measured
      plan/default/auto-edit/auto set, c18).
    * effective_mode    - the mode this bridge SET at session creation
      (the agent's own post-set_mode current_mode_update echo when the
      stream carries one, else None) - the capability surface's
      "effective session mode" fact (h15). Never the agent's default:
      the bridge refuses to serve a session it did not set.
    * session_id        - the session/new sessionId (the qwen session
      identity; rides as the 13.2 continuation_ref when the mapping
      module builds a result - the ported t5 design).
    * current_model_id  - the session/new models.currentModelId (the
      host's own model identity, the fleet's measured
      unsloth/Qwen3.8-27B-NVFP4 / cortex split).
    """

    protocol_version: int | None = None
    agent_info: dict[str, str] | None = None
    auth_methods: tuple[dict[str, Any], ...] = ()
    modes_available: tuple[str, ...] = ()
    effective_mode: str | None = None
    session_id: str | None = None
    current_model_id: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)
