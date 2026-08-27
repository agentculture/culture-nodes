from __future__ import annotations

from qwen_bridge.idempotency import IdempotencyStore


def test_get_on_unknown_key_is_none(tmp_path):
    store = IdempotencyStore(tmp_path)
    assert store.get("nope") is None


def test_put_then_get_replays_the_same_response(tmp_path):
    store = IdempotencyStore(tmp_path)
    store.put("att_1", 200, {"outcome": "completed"}, request_fingerprint="do the thing")
    replay = store.get("att_1")
    assert replay is not None
    assert replay.status_code == 200
    assert replay.body == {"outcome": "completed"}
    assert replay.request_fingerprint == "do the thing"


def test_put_overwrites_a_prior_record_for_the_same_key(tmp_path):
    store = IdempotencyStore(tmp_path)
    store.put("att_1", 202, {"invocation_id": "x"})
    store.put("att_1", 202, {"invocation_id": "y"})
    replay = store.get("att_1")
    assert replay.body == {"invocation_id": "y"}


def test_survives_a_fresh_store_instance_over_the_same_dir(tmp_path):
    IdempotencyStore(tmp_path).put("att_1", 200, {"outcome": "completed"})
    reopened = IdempotencyStore(tmp_path)
    replay = reopened.get("att_1")
    assert replay is not None
    assert replay.status_code == 200


def test_different_keys_do_not_collide(tmp_path):
    store = IdempotencyStore(tmp_path)
    store.put("att_1", 200, {"a": 1})
    store.put("att_2", 202, {"b": 2})
    assert store.get("att_1").body == {"a": 1}
    assert store.get("att_2").body == {"b": 2}


def test_odd_key_shapes_do_not_escape_the_state_dir(tmp_path):
    store = IdempotencyStore(tmp_path)
    weird_key = "../../etc/passwd"
    store.put(weird_key, 200, {"ok": True})
    assert store.get(weird_key).body == {"ok": True}
    # Nothing was written outside the store's own directory.
    assert not (tmp_path.parent / "etc").exists()


def test_corrupt_replay_file_is_treated_as_a_cache_miss(tmp_path):
    store = IdempotencyStore(tmp_path)
    store.put("att_1", 200, {"ok": True})
    # Corrupt the underlying file directly.
    idem_dir = tmp_path / "idempotency"
    for f in idem_dir.glob("*.json"):
        f.write_text("not json")
    assert store.get("att_1") is None
