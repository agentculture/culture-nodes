"""Idempotency-Key replay store (PRD §20.3), mirroring the sibling
bridges' test_idempotency.py."""

from __future__ import annotations

from notify_bridge.idempotency import IdempotencyStore


def test_roundtrip(tmp_path):
    store = IdempotencyStore(tmp_path)
    store.put("att_1", 200, {"outcome": "sent"}, request_fingerprint="att_1")
    got = store.get("att_1")
    assert got is not None
    assert got.status_code == 200
    assert got.body == {"outcome": "sent"}
    assert got.request_fingerprint == "att_1"


def test_missing_key_is_none(tmp_path):
    assert IdempotencyStore(tmp_path).get("nope") is None


def test_survives_a_new_store_instance(tmp_path):
    IdempotencyStore(tmp_path).put("att_1", 200, {"outcome": "sent"})
    got = IdempotencyStore(tmp_path).get("att_1")
    assert got is not None
    assert got.status_code == 200


def test_corrupt_file_is_none(tmp_path):
    store = IdempotencyStore(tmp_path)
    store.put("att_1", 200, {"ok": True})
    for path in (tmp_path / "idempotency").glob("*.json"):
        path.write_text("{corrupt", encoding="utf-8")
    assert store.get("att_1") is None
