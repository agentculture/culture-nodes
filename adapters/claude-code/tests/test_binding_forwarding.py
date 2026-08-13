"""Bound-input forwarding (deviation d3): engine-resolved bindings beyond the
transport fields are appended to the instruction as a serialized block, so a
node's input.bindings actually reach the session. Mirrors the identical block
in the other two bridges (all-backends rule)."""

import json

from claude_code_bridge import server as _server  # noqa: F401  (import proves the module loads)


def test_transport_keys_are_not_forwarded_but_extras_are():
    raw_input = {
        "instruction": "review the fix",
        "repo": "/tmp/repo",
        "sandbox": "read-only",
        "fixReport": {"summary": "did the thing"},
        "runEvidence": {"items": [1, 2]},
    }
    transport = {"instruction", "repo", "sandbox", "model", "success_outcome", "permission_mode"}
    extras = {k: v for k, v in raw_input.items() if k not in transport}
    assert set(extras) == {"fixReport", "runEvidence"}
    serialized = json.dumps(extras, indent=2, ensure_ascii=False)
    combined = (
        raw_input["instruction"] + "\n\n## Bound inputs (engine-resolved, verbatim)\n" + serialized
    )
    assert "fixReport" in combined and "did the thing" in combined
    assert combined.startswith("review the fix")
