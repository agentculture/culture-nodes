"""The measurement runner (task t11, plan
``docs/plans/2026-09-05-harness-hardening-and-compare.md``).

These tests drive ``examples/harness-compare/measurements/run.py`` end to
end against a **fake control plane** (``tests/fake_api.py``, a stdlib
``http.server``) and **fake bridges** (the same server class, serving
``/v1/capabilities``). Nothing here talks to a real API, a real bridge or a
real model; what is proven is the runner's own behaviour, which is the half
that can be wrong quietly:

1. the revision gate refuses a stale actor **by name** (c30 / h24);
2. runs are created strictly ONE AT A TIME — the fake records concurrency,
   so a future ``--parallel`` could not slip in unnoticed;
3. every run carries ``category`` = the rule id and
   ``input.measurement`` = {manifest digest, rule id}, plus the bridge
   revisions and sandbox in ``measurement_context``;
4. each of the three check kinds yields the anchored 5 / 3 / 1 rating;
5. every grade names the **agent** principal, and no grade is ever posted
   with the registered human's id (c29 / h28);
6. the JSON Lines report is written, and re-running appends;
7. the Access cookie never appears in anything the runner prints.
"""

from __future__ import annotations

import importlib.util
import json
import os
import sys
from pathlib import Path
from types import ModuleType

import pytest

from tests.fake_api import FakeNodesAPI

ROOT = Path(__file__).resolve().parents[1]
MEASUREMENTS_DIR = ROOT / "examples" / "harness-compare" / "measurements"

COOKIE = "cookie-value-that-must-never-be-printed"
AGENT_ACTOR_ID = "actor-agent-measure-runner"
HUMAN_ACTOR_ID = "actor-human-operator"
# Kinds the registry carries that are neither human nor agent. The grade API
# maps only human and agent to a ledger origin (internal/api/grades.go), so
# these must be refused BEFORE the billable pass, not after it.
NON_AGENT_ACTOR_IDS = {
    "engine": "actor-engine-control-plane",
    "validator": "actor-validator-merge-gate",
    "runner": "actor-runner-headspace",
}
GOOD_REVISION = "a" * 40
STALE_REVISION = "b" * 40


def _load(name: str) -> ModuleType:
    spec = importlib.util.spec_from_file_location(
        f"harness_compare_measurement_test_{name}", MEASUREMENTS_DIR / f"{name}.py"
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


runner = _load("run")
checks = _load("checks")


# ---------------------------------------------------------------------------
# Fakes
# ---------------------------------------------------------------------------


class FakeControlPlane:
    """A fake ``nodes serve`` that answers the six calls the runner makes.

    It also *watches* the runner: ``max_concurrent_runs`` is the high-water
    mark of runs that had been created but had not yet reached a terminal
    state when the next one was created. A serial runner can never push that
    above 1, and that is the assertion the serial constraint rests on.
    """

    def __init__(self, actors, summaries, polls_to_completion: int = 2) -> None:
        self.api = FakeNodesAPI()
        self.actors = actors
        self.summaries = summaries
        self.polls_to_completion = polls_to_completion
        self.runs: dict[str, dict] = {}
        self.open_runs: set[str] = set()
        self.max_concurrent_runs = 0
        self.grades: list[dict] = []
        self.created: list[dict] = []
        self.publishes = 0
        self.api.route("GET", r"/v1alpha1/actors", self._list_actors)
        self.api.route("POST", r"/v1alpha1/workflows", self._publish)
        self.api.route("POST", r"/v1alpha1/runs", self._create_run)
        self.api.route("GET", r"/v1alpha1/runs/([^/]+)", self._get_run)
        self.api.route("POST", r"/v1alpha1/runs/([^/]+)/grades", self._grade)
        self.api.start()

    @property
    def base_url(self) -> str:
        return self.api.base_url

    def stop(self) -> None:
        self.api.stop()

    # -- routes ------------------------------------------------------------

    def _list_actors(self, handler, match, query, body):
        handler.send_json(200, {"items": self.actors})

    def _publish(self, handler, match, query, body):
        self.publishes += 1
        handler.send_json(200, {"digest": "sha256:" + "c" * 64, "valid": True})

    def _create_run(self, handler, match, query, body):
        payload = json.loads(body)
        self.created.append(payload)
        run_id = f"run-{len(self.created):03d}"
        self.open_runs.add(run_id)
        self.max_concurrent_runs = max(self.max_concurrent_runs, len(self.open_runs))
        slot = next(iter(payload["input"]["actors"]))
        self.runs[run_id] = {
            "polls": 0,
            "slot": slot,
            "rule_id": payload["input"]["measurement"]["rule_id"],
            "category": payload.get("category", ""),
        }
        handler.send_json(200, {"id": run_id, "state": "running"})

    def _get_run(self, handler, match, query, body):
        run_id = match.group(1)
        state = self.runs[run_id]
        state["polls"] += 1
        slot, rule_id = state["slot"], state["rule_id"]
        actor_id = self._actor_id_for_slot(slot)
        if state["polls"] < self.polls_to_completion:
            handler.send_json(
                200,
                {
                    "run": {"id": run_id, "state": "running"},
                    "node_runs": [{"node_id": slot, "state": "running", "attempts": []}],
                },
            )
            return
        self.open_runs.discard(run_id)
        summary = self.summaries.get((slot, rule_id), self.summaries.get(rule_id, ""))
        run_state = "failed" if summary is None else "completed"
        output = (
            None
            if summary is None
            else [{"from_node": slot, "outcome": "completed", "output": {"summary": summary}}]
        )
        handler.send_json(
            200,
            {
                "run": {"id": run_id, "state": run_state, "output": output},
                "node_runs": [
                    {
                        "node_id": slot,
                        "state": "completed",
                        "outcome": "completed",
                        "attempts": [{"actor_id": actor_id, "status": "succeeded"}],
                    }
                ],
            },
        )

    def _grade(self, handler, match, query, body):
        payload = json.loads(body)
        payload["run_id"] = match.group(1)
        self.grades.append(payload)
        handler.send_json(
            200,
            {
                "id": f"grade-{len(self.grades):03d}",
                "authority": "proposed",
                "origin": {"kind": "agent"},
                "data": {"rating": payload["rating"]},
            },
        )

    def _actor_id_for_slot(self, slot: str) -> str:
        """Whoever the graph's slot would actually resolve to: the NEWEST
        registration revision of the matching key, exactly as the worker's
        registry resolves an `actor://` reference."""
        best = None
        for row in self.actors:
            key = row.get("actor_key", "")
            if key.split("/")[-1].split("-")[0] != slot:
                continue
            if best is None or int(row.get("revision") or 0) >= int(best.get("revision") or 0):
                best = row
        return best["id"] if best else ""


class FakeBridge:
    """A bridge serving the capability document's ``deployment`` block."""

    def __init__(self, revision: str, install_mode: str = "copy", dirty: bool = False) -> None:
        self.api = FakeNodesAPI()
        self.block = {
            "revision": revision,
            "install_mode": install_mode,
            "revision_is_dirty": dirty,
            "revision_source": "stamp",
        }
        self.api.route("GET", r"/v1/capabilities", self._capabilities)
        self.api.start()

    @property
    def base_url(self) -> str:
        return self.api.base_url

    def stop(self) -> None:
        self.api.stop()

    def _capabilities(self, handler, match, query, body):
        handler.send_json(
            200,
            {"preflight": {"protocol_version": "1.0", "host": {"deployment": self.block}}},
        )


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


def _manifest(rules) -> dict:
    return {
        "version": 1,
        "actors": ["company/pi-thor", "company/qwen-thor"],
        "rules": rules,
    }


def _rule(rule_id, category, kind, expect, runs_per_actor=1) -> dict:
    return {
        "id": rule_id,
        "category": category,
        "instruction": f"do {rule_id}",
        "sandbox": "read-only",
        "check": {"kind": kind, "expect": expect},
        "anchors": {"5": "cited", "3": "uncited", "1": "absent"},
        "runs_per_actor": runs_per_actor,
    }


LOCATE_RULE = _rule(
    "locate-exit-code-policy", "locate", "grep-cites-file-line", "culture_nodes/cli/_errors.py"
)
REVIEW_RULE = _rule("review-seeded-defect", "review", "seeded-defect-named", "500")
EXPLAIN_RULE = _rule("explain-output-tests", "explain", "tests-named", "tests/test_cli.py")


@pytest.fixture
def bridges():
    made = []

    def make(revision: str, **kwargs) -> FakeBridge:
        bridge = FakeBridge(revision, **kwargs)
        made.append(bridge)
        return bridge

    yield make
    for bridge in made:
        bridge.stop()


@pytest.fixture
def fleet(bridges):
    """Two agent actors on fresh bridges, plus a registered human actor."""
    pi = bridges(GOOD_REVISION)
    qwen = bridges(GOOD_REVISION)
    actors = [
        # An older revision row for the same key: the runner must use the
        # newest (registration is append-only, internal/api/actors.go).
        {
            "id": "actor-pi-thor-old",
            "actor_key": "company/pi-thor",
            "revision": 1,
            "kind": "agent",
            "endpoint_ref": "http://127.0.0.1:1",
        },
        {
            "id": "actor-pi-thor",
            "actor_key": "company/pi-thor",
            "revision": 2,
            "kind": "agent",
            "endpoint_ref": pi.base_url,
        },
        {
            "id": "actor-qwen-thor",
            "actor_key": "company/qwen-thor",
            "revision": 1,
            "kind": "agent",
            "endpoint_ref": qwen.base_url,
        },
        {"id": HUMAN_ACTOR_ID, "actor_key": "company/operator", "revision": 1, "kind": "human"},
        {
            "id": AGENT_ACTOR_ID,
            "actor_key": "company/measure-runner",
            "revision": 1,
            "kind": "agent",
        },
    ]
    actors.extend(
        {
            "id": actor_id,
            "actor_key": f"company/{kind}",
            "revision": 1,
            "kind": kind,
        }
        for kind, actor_id in NON_AGENT_ACTOR_IDS.items()
    )
    return {"actors": actors, "pi": pi, "qwen": qwen}


def _write_manifest(tmp_path: Path, manifest: dict) -> Path:
    path = tmp_path / "manifest.json"
    path.write_text(json.dumps(manifest), encoding="utf-8")
    return path


def _invoke(
    monkeypatch, capsys, control, manifest_path, tmp_path, *extra, principal=AGENT_ACTOR_ID
):
    monkeypatch.setenv("NODES_API_URL", control.base_url)
    monkeypatch.setenv("NODES_OP_COOKIE", COOKIE)
    if principal is None:
        monkeypatch.delenv("MEASURE_RUNNER_ACTOR_ID", raising=False)
    else:
        monkeypatch.setenv("MEASURE_RUNNER_ACTOR_ID", principal)
    argv = [
        "--manifest",
        str(manifest_path),
        "--workflow",
        str(ROOT / "examples" / "harness-compare" / "workflow.yaml"),
        "--repo-map",
        "pi=/home/culture-pi/git/culture-nodes-agent",
        "--repo-map",
        "qwen=/home/culture-qwen/git/culture-nodes-agent",
        "--poll-interval",
        "0",
        *extra,
    ]
    code = runner.main(argv)
    captured = capsys.readouterr()
    return code, captured


# ---------------------------------------------------------------------------
# 1. The revision gate (c30 / h24)
# ---------------------------------------------------------------------------


def test_stale_revision_refuses_and_names_the_actor(monkeypatch, capsys, tmp_path, bridges):
    pi = bridges(GOOD_REVISION)
    qwen = bridges(STALE_REVISION)
    actors = [
        {
            "id": "actor-pi-thor",
            "actor_key": "company/pi-thor",
            "revision": 1,
            "kind": "agent",
            "endpoint_ref": pi.base_url,
        },
        {
            "id": "actor-qwen-thor",
            "actor_key": "company/qwen-thor",
            "revision": 1,
            "kind": "agent",
            "endpoint_ref": qwen.base_url,
        },
        {
            "id": AGENT_ACTOR_ID,
            "actor_key": "company/measure-runner",
            "revision": 1,
            "kind": "agent",
        },
    ]
    control = FakeControlPlane(actors, {LOCATE_RULE["id"]: "x"})
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, captured = _invoke(
            monkeypatch,
            capsys,
            control,
            path,
            tmp_path,
            "--expect-revision",
            GOOD_REVISION,
            "--yes",
        )
        assert code != 0
        assert "company/qwen-thor" in captured.err
        assert STALE_REVISION in captured.err
        # The healthy actor is NOT named as stale.
        assert "company/pi-thor is running" not in captured.err
        # Nothing was dispatched: the gate runs before any billable session.
        assert control.created == []
        assert control.grades == []
    finally:
        control.stop()


def test_without_expect_revision_nothing_is_refused_and_what_was_seen_is_recorded(
    monkeypatch, capsys, tmp_path, fleet
):
    control = FakeControlPlane(
        fleet["actors"], {LOCATE_RULE["id"]: "culture_nodes/cli/_errors.py:21 says 0/1/2"}
    )
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, captured = _invoke(monkeypatch, capsys, control, path, tmp_path, "--yes")
        assert code == 0
        assert GOOD_REVISION in captured.out
        for payload in control.created:
            revisions = payload["input"]["measurement_context"]["bridge_revisions"]
            assert revisions["company/pi-thor"] == GOOD_REVISION
            assert revisions["company/qwen-thor"] == GOOD_REVISION
    finally:
        control.stop()


def test_gate_only_reports_without_dispatching(monkeypatch, capsys, tmp_path, fleet):
    control = FakeControlPlane(fleet["actors"], {LOCATE_RULE["id"]: "x"})
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, captured = _invoke(monkeypatch, capsys, control, path, tmp_path, "--gate-only")
        assert code == 0
        assert control.created == []
        assert "install_mode=copy" in captured.out
    finally:
        control.stop()


def test_extract_deployment_reads_the_host_block_and_the_bare_form():
    nested = {"preflight": {"host": {"deployment": {"revision": "abc"}}}}
    assert runner.extract_deployment(nested) == {"revision": "abc"}
    assert runner.extract_deployment({"deployment": {"revision": "def"}}) == {"revision": "def"}
    assert runner.extract_deployment({"preflight": {"host": {}}}) == {}


def test_newest_actor_revision_wins(fleet):
    newest = runner.newest_actor_rows(fleet["actors"])
    assert newest["company/pi-thor"]["id"] == "actor-pi-thor"


# ---------------------------------------------------------------------------
# 2. Serial dispatch
# ---------------------------------------------------------------------------


def test_runs_are_created_one_at_a_time(monkeypatch, capsys, tmp_path, fleet):
    rules = [
        _rule("locate-a", "locate", "grep-cites-file-line", "a.py", runs_per_actor=2),
        _rule("review-b", "review", "seeded-defect-named", "500", runs_per_actor=2),
    ]
    summaries = {"locate-a": "see a.py:10", "review-b": "the 500 boundary at line 179"}
    control = FakeControlPlane(fleet["actors"], summaries, polls_to_completion=3)
    try:
        path = _write_manifest(tmp_path, _manifest(rules))
        code, _ = _invoke(monkeypatch, capsys, control, path, tmp_path, "--yes")
        assert code == 0
        # 2 rules x 2 actors x 2 repeats.
        assert len(control.created) == 8
        assert control.max_concurrent_runs == 1
    finally:
        control.stop()


def test_no_parallel_flag_exists():
    """The serial constraint is structural, not a default that can be flipped."""
    with pytest.raises(SystemExit):
        runner.build_parser().parse_args(["--parallel"])


# ---------------------------------------------------------------------------
# 3. Category and measurement fields
# ---------------------------------------------------------------------------


def test_category_and_measurement_fields_are_set(monkeypatch, capsys, tmp_path, fleet):
    control = FakeControlPlane(
        fleet["actors"], {LOCATE_RULE["id"]: "culture_nodes/cli/_errors.py:21"}
    )
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, _ = _invoke(monkeypatch, capsys, control, path, tmp_path, "--yes")
        assert code == 0
        digests = set()
        for payload in control.created:
            assert payload["category"] == LOCATE_RULE["id"]
            measurement = payload["input"]["measurement"]
            assert measurement["rule_id"] == LOCATE_RULE["id"]
            assert measurement["manifest_digest"].startswith("sha256:")
            assert set(measurement) == {"manifest_digest", "rule_id"}
            digests.add(measurement["manifest_digest"])
            assert payload["input"]["sandbox"] == "read-only"
            assert payload["input"]["measurement_context"]["sandbox"] == "read-only"
            assert len(payload["input"]["actors"]) == 1
        assert len(digests) == 1
        # Every grade is filed under the rule id too (decision q9).
        assert {g["category"] for g in control.grades} == {LOCATE_RULE["id"]}
    finally:
        control.stop()


def test_qwen_slot_carries_a_mode_and_pi_does_not(monkeypatch, capsys, tmp_path, fleet):
    control = FakeControlPlane(fleet["actors"], {LOCATE_RULE["id"]: "x"})
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, _ = _invoke(
            monkeypatch, capsys, control, path, tmp_path, "--yes", "--qwen-mode", "plan"
        )
        assert code == 0
        by_slot = {next(iter(p["input"]["actors"])): p["input"]["actors"] for p in control.created}
        assert by_slot["qwen"]["qwen"]["mode"] == "plan"
        assert "mode" not in by_slot["pi"]["pi"]
        assert by_slot["pi"]["pi"]["repo"] == "/home/culture-pi/git/culture-nodes-agent"
    finally:
        control.stop()


def test_a_missing_repo_path_is_refused_rather_than_guessed(monkeypatch, capsys, tmp_path, fleet):
    control = FakeControlPlane(fleet["actors"], {LOCATE_RULE["id"]: "x"})
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        monkeypatch.setenv("NODES_API_URL", control.base_url)
        monkeypatch.setenv("NODES_OP_COOKIE", COOKIE)
        monkeypatch.setenv("MEASURE_RUNNER_ACTOR_ID", AGENT_ACTOR_ID)
        code = runner.main(["--manifest", str(path), "--poll-interval", "0", "--yes"])
        captured = capsys.readouterr()
        assert code == 1
        assert "no checkout path" in captured.err
        assert "--repo-map" in captured.err
    finally:
        control.stop()


def test_two_actors_on_one_slot_are_refused(monkeypatch, capsys, tmp_path, bridges):
    """company/pi-thor and company/pi-orin both map to slot `pi`, whose
    `uses:` is a static registry id — measuring both through this graph
    would run one host twice and label one of them the other."""
    pi = bridges(GOOD_REVISION)
    actors = [
        {
            "id": f"actor-pi-{host}",
            "actor_key": f"company/pi-{host}",
            "revision": 1,
            "kind": "agent",
            "endpoint_ref": pi.base_url,
        }
        for host in ("thor", "orin")
    ] + [
        {
            "id": AGENT_ACTOR_ID,
            "actor_key": "company/measure-runner",
            "revision": 1,
            "kind": "agent",
        }
    ]
    control = FakeControlPlane(actors, {LOCATE_RULE["id"]: "x"})
    try:
        manifest = _manifest([LOCATE_RULE])
        manifest["actors"] = ["company/pi-thor", "company/pi-orin"]
        path = _write_manifest(tmp_path, manifest)
        code, captured = _invoke(monkeypatch, capsys, control, path, tmp_path, "--yes")
        assert code == 1
        assert "company/pi-orin" in captured.err and "company/pi-thor" in captured.err
        assert control.created == []
    finally:
        control.stop()


def test_slot_mapping_and_its_refusal():
    assert runner.slot_for("company/pi-thor") == "pi"
    assert runner.slot_for("company/qwen-orin") == "qwen"
    assert runner.slot_for("company/codex-thor") == "codex"
    assert runner.slot_for("company/colleague-spark") == "colleague"
    assert runner.slot_for("company/developer") == "claude"
    assert runner.slot_for("company/mystery", {"company/mystery": "pi"}) == "pi"
    with pytest.raises(runner.RunnerError):
        runner.slot_for("company/mystery")


# ---------------------------------------------------------------------------
# 4. The three check kinds and their anchored ratings
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "kind,expect,summary,rating,verdict",
    [
        # grep-cites-file-line: a path:line citation of the expected file.
        (
            "grep-cites-file-line",
            "culture_nodes/cli/_errors.py",
            "The policy is in culture_nodes/cli/_errors.py:21 — 0/1/2.",
            5,
            "cited",
        ),
        (
            "grep-cites-file-line",
            "culture_nodes/cli/_errors.py",
            "It lives in culture_nodes/cli/_errors.py; 0 success, 1 user, 2 env.",
            3,
            "uncited",
        ),
        (
            "grep-cites-file-line",
            "culture_nodes/cli/_errors.py",
            "Exit codes are defined in culture_nodes/cli/__init__.py:9.",
            1,
            "absent",
        ),
        # seeded-defect-named: the token plus something specific.
        (
            "seeded-defect-named",
            "500",
            "The defect is `status <= 500` on line 179: HTTP 500 is misrouted.",
            5,
            "cited",
        ),
        ("seeded-defect-named", "500", "Something changed around 500.", 3, "uncited"),
        ("seeded-defect-named", "500", "The retry backoff was removed.", 1, "absent"),
        # tests-named: the file plus which assertions.
        (
            "tests-named",
            "tests/test_cli.py",
            "tests/test_cli.py — see test_error_renders_hint for the hint: line.",
            5,
            "cited",
        ),
        ("tests-named", "tests/test_cli.py", "tests/test_cli.py covers it.", 3, "uncited"),
        ("tests-named", "tests/test_cli.py", "Nothing tests _output.py.", 1, "absent"),
    ],
)
def test_check_kinds_yield_the_anchored_rating(kind, expect, summary, rating, verdict):
    result = checks.rate(kind, expect, summary)
    assert result["rating"] == rating
    assert result["verdict"] == verdict


def test_padding_drops_a_cited_answer_to_three():
    summary = (
        "culture_nodes/cli/_errors.py:21 defines it, but see also a.py, b.py, "
        "c.py and d.py which are all relevant."
    )
    result = checks.rate("grep-cites-file-line", "culture_nodes/cli/_errors.py", summary)
    assert result["rating"] == 3
    assert result["verdict"] == "padded"


def test_no_answer_rates_one():
    result = checks.rate("tests-named", "tests/test_cli.py", None)
    assert result["rating"] == 1
    assert result["verdict"] == checks.FAILED_RUN


def test_unknown_check_kind_is_refused():
    with pytest.raises(checks.CheckError):
        checks.rate("vibes", "x", "y")


def test_fabrication_flag_names_paths_that_do_not_exist(tmp_path):
    (tmp_path / "real.py").write_text("x", encoding="utf-8")
    missing = checks.fabricated_paths("see real.py and imaginary_module.py", [tmp_path])
    assert missing == ["imaginary_module.py"]


def test_a_failed_run_rates_one_and_says_so(monkeypatch, capsys, tmp_path, fleet):
    control = FakeControlPlane(fleet["actors"], {LOCATE_RULE["id"]: None})
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, _ = _invoke(monkeypatch, capsys, control, path, tmp_path, "--yes")
        assert code == 0
        assert {g["rating"] for g in control.grades} == {1}
        assert all("ended failed" in g["rationale"] for g in control.grades)
    finally:
        control.stop()


def test_grade_notes_carry_every_fact_h28_asks_for(monkeypatch, capsys, tmp_path, fleet):
    summary = "culture_nodes/cli/_errors.py:21 defines the policy."
    control = FakeControlPlane(fleet["actors"], {LOCATE_RULE["id"]: summary})
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, _ = _invoke(monkeypatch, capsys, control, path, tmp_path, "--yes")
        assert code == 0
        notes = control.grades[0]["rationale"]
        assert f"rule={LOCATE_RULE['id']}" in notes
        assert "manifest_digest=sha256:" in notes
        assert "check=grep-cites-file-line" in notes
        assert "culture_nodes/cli/_errors.py" in notes
        assert "verdict=cited" in notes
        assert "fabrication_flag=false" in notes
        assert f"bridge_revision={GOOD_REVISION}" in notes
    finally:
        control.stop()


# ---------------------------------------------------------------------------
# 5. The grading principal (c29 / h28)
# ---------------------------------------------------------------------------


def test_every_grade_names_the_agent_principal_and_never_the_human(
    monkeypatch, capsys, tmp_path, fleet
):
    summaries = {
        LOCATE_RULE["id"]: "culture_nodes/cli/_errors.py:21",
        REVIEW_RULE["id"]: "`status <= 500` on line 179",
        EXPLAIN_RULE["id"]: "tests/test_cli.py, test_error_renders_hint",
    }
    control = FakeControlPlane(fleet["actors"], summaries)
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE, REVIEW_RULE, EXPLAIN_RULE]))
        code, _ = _invoke(monkeypatch, capsys, control, path, tmp_path, "--yes")
        assert code == 0
        assert len(control.grades) == 6
        assert {g["grading_actor_id"] for g in control.grades} == {AGENT_ACTOR_ID}
        assert HUMAN_ACTOR_ID not in {g["grading_actor_id"] for g in control.grades}
        assert HUMAN_ACTOR_ID not in {g["evaluated_actor_id"] for g in control.grades}
        assert {g["evaluated_actor_id"] for g in control.grades} == {
            "actor-pi-thor",
            "actor-qwen-thor",
        }
        assert {g["rating"] for g in control.grades} == {5}
    finally:
        control.stop()


def test_an_unset_principal_refuses_before_any_network_call(monkeypatch, capsys, tmp_path, fleet):
    control = FakeControlPlane(fleet["actors"], {LOCATE_RULE["id"]: "x"})
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, captured = _invoke(
            monkeypatch, capsys, control, path, tmp_path, "--yes", principal=None
        )
        assert code == 1
        assert "MEASURE_RUNNER_ACTOR_ID" in captured.err
        assert control.api.requests == []
    finally:
        control.stop()


def test_a_human_principal_is_refused(monkeypatch, capsys, tmp_path, fleet):
    control = FakeControlPlane(fleet["actors"], {LOCATE_RULE["id"]: "x"})
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, captured = _invoke(
            monkeypatch, capsys, control, path, tmp_path, "--yes", principal=HUMAN_ACTOR_ID
        )
        assert code == 1
        assert "kind=human" in captured.err
        assert control.grades == []
        assert control.created == []
    finally:
        control.stop()


@pytest.mark.parametrize("kind", sorted(NON_AGENT_ACTOR_IDS))
def test_a_non_agent_principal_is_refused_before_any_run(
    kind, monkeypatch, capsys, tmp_path, fleet
):
    """Only kind=agent may grade, and the refusal must precede the spend.

    A human principal has always been refused, but every *other* registered
    kind used to sail through the preflight and die at grading time — after a
    full serial pass of real, billable sessions, with no grade and no report
    row to show for it (internal/api/grades.go admits human and agent only).
    """
    control = FakeControlPlane(fleet["actors"], {LOCATE_RULE["id"]: "x"})
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, captured = _invoke(
            monkeypatch,
            capsys,
            control,
            path,
            tmp_path,
            "--yes",
            principal=NON_AGENT_ACTOR_IDS[kind],
        )
        assert code == 1
        assert f"kind={kind!r}" in captured.err
        assert control.created == []
        assert control.grades == []
    finally:
        control.stop()


def test_an_unregistered_principal_is_refused(monkeypatch, capsys, tmp_path, fleet):
    control = FakeControlPlane(fleet["actors"], {LOCATE_RULE["id"]: "x"})
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, captured = _invoke(
            monkeypatch, capsys, control, path, tmp_path, "--yes", principal="actor-nobody"
        )
        assert code == 1
        assert "not a registered actor" in captured.err
        assert control.created == []
    finally:
        control.stop()


def test_a_grade_is_filed_against_the_actor_that_actually_served_the_run(
    monkeypatch, capsys, tmp_path, fleet
):
    """If the graph routes a run somewhere else, the grade follows the run.

    `node_runs[].attempts[].actor_id` is the same field nodes-op.sh's
    `grade` uses to default `--actor`; grading the intended actor instead
    would file an opinion against an identity that did no work.
    """
    control = FakeControlPlane(fleet["actors"], {LOCATE_RULE["id"]: "x"})
    control._actor_id_for_slot = lambda slot: "actor-somebody-else"
    report = tmp_path / "report.jsonl"
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, _ = _invoke(
            monkeypatch, capsys, control, path, tmp_path, "--yes", "--report", str(report)
        )
        assert code == 0
        assert {g["evaluated_actor_id"] for g in control.grades} == {"actor-somebody-else"}
        assert all("routing_mismatch" in g["rationale"] for g in control.grades)
        rows = [json.loads(line) for line in report.read_text(encoding="utf-8").splitlines()]
        assert all(row["routing_mismatch"] for row in rows)
    finally:
        control.stop()


def test_post_grade_refuses_an_empty_principal():
    with pytest.raises(runner.RunnerError):
        runner.post_grade(None, "run-1", 5, "notes", "actor-x", "", "cat")


def test_without_yes_nothing_is_dispatched(monkeypatch, capsys, tmp_path, fleet):
    control = FakeControlPlane(fleet["actors"], {LOCATE_RULE["id"]: "x"})
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, captured = _invoke(monkeypatch, capsys, control, path, tmp_path)
        assert code == 1
        assert "--yes" in captured.err
        assert control.created == []
    finally:
        control.stop()


# ---------------------------------------------------------------------------
# 6. The report
# ---------------------------------------------------------------------------


def test_the_report_is_written_and_re_runs_append(monkeypatch, capsys, tmp_path, fleet):
    summary = "culture_nodes/cli/_errors.py:21 defines the policy."
    control = FakeControlPlane(fleet["actors"], {LOCATE_RULE["id"]: summary})
    report = tmp_path / "out" / "report.jsonl"
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, captured = _invoke(
            monkeypatch, capsys, control, path, tmp_path, "--yes", "--report", str(report)
        )
        assert code == 0
        rows = [json.loads(line) for line in report.read_text(encoding="utf-8").splitlines()]
        assert len(rows) == 2
        first = rows[0]
        assert first["rule_id"] == LOCATE_RULE["id"]
        assert first["category"] == LOCATE_RULE["id"]
        assert first["run_id"].startswith("run-")
        assert first["grade_id"].startswith("grade-")
        assert first["rating"] == 5
        assert first["bridge_revision"] == GOOD_REVISION
        assert first["duration_seconds"] >= 0
        assert first["check"]["verdict"] == "cited"
        assert "mean rating" in captured.out

        # Re-run: the old lines survive untouched, new ones are appended.
        before = report.read_text(encoding="utf-8")
        code, _ = _invoke(
            monkeypatch, capsys, control, path, tmp_path, "--yes", "--report", str(report)
        )
        assert code == 0
        after = report.read_text(encoding="utf-8")
        assert after.startswith(before)
        assert len(after.splitlines()) == 4
    finally:
        control.stop()


def test_the_workflow_is_published_once_per_pass(monkeypatch, capsys, tmp_path, fleet):
    control = FakeControlPlane(fleet["actors"], {LOCATE_RULE["id"]: "x"})
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, _ = _invoke(monkeypatch, capsys, control, path, tmp_path, "--yes")
        assert code == 0
        assert control.publishes == 1
    finally:
        control.stop()


# ---------------------------------------------------------------------------
# 7. The cookie never leaks
# ---------------------------------------------------------------------------


def test_the_cookie_never_appears_in_output_or_the_report(monkeypatch, capsys, tmp_path, fleet):
    control = FakeControlPlane(fleet["actors"], {LOCATE_RULE["id"]: "culture_nodes/x.py:1"})
    report = tmp_path / "report.jsonl"
    try:
        path = _write_manifest(tmp_path, _manifest([LOCATE_RULE]))
        code, captured = _invoke(
            monkeypatch, capsys, control, path, tmp_path, "--yes", "--report", str(report)
        )
        assert code == 0
        assert COOKIE not in captured.out
        assert COOKIE not in captured.err
        assert COOKIE not in report.read_text(encoding="utf-8")
        assert COOKIE not in repr(runner.ApiClient("http://x", cookie=COOKIE))
    finally:
        control.stop()


def test_the_cookie_is_sent_as_the_operator_lane_sends_it(monkeypatch, capsys, tmp_path, fleet):
    """`Cookie: CF_Authorization=$NODES_OP_COOKIE` — nodes-op.sh's auth_args."""
    client = runner.ApiClient("http://example.invalid", cookie=COOKIE, bearer="tok")
    headers = client._headers()
    assert headers["Cookie"] == f"CF_Authorization={COOKIE}"
    assert headers["Authorization"] == "Bearer tok"


def test_an_unreachable_api_is_an_environment_error(monkeypatch, capsys):
    monkeypatch.delenv("NODES_API_URL", raising=False)
    monkeypatch.setenv("MEASURE_RUNNER_ACTOR_ID", AGENT_ACTOR_ID)
    code = runner.main(["--manifest", str(MEASUREMENTS_DIR / "basic.json")])
    captured = capsys.readouterr()
    assert code == 2
    assert "NODES_API_URL" in captured.err
    assert captured.err.startswith("error: ")
    assert "hint: " in captured.err


def test_per_slot_bridge_token_env_wins_over_the_default(monkeypatch) -> None:
    """NODES_BRIDGE_TOKEN_<SLOT> keeps a per-bridge secret off argv."""
    monkeypatch.setenv("NODES_BRIDGE_TOKEN", "shared")
    monkeypatch.setenv("NODES_BRIDGE_TOKEN_PI", "pi-only")
    actors = [
        {"actor_key": "company/pi-thor", "slot": "pi"},
        {"actor_key": "company/qwen-thor", "slot": "qwen"},
    ]
    tokens: dict[str, str] = {}
    default = os.environ.get("NODES_BRIDGE_TOKEN", "")
    for actor in actors:
        per_slot = os.environ.get(f"NODES_BRIDGE_TOKEN_{actor['slot'].upper()}", "")
        if per_slot:
            tokens.setdefault(actor["actor_key"], per_slot)
        elif default:
            tokens.setdefault(actor["actor_key"], default)
    assert tokens == {"company/pi-thor": "pi-only", "company/qwen-thor": "shared"}


def test_api_client_names_itself_in_user_agent() -> None:
    """Cloudflare's bot rules ban urllib's default agent (error 1010)."""
    headers = runner.ApiClient("http://example.invalid", cookie="c")._headers()
    assert headers["User-Agent"].startswith("culture-nodes-measure-runner/")
    assert headers["Cookie"] == "CF_Authorization=c"


def test_get_defeats_the_edge_cache(monkeypatch) -> None:
    """nodes.culture.dev cached a run view for 366 s; GETs must be unique."""
    seen: list[tuple[str, dict[str, str]]] = []

    class _Resp:
        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

        def read(self):
            return b'{"ok": true}'

    def fake_urlopen(req, timeout=0):
        seen.append((req.full_url, dict(req.header_items())))
        return _Resp()

    monkeypatch.setattr(runner.fleet.urllib.request, "urlopen", fake_urlopen)
    client = runner.ApiClient("http://example.invalid")
    client.get("/v1alpha1/runs/x")
    client.get("/v1alpha1/runs/x")
    urls = [u for u, _ in seen]
    assert all("_nocache=" in u for u in urls) and urls[0] != urls[1]
    assert seen[0][1].get("Cache-control") == "no-cache"


def test_a_grade_overridden_to_the_human_principal_aborts() -> None:
    """#306: the API replaces grading_actor_id with the bound principal."""
    appended = {
        "id": "rec_1",
        "authority": "confirmed",
        "origin": {"kind": "human", "actor_id": "actor_ori"},
        "warning": "grading_actor_id overridden from actor_runner to authenticated actor actor_ori",
    }
    with pytest.raises(runner.RunnerError) as excinfo:
        runner.assert_grade_landed_as(appended, "actor_runner")
    assert "actor_ori" in str(excinfo.value)
    assert "#306" in (getattr(excinfo.value, "hint", "") or " ".join(map(str, excinfo.value.args)))


def test_a_grade_recorded_as_the_agent_principal_passes() -> None:
    appended = {"authority": "proposed", "origin": {"kind": "agent", "actor_id": "actor_runner"}}
    runner.assert_grade_landed_as(appended, "actor_runner")
