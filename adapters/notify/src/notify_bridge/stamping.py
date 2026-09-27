"""Shared cn1 artifact stamping contract.

The control plane mints and authenticates markers. A bridge checks their
shape, embeds the supplied bytes in the artifact it creates, and reports the
provider-assigned artifact id. It cannot verify the MAC without the key.
This file is copied byte for byte to every adapter.
"""

from __future__ import annotations

import re
from typing import Any

_FIELD = r"[^:]{1,256}"
_MARKER = re.compile(rf"cn1:{_FIELD}:{_FIELD}:[0-9a-fA-F]{{48}}:[0-9a-fA-F]{{64}}\Z")

# The `/v1/capabilities` block every bridge merges into its own capability
# facts to advertise the cn1 marker contract. A module constant rather than
# a literal at each call site so `{**preflight.capability_block(host),
# **stamping.CAPABILITY}` is the whole advertisement, in every bridge.
CAPABILITY: dict[str, dict[str, Any]] = {"stamping": {"marker": "cn1", "version": 1}}


def validate_marker(marker: str) -> str:
    """Return a syntactically valid marker without changing its bytes."""
    if not isinstance(marker, str) or not _MARKER.fullmatch(marker):
        raise ValueError("invalid cn1 marker")
    return marker


def read_marker(raw_input: dict[str, Any]) -> tuple[str | None, bool]:
    """Read and validate `input.marker`.

    Returns `(marker, invalid)`. `marker` is `None` when the request carried
    none. `invalid` is `True` when a marker was present but failed
    `validate_marker`, so the caller can answer 400 without re-deriving the
    validation it already ran here.
    """
    marker = raw_input.get("marker")
    if marker is None:
        return None, False
    try:
        validate_marker(marker)
    except ValueError:
        return None, True
    return marker, False


def stamp_text(text: str, marker: str) -> str:
    """Append the marker to an artifact's text, preserving prior content."""
    validate_marker(marker)
    if not isinstance(text, str):
        raise TypeError("artifact text must be a string")
    if marker in text.splitlines():
        return text
    return f"{text.rstrip()}\n\n{marker}" if text.strip() else marker


def artifact_result(artifact_id: str | int, marker: str) -> dict[str, str]:
    """Return the provider-assigned id for control-plane marker binding."""
    validate_marker(marker)
    if isinstance(artifact_id, bool) or not isinstance(artifact_id, (str, int)):
        raise ValueError("artifact id must be a non-empty string or integer")
    value = str(artifact_id)
    if not value:
        raise ValueError("artifact id must be non-empty")
    return {"artifact_id": value, "marker": marker}
