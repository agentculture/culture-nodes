"""The durable inbox store: one JSON file per task under the state dir,
surviving process restart (plan t12 acceptance: "pending tasks survive a
bridge restart — durable inbox, not in-memory")."""

from __future__ import annotations

import json

from human_inbox_bridge.store import HumanTask, TaskStore


def _task(invocation_id="hit_1", **overrides):
    fields = dict(
        invocation_id=invocation_id,
        status="pending",
        created_at="2026-08-13T00:00:00+00:00",
        instruction="review the release notes",
        run_id="run_1",
        node_run_id="nr_1",
        attempt_id="att_1",
        callback_url="http://127.0.0.1:1/events",
        callback_token="cbtok",
    )
    fields.update(overrides)
    return HumanTask(**fields)


def test_create_get_roundtrip(tmp_path):
    store = TaskStore(tmp_path / "state")
    store.save(_task())
    got = store.get("hit_1")
    assert got is not None
    assert got.instruction == "review the release notes"
    assert got.status == "pending"
    assert got.callback_token == "cbtok"
    assert got.events_sent == 0


def test_get_unknown_is_none(tmp_path):
    store = TaskStore(tmp_path / "state")
    assert store.get("hit_missing") is None


def test_read_only_store_does_not_create_state_directory(tmp_path):
    store = TaskStore(tmp_path / "missing", create=False)
    assert store.list(status="pending") == []
    assert not store.tasks_dir.exists()


def test_tasks_survive_a_new_store_instance(tmp_path):
    # The restart test at store level: a NEW TaskStore over the same
    # directory (as after a process restart) sees the same pending task.
    TaskStore(tmp_path / "state").save(_task())
    reloaded = TaskStore(tmp_path / "state")
    got = reloaded.get("hit_1")
    assert got is not None
    assert got.status == "pending"


def test_list_filters_by_status(tmp_path):
    store = TaskStore(tmp_path / "state")
    store.save(_task("hit_a", created_at="2026-08-13T00:00:01+00:00"))
    store.save(_task("hit_b", created_at="2026-08-13T00:00:02+00:00"))
    done = _task("hit_c", created_at="2026-08-13T00:00:03+00:00")
    done.status = "completed"
    store.save(done)

    pending = store.list(status="pending")
    assert [t.invocation_id for t in pending] == ["hit_a", "hit_b"]
    assert [t.invocation_id for t in store.list(status="completed")] == ["hit_c"]
    assert len(store.list()) == 3


def test_reserve_sequence_increments_and_persists(tmp_path):
    store = TaskStore(tmp_path / "state")
    store.save(_task())
    event_id, seq = store.reserve_sequence("hit_1")
    assert seq == 1
    assert event_id == "evt_hit_1_1"
    event_id2, seq2 = store.reserve_sequence("hit_1")
    assert (event_id2, seq2) == ("evt_hit_1_2", 2)
    # Persisted: a fresh store continues the count instead of reusing it.
    fresh = TaskStore(tmp_path / "state")
    assert fresh.reserve_sequence("hit_1") == ("evt_hit_1_3", 3)


def test_public_dict_never_leaks_the_callback_token(tmp_path):
    store = TaskStore(tmp_path / "state")
    store.save(_task())
    public = store.get("hit_1").public_dict()
    assert "cbtok" not in json.dumps(public)
    assert public["invocation_id"] == "hit_1"
    assert public["instruction"] == "review the release notes"
    assert public["status"] == "pending"


def test_corrupt_task_file_is_skipped_not_fatal(tmp_path):
    store = TaskStore(tmp_path / "state")
    store.save(_task())
    (store.tasks_dir / "garbage.json").write_text("{not json", encoding="utf-8")
    assert [t.invocation_id for t in store.list()] == ["hit_1"]
