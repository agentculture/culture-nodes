"""The hand-turn observer's deterministic core (task t16, decision c25).

``examples/hand-turn-observer/observer.py`` is loaded by path (it is an
example, not a package) and run against the recorded PR #307 fixture. What
is proven is the recognition, not the network: the fixture carries the four
shapes the acceptance names -- the cherry-pick, the eight branch deletes,
the operator replies and the ssh checkout reset -- beside the near-misses
the rules must leave alone (a bot's commit inside a run window, the loop's
own handover commit pushed by a person, a human commit inside a window, a
bot-deleted branch, an unrelated reply). The one network helper is exercised
against ``tests/fake_api.py``.
"""

from __future__ import annotations

import importlib.util
import json
import sys
from collections import Counter
from pathlib import Path
from types import ModuleType

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[1]
EXAMPLE = ROOT / "examples" / "hand-turn-observer"
FIXTURE = ROOT / "tests" / "fixtures" / "hand-turn-observer" / "pr-307.json"


def _load(name: str) -> ModuleType:
    spec = importlib.util.spec_from_file_location(
        f"hand_turn_observer_test_{name}", EXAMPLE / f"{name}.py"
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


observer = _load("observer")


@pytest.fixture
def inputs() -> dict:
    return json.loads(FIXTURE.read_text(encoding="utf-8"))


@pytest.fixture
def definition() -> dict:
    return json.loads((EXAMPLE / "definition.json").read_text(encoding="utf-8"))


def test_definition_data_has_the_schema_core_and_names_every_stage(definition) -> None:
    assert definition["stages"] == ["dispatch", "land", "review", "cleanup"]
    for rule in definition["rules"]:
        assert {"id", "stage", "description"} <= set(rule), rule
        assert rule["stage"] in definition["stages"], rule


def test_pr_307_fixture_proposes_the_four_turn_kinds(inputs, definition) -> None:
    proposals = observer.propose(inputs, definition, "ledger_def1")
    by_rule = Counter(p["rule"] for p in proposals)
    assert by_rule == {
        "commit_without_run": 1,
        "review_fix_branch_deleted": 8,
        "operator_reply": 3,
        "ssh_checkout_reset": 1,
    }
    for proposal in proposals:
        assert proposal["work_item"] == "SCRUM-7"
        assert proposal["definition_ref"] == "ledger_def1"
        assert proposal["what"]
        assert proposal["evidence_refs"]
    stages = {p["rule"]: p["stage"] for p in proposals}
    assert stages == {
        "commit_without_run": "land",
        "review_fix_branch_deleted": "cleanup",
        "operator_reply": "review",
        "ssh_checkout_reset": "dispatch",
    }


def test_cherry_pick_is_the_only_commit_proposed(inputs, definition) -> None:
    commits = [p for p in observer.propose(inputs, definition) if p["rule"] == "commit_without_run"]
    assert len(commits) == 1
    assert commits[0]["evidence_refs"] == ["a029689c1d2e3f40"]
    assert "a029689" in commits[0]["what"]
    assert "cherry-pick" in commits[0]["what"]
    # The near-misses: a bot commit in a run window, the loop's own handover
    # commit pushed by a person, and a human commit inside an attempt window.
    whats = " ".join(p["what"] for p in commits)
    for sha in ("f1e2d3c", "0a1b2c3", "9988776"):
        assert sha not in whats


def test_branch_deletes_are_review_fix_only_and_human_only(inputs, definition) -> None:
    deletes = [
        p for p in observer.propose(inputs, definition) if p["rule"] == "review_fix_branch_deleted"
    ]
    branches = sorted(p["evidence_refs"][0] for p in deletes)
    assert branches == [f"b-delete-{i}" for i in range(1, 9)]
    whats = " ".join(p["what"] for p in deletes)
    assert "review-fix/t9" not in whats  # deleted by the bot
    assert "feat/old-spike" not in whats  # not a review-fix branch


def test_replies_and_reset_skip_bots_and_unrelated_comments(inputs, definition) -> None:
    proposals = observer.propose(inputs, definition)
    replies = [p for p in proposals if p["rule"] == "operator_reply"]
    assert sorted(p["evidence_refs"][0].rsplit("_r", 1)[1] for p in replies) == ["1", "2", "3"]
    resets = [p for p in proposals if p["rule"] == "ssh_checkout_reset"]
    assert len(resets) == 1
    assert resets[0]["evidence_refs"] == [
        "https://github.com/agentculture/culture-nodes/issues/286#issuecomment-9"
    ]
    assert resets[0]["observed_at"] == "2026-09-05T07:50:00Z"


def test_proposals_are_deterministically_ordered(inputs, definition) -> None:
    first = observer.propose(inputs, definition, "d")
    second = observer.propose(json.loads(json.dumps(inputs)), definition, "d")
    assert first == second
    keys = [(p.get("observed_at") or "", p["rule"], p["what"]) for p in first]
    assert keys == sorted(keys)


def test_unknown_rule_kind_and_unknown_stage_are_refused(inputs, definition) -> None:
    bad_kind = dict(
        definition, rules=[{"id": "x", "stage": "land", "description": "d", "kind": "telepathy"}]
    )
    with pytest.raises(ValueError, match="unknown kind"):
        observer.propose(inputs, bad_kind)
    bad_stage = dict(definition, rules=[dict(definition["rules"][0], stage="merge")])
    with pytest.raises(ValueError, match="names stage"):
        observer.propose(inputs, bad_stage)
    with pytest.raises(ValueError, match="work_item"):
        observer.propose({}, definition)


def test_to_request_is_the_create_hand_turn_body(inputs, definition) -> None:
    proposal = observer.propose(inputs, definition, "ledger_def1")[0]
    body = observer.to_request(proposal, "actor-observer")
    assert body["actor_id"] == "actor-observer"
    assert {"what", "stage", "work_item", "definition_ref", "rule", "evidence_refs"} <= set(body)
    assert "definition_ref" not in observer.to_request(observer.propose(inputs, definition)[0], "a")


def test_post_proposals_uses_the_hand_turns_route_with_the_bearer(
    fake_api, inputs, definition
) -> None:
    seen: list[tuple[dict, str | None]] = []

    def handler(h, m, q, b):
        seen.append((json.loads(b), h.headers.get("Authorization")))
        h.send_json(
            201, {"id": f"ledger_{len(seen)}", "record_type": "hand_turn", "authority": "proposed"}
        )

    fake_api.route("POST", r"/v1alpha1/hand-turns$", handler)
    fake_api.start()
    proposals = observer.propose(inputs, definition, "ledger_def1")[:2]
    records = observer.post_proposals(
        fake_api.base_url, "observer-token", "actor-observer", proposals
    )
    assert [r["id"] for r in records] == ["ledger_1", "ledger_2"]
    assert all(auth == "Bearer observer-token" for _, auth in seen)
    assert all(
        body["actor_id"] == "actor-observer" and body["work_item"] == "SCRUM-7" for body, _ in seen
    )


def test_main_prints_the_batch_without_network(capsys) -> None:
    rc = observer.main(
        [
            "--inputs",
            str(FIXTURE),
            "--definition",
            str(EXAMPLE / "definition.json"),
            "--definition-ref",
            "ledger_def1",
        ]
    )
    assert rc == 0
    out = json.loads(capsys.readouterr().out)
    assert out["work_item"] == "SCRUM-7"
    assert out["proposed"] == 13
    assert "record_ids" not in out


def test_main_reads_nodes_input_json_when_no_inputs_flag(monkeypatch, capsys) -> None:
    monkeypatch.setenv("NODES_INPUT_JSON", FIXTURE.read_text(encoding="utf-8"))
    rc = observer.main(["--definition", str(EXAMPLE / "definition.json")])
    assert rc == 0
    assert json.loads(capsys.readouterr().out)["proposed"] == 13
    monkeypatch.delenv("NODES_INPUT_JSON")
    assert observer.main(["--definition", str(EXAMPLE / "definition.json")]) == 2


def test_the_observe_node_binds_the_run_input() -> None:
    """A code node that declares no `input:` gets no NODES_INPUT_JSON.

    `internal/worker/code.go` forwards a node's RESOLVED input document, and
    `resolveNodeInput` returns the literal `{}` for a node with no binding —
    which that worker treats as nothing to forward. The observer's only other
    source is `--inputs`, a path the graph's argv does not pass, so without a
    binding every dispatched run exits 2 before reading a rule.
    """
    document = yaml.safe_load((EXAMPLE / "workflow.yaml").read_text(encoding="utf-8"))
    observe = document["spec"]["nodes"]["observe"]
    assert observe["kind"] == "code"
    assert observe["input"]["bindings"] == {"item": "/run/input"}


def test_main_reads_the_engine_bound_input_document(monkeypatch, capsys) -> None:
    # The engine forwards the resolved input, so the binding NAME wraps the
    # run input: NODES_INPUT_JSON is {"item": {...}}, not the bare document.
    payload = {"item": json.loads(FIXTURE.read_text(encoding="utf-8"))}
    monkeypatch.setenv("NODES_INPUT_JSON", json.dumps(payload))
    rc = observer.main(["--definition", str(EXAMPLE / "definition.json")])
    assert rc == 0
    out = json.loads(capsys.readouterr().out)
    assert out["work_item"] == "SCRUM-7"
    assert out["proposed"] == 13


def test_a_gh_work_item_is_url_encoded_in_the_runs_query(fake_api) -> None:
    # The transient `gh:<owner>/<repo>#<n>` form is a real work item for a PR
    # with no ticket; unescaped, everything from `#` is a URL fragment and the
    # control plane is asked about the wrong item.
    seen: list[dict] = []

    def listing(h, m, q, b):
        seen.append(q)
        h.send_json(200, {"items": []})

    fake_api.route("GET", r"/v1alpha1/runs$", listing)
    fake_api.start()
    assert observer.fetch_runs(fake_api.base_url, "gh:owner/repo#12") == []
    assert seen[0]["work_item"] == ["gh:owner/repo#12"]
