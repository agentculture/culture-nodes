"""The recorded cutover decision must list every non-dispatch consumer of
`actors.endpoint_ref` (task t7, issues #121/#136).

`docs/decisions/transport-inversion.md` names the consumers that have to be
converted before migration 0036 drops the column. The list was incomplete:
the challenge pass found a third, `adapters/human-inbox/.../tracker.py`,
whose startup guard reads a registered `endpoint_ref` and refuses to run
when it does not match the tracker's own bridge URL — so dropping the column
would have stopped a live systemd unit from starting.

These tests keep the document and the code honest about each other. A
consumer list that is only true on the day it is written is the thing that
produced this task.
"""

from __future__ import annotations

from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
DECISION = REPO_ROOT / "docs" / "decisions" / "transport-inversion.md"
_BRIDGE_PKG = REPO_ROOT / "adapters" / "human-inbox" / "src" / "human_inbox_bridge"
#: Both halves of the tracker: the guard itself was split into
#: tracker_identity.py, and the consumer entry is only true if neither reads
#: an address.
TRACKER_MODULES = (_BRIDGE_PKG / "tracker.py", _BRIDGE_PKG / "tracker_identity.py")


def _consumer_section() -> str:
    """The paragraph listing the non-dispatch consumers, lowercased."""
    text = DECISION.read_text(encoding="utf-8")
    marker = "Two non-dispatch consumers"
    alt = "non-dispatch consumers"
    start = text.find(marker)
    if start < 0:
        start = text.find(alt)
    assert start >= 0, "transport-inversion.md no longer lists non-dispatch consumers"
    end = text.find("\n## ", start)
    return text[start : end if end > 0 else len(text)].lower()


def test_the_consumer_list_names_the_human_inbox_tracker():
    section = _consumer_section()
    assert "human_inbox_bridge/tracker.py" in section
    # Naming the file is not enough: the entry has to say what replaced the
    # address, or the next reader learns only that something was here.
    assert "presence" in section


def test_the_consumer_list_still_names_the_two_it_always_did():
    section = _consumer_section()
    assert "internal/worker/registry.go" in section
    assert "scripts/collect-handover.py" in section


def test_the_tracker_no_longer_reads_endpoint_ref():
    """The other half of the same fact. If this fails, the consumer entry
    describing the tracker as converted has become false.

    Prose may still name the column — the module explains what it stopped
    doing and why. What must be gone is every way of READING one: an
    attribute access on a registration row, and a lookup of the JSON key.
    """
    for module in TRACKER_MODULES:
        source = module.read_text(encoding="utf-8")
        for read in (".endpoint_ref", '"endpoint_ref"', "'endpoint_ref'"):
            assert read not in source, f"{module.name} still reads an address via {read}"
