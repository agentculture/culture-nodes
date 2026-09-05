"""Review-era tests for the measurement runner (PR #307).

Split out of test_measurement_runner.py, which sits at the 1000-line hard limit
(tests/lint/filelength_test.go). Same module loader, same fixtures.
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
GOOD_REVISION = "a" * 40
STALE_REVISION = "b" * 40


def _load(name: str) -> ModuleType:
    spec = importlib.util.spec_from_file_location(
        f"harness_compare_measurement_test_{name}", MEASUREMENTS_DIR / f"{name}.py"
    )
    assert spec is not None
    assert spec.loader is not None
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
    assert all("_nocache=" in u for u in urls)
    assert urls[0] != urls[1]
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


def test_non_json_answer_is_a_runner_error(monkeypatch) -> None:
    """A 200 with an HTML body (an edge page) is an environment error, not a traceback."""

    class _Resp:
        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

        def read(self):
            return b"<html>blocked</html>"

    monkeypatch.setattr(runner.fleet.urllib.request, "urlopen", lambda req, timeout=0: _Resp())
    client = runner.ApiClient("http://example.invalid")
    with pytest.raises(runner.RunnerError) as excinfo:
        client.get("/v1alpha1/actors")
    assert "not JSON" in str(excinfo.value)


def test_grader_must_be_registered_kind_agent(monkeypatch) -> None:
    """An engine/runner/validator principal is refused before any dispatch (#307 review)."""
    rows = [{"id": "a1", "actor_key": "company/x", "kind": "engine", "revision": 1}]
    monkeypatch.setattr(
        runner.fleet.ApiClient, "get", lambda self, path: {"items": rows}, raising=False
    )
    client = runner.ApiClient("http://example.invalid")
    with pytest.raises(runner.RunnerError) as excinfo:
        runner.fleet.resolve_grading_actor(client, "a1")
    assert "kind=engine" in str(excinfo.value)
