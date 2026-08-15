"""Small HTTP response helpers shared by the split run CLI test modules."""

from __future__ import annotations

import json


def write_compact(handler, status: int, payload: object) -> None:
    """Write JSON without the fake server's normal pretty formatting."""
    raw = json.dumps(payload, separators=(",", ":")).encode()
    handler.send_response(status)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(raw)))
    handler.end_headers()
    handler.wfile.write(raw)
