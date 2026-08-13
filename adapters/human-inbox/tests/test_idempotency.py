"""Idempotency-Key replay store (PRD §20.3), mirroring the sibling
bridges' test_idempotency.py."""

from __future__ import annotations

from human_inbox_bridge.idempotency import IdempotencyStore


def test_roundtrip(tmp_path):
    store = IdempotencyStore(tmp_path)
    store.put("att_1", 202, {"invocation_id": "hit_1"}, request_fingerprint="do the thing")
    got = store.get("att_1")
    assert got is not None
    assert got.status_code == 202
    assert got.body == {"invocation_id": "hit_1"}
    assert got.request_fingerprint == "do the thing"


def test_missing_key_is_none(tmp_path):
    assert IdempotencyStore(tmp_path).get("nope") is None


def test_survives_a_new_store_instance(tmp_path):
    IdempotencyStore(tmp_path).put("att_1", 202, {"invocation_id": "hit_1"})
    got = IdempotencyStore(tmp_path).get("att_1")
    assert got is not None
    assert got.status_code == 202


def test_corrupt_file_is_none(tmp_path):
    store = IdempotencyStore(tmp_path)
    store.put("att_1", 202, {"ok": True})
    for path in (tmp_path / "idempotency").glob("*.json"):
        path.write_text("{corrupt", encoding="utf-8")
    assert store.get("att_1") is None
