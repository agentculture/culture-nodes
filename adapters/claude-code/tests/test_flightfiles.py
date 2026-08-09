from __future__ import annotations

import json

from claude_code_bridge import flightfiles


def test_write_stop_creates_control_file_with_stop_true(tmp_path):
    flightfiles.write_stop(tmp_path, "inv1")
    data = json.loads(flightfiles.control_path(tmp_path, "inv1").read_text())
    assert data == {"stop": True, "guidance": []}


def test_write_stop_preserves_existing_guidance(tmp_path):
    cp = flightfiles.control_path(tmp_path, "inv1")
    cp.parent.mkdir(parents=True)
    cp.write_text(json.dumps({"stop": False, "guidance": ["do this first"]}))
    flightfiles.write_stop(tmp_path, "inv1")
    data = json.loads(cp.read_text())
    assert data == {"stop": True, "guidance": ["do this first"]}


def test_write_stop_is_idempotent(tmp_path):
    flightfiles.write_stop(tmp_path, "inv1")
    flightfiles.write_stop(tmp_path, "inv1")
    data = json.loads(flightfiles.control_path(tmp_path, "inv1").read_text())
    assert data["stop"] is True


def test_write_stop_survives_a_corrupt_existing_control_file(tmp_path):
    cp = flightfiles.control_path(tmp_path, "inv1")
    cp.parent.mkdir(parents=True)
    cp.write_text("not json")
    flightfiles.write_stop(tmp_path, "inv1")
    data = json.loads(cp.read_text())
    assert data["stop"] is True


def test_stop_requested_false_when_never_written(tmp_path):
    assert flightfiles.stop_requested(tmp_path, "inv1") is False


def test_stop_requested_true_after_write_stop(tmp_path):
    flightfiles.write_stop(tmp_path, "inv1")
    assert flightfiles.stop_requested(tmp_path, "inv1") is True


def test_stop_requested_false_on_corrupt_file(tmp_path):
    cp = flightfiles.control_path(tmp_path, "inv1")
    cp.parent.mkdir(parents=True)
    cp.write_text("not json")
    assert flightfiles.stop_requested(tmp_path, "inv1") is False


def test_feed_tail_reads_no_records_when_file_absent(tmp_path):
    tail = flightfiles.FeedTail(tmp_path, "inv1")
    assert tail.read_new_records() == []


def test_feed_tail_reads_only_new_complete_lines(tmp_path):
    fp = flightfiles.feed_path(tmp_path, "inv1")
    fp.parent.mkdir(parents=True)
    fp.write_text(json.dumps({"type": "system", "subtype": "progress", "note": "thinking"}) + "\n")

    tail = flightfiles.FeedTail(tmp_path, "inv1")
    first = tail.read_new_records()
    assert len(first) == 1
    assert first[0]["note"] == "thinking"

    # Nothing new yet.
    assert tail.read_new_records() == []

    with open(fp, "a") as f:
        f.write(json.dumps({"type": "result", "subtype": "success"}) + "\n")
    second = tail.read_new_records()
    assert len(second) == 1
    assert second[0]["type"] == "result"


def test_feed_tail_leaves_a_partial_trailing_line_for_next_read(tmp_path):
    fp = flightfiles.feed_path(tmp_path, "inv1")
    fp.parent.mkdir(parents=True)
    fp.write_text(json.dumps({"type": "system", "note": "a"}) + "\n")
    tail = flightfiles.FeedTail(tmp_path, "inv1")
    assert len(tail.read_new_records()) == 1

    # Simulate a write still in progress: an unterminated line.
    with open(fp, "a") as f:
        f.write('{"type": "system", "note": "partial"')
    assert tail.read_new_records() == []  # not yet a complete line

    with open(fp, "a") as f:
        f.write("}\n")
    records = tail.read_new_records()
    assert len(records) == 1
    assert records[0]["note"] == "partial"


def test_feed_tail_skips_malformed_lines_without_raising(tmp_path):
    fp = flightfiles.feed_path(tmp_path, "inv1")
    fp.parent.mkdir(parents=True)
    fp.write_text("not json\n" + json.dumps({"type": "system", "note": "a"}) + "\n")
    tail = flightfiles.FeedTail(tmp_path, "inv1")
    records = tail.read_new_records()
    assert len(records) == 1
    assert records[0]["note"] == "a"
