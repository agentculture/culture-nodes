"""``nodes hand-turn`` (task t16, decision c25): the thin client over
``POST /v1alpha1/hand-turns``, exercised against ``tests/fake_api.py``."""

from __future__ import annotations

import json

import pytest

from culture_nodes.cli import main
from culture_nodes.explain import known_paths

RECORD = {
    "id": "ledger_ht1",
    "record_type": "hand_turn",
    "run_id": "run-9",
    "authority": "proposed",
    "origin": {"kind": "human", "actor_id": "actor-ori"},
    "data": {
        "what": "cherry-picked a029689",
        "stage": "land",
        "work_item": "SCRUM-9",
        "definition_ref": None,
    },
}


def _route(fake_api, seen, status=201, payload=RECORD):
    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        seen["auth"] = h.headers.get("Authorization")
        h.send_json(status, payload)

    fake_api.route("POST", r"/v1alpha1/hand-turns$", handler)
    fake_api.start()


def test_hand_turn_text(fake_api, capsys, monkeypatch) -> None:
    monkeypatch.delenv("NODES_HUMAN_DECISION_TOKEN", raising=False)
    seen: dict = {}
    _route(fake_api, seen)
    rc = main(
        [
            "hand-turn",
            "cherry-picked a029689",
            "--stage",
            "land",
            "--work-item",
            "SCRUM-9",
            "--as",
            "actor-ori",
            "--token",
            "secret-token",
            "--api-url",
            fake_api.base_url,
        ]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert seen["body"] == {
        "what": "cherry-picked a029689",
        "stage": "land",
        "work_item": "SCRUM-9",
        "actor_id": "actor-ori",
    }
    assert seen["auth"] == "Bearer secret-token"
    assert "id: ledger_ht1" in out
    assert "authority: proposed" in out
    assert "origin: human (actor-ori)" in out
    assert "run_id: run-9" in out
    assert "stage: land" in out
    assert "work_item: SCRUM-9" in out
    assert "review create" in out
    assert "secret-token" not in out


def test_hand_turn_optional_fields_and_json(fake_api, capsys, monkeypatch) -> None:
    monkeypatch.setenv("NODES_HUMAN_DECISION_TOKEN", "env-token")
    monkeypatch.setenv("NODES_HAND_TURN_ACTOR_ID", "actor-observer")
    seen: dict = {}
    _route(fake_api, seen)
    rc = main(
        [
            "hand-turn",
            "deleted review-fix/t3",
            "--stage",
            "cleanup",
            "--work-item",
            "SCRUM-9",
            "--run",
            "run-9",
            "--definition-ref",
            "ledger_def1",
            "--rule",
            "review_fix_branch_deleted",
            "--evidence",
            "b-delete-3",
            "--evidence",
            "https://example.com/pr/307",
            "--json",
            "--api-url",
            fake_api.base_url,
        ]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert seen["auth"] == "Bearer env-token"
    assert seen["body"] == {
        "what": "deleted review-fix/t3",
        "stage": "cleanup",
        "work_item": "SCRUM-9",
        "actor_id": "actor-observer",
        "run_id": "run-9",
        "definition_ref": "ledger_def1",
        "rule": "review_fix_branch_deleted",
        "evidence_refs": ["b-delete-3", "https://example.com/pr/307"],
    }
    assert json.loads(out) == RECORD


def test_hand_turn_requires_stage_and_work_item(capsys) -> None:
    with pytest.raises(SystemExit) as exc:
        main(["hand-turn", "x", "--work-item", "SCRUM-9"])
    assert exc.value.code == 1
    err = capsys.readouterr().err
    assert err.startswith("error:")
    assert "hint:" in err


def test_hand_turn_missing_token_and_actor_are_structured_errors(capsys, monkeypatch) -> None:
    monkeypatch.delenv("NODES_HUMAN_DECISION_TOKEN", raising=False)
    monkeypatch.delenv("NODES_HAND_TURN_ACTOR_ID", raising=False)
    rc = main(["hand-turn", "x", "--stage", "land", "--work-item", "SCRUM-9", "--token", "t"])
    captured = capsys.readouterr()
    assert rc == 1
    assert "no actor" in captured.err
    assert "NODES_HAND_TURN_ACTOR_ID" in captured.err
    rc = main(["hand-turn", "x", "--stage", "land", "--work-item", "SCRUM-9", "--as", "a"])
    captured = capsys.readouterr()
    assert rc == 1
    assert "hint:" in captured.err
    assert "NODES_HUMAN_DECISION_TOKEN" in captured.err


def test_hand_turn_relays_api_404_verbatim(fake_api, capsys) -> None:
    seen: dict = {}
    _route(
        fake_api,
        seen,
        status=404,
        payload={
            "code": 1,
            "message": 'no run in this namespace carries work_item "SCRUM-404"',
            "remediation": "create the work item's run first, or pass run_id",
        },
    )
    rc = main(
        [
            "hand-turn",
            "x",
            "--stage",
            "land",
            "--work-item",
            "SCRUM-404",
            "--as",
            "a",
            "--token",
            "t",
            "--api-url",
            fake_api.base_url,
        ]
    )
    captured = capsys.readouterr()
    assert rc == 1
    assert "SCRUM-404" in captured.err
    assert "hint:" in captured.err


def test_hand_turn_has_a_catalog_entry() -> None:
    assert ("hand-turn",) in set(known_paths())
