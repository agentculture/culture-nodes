"""The common marker contract shared by every bridge."""

import pytest
from codex_bridge import stamping

MARKER = "cn1:firing-a:github.comment:" + "ab" * 24 + ":" + "cd" * 32


def test_stamp_text_embeds_the_exact_minted_marker_once():
    assert stamping.stamp_text("hello", MARKER) == f"hello\n\n{MARKER}"
    assert stamping.stamp_text(f"hello\n\n{MARKER}", MARKER) == f"hello\n\n{MARKER}"


def test_stamp_result_reports_provider_artifact_id():
    assert stamping.artifact_result("42", MARKER) == {
        "artifact_id": "42",
        "marker": MARKER,
    }


@pytest.mark.parametrize(
    "marker", ["legacy", "cn1:a:b:x:y", "cn1:a:b:" + "00" * 24 + ":" + "zz" * 32]
)
def test_malformed_marker_is_refused(marker):
    with pytest.raises(ValueError, match="marker"):
        stamping.stamp_text("hello", marker)
