"""What the measurement runner does when a run does NOT finish in time.

Split out of ``tests/test_measurement_runner.py`` only because that file is
close to the repo's 1000-line hard limit (``tests/lint/filelength_test.go``);
this is the same runner and the same contract.

The contract under test is the serial one. ``run.py``'s module docstring
promises that ONE run exists at a time across all actors and rules, because
pi, qwen and colleague are served by one model on one host — two runs at once
measure a contended queue, not two actors. A watch timeout used to be a
silent hole in that promise: the runner synthesised a ``timed_out`` state
locally and dispatched the next billable session while the control plane
still had the previous run open. So:

1. a timeout cancels the run and waits for the control plane to call it
   terminal before ``watch_run`` returns;
2. a run that will not settle stops the pass instead of overlapping it;
3. a run that finished in the gap keeps its real state and its answer;
4. ``run_pass`` creates the next run only after all of that has happened.

These drive a STUB control plane rather than ``tests/fake_api.py``'s HTTP
server, because what is asserted is a call *sequence* and a call log is the
direct way to assert one.
"""

from __future__ import annotations

import importlib.util
import io
import sys
from pathlib import Path
from types import ModuleType

import pytest

ROOT = Path(__file__).resolve().parents[1]
MEASUREMENTS_DIR = ROOT / "examples" / "harness-compare" / "measurements"

AGENT_ACTOR_ID = "actor-agent-measure-runner"
MANIFEST_DIGEST = "sha256:" + "d" * 64
WORKFLOW_DIGEST = "sha256:" + "c" * 64


def _load(name: str) -> ModuleType:
    spec = importlib.util.spec_from_file_location(
        f"harness_compare_measurement_timeout_{name}", MEASUREMENTS_DIR / f"{name}.py"
    )
    assert spec is not None
    assert spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


runner = _load("run")


class StubControlPlane:
    """The calls the timeout path makes, recorded in order.

    ``on_cancel`` is the state the run takes the moment a cancel is
    attempted — ``"cancelled"`` for the ordinary case, ``None`` for a cancel
    that does not land, ``"completed"`` for a run that had already finished
    (which the real control plane answers with a 409, hence
    ``cancel_raises``).
    """

    def __init__(self, *, on_cancel: str | None = "cancelled", cancel_raises: bool = False) -> None:
        self.calls: list[str] = []
        self.states: dict[str, str] = {}
        self.created: list[dict] = []
        self.grades: list[dict] = []
        self.on_cancel = on_cancel
        self.cancel_raises = cancel_raises

    def get(self, path: str):
        self.calls.append(f"GET {path}")
        run_id = path.split("/")[3]
        return {"run": {"id": run_id, "state": self.states.get(run_id, "running")}}

    def post(self, path: str, body):
        self.calls.append(f"POST {path}")
        if path == "/v1alpha1/runs":
            run_id = f"run-{len(self.created) + 1:03d}"
            self.created.append(body)
            return {"id": run_id, "state": "running"}
        run_id = path.split("/")[3]
        if path.endswith("/cancel"):
            if self.on_cancel is not None:
                self.states[run_id] = self.on_cancel
            if self.cancel_raises:
                raise runner.RunnerError(
                    f"POST {path} returned HTTP 409: the run has already reached a "
                    "terminal state",
                    "check the run id",
                    runner.EXIT_ENV_ERROR,
                )
            return {"state": self.states.get(run_id, "running")}
        if path.endswith("/grades"):
            self.grades.append(body)
            return {"id": f"grade-{len(self.grades):03d}", "authority": "proposed"}
        raise AssertionError(f"unexpected POST {path}")


def _never_sleep(_seconds: float) -> None:
    """The runner's injected sleep. A test may not wait on a poll interval."""


def test_a_timed_out_run_is_cancelled_and_settled_before_watch_returns():
    control = StubControlPlane()
    view = runner.watch_run(
        control, "run-001", timeout=0.0, interval=0.0, sleep=_never_sleep, grace=5.0
    )
    assert control.calls == [
        "GET /v1alpha1/runs/run-001",
        "POST /v1alpha1/runs/run-001/cancel",
        "GET /v1alpha1/runs/run-001",
    ]
    # The runner's word for why the run ended, and the ledger's word for how.
    assert view["run"]["state"] == "timed_out"
    assert view["run"]["settled_state"] == "cancelled"


def test_a_run_that_will_not_settle_stops_the_pass_rather_than_overlapping():
    control = StubControlPlane(on_cancel=None)
    with pytest.raises(runner.RunnerError) as caught:
        runner.watch_run(
            control, "run-001", timeout=0.0, interval=0.0, sleep=_never_sleep, grace=0.0
        )
    message = str(caught.value)
    assert "run-001" in message and "still running" in message
    assert "serial" in caught.value.hint
    assert caught.value.code == runner.EXIT_ENV_ERROR


def test_a_run_that_finished_in_the_gap_keeps_its_state_and_its_answer():
    """The cancel loses a race with the run itself: 409, and the run is done.

    Rating that 1 would be a claim about the actor that is not true — the
    answer exists — so the real terminal state is what comes back.
    """
    control = StubControlPlane(on_cancel="completed", cancel_raises=True)
    view = runner.watch_run(
        control, "run-001", timeout=0.0, interval=0.0, sleep=_never_sleep, grace=5.0
    )
    assert view["run"]["state"] == "completed"
    assert view["run"]["settled_state"] == "completed"


RULE = {
    "id": "locate-exit-code-policy",
    "category": "locate",
    "instruction": "find the exit-code policy",
    "sandbox": "read-only",
    "check": {"kind": "grep-cites-file-line", "expect": "culture_nodes/cli/_errors.py"},
    "anchors": {"5": "cited", "3": "uncited", "1": "absent"},
    "runs_per_actor": 1,
}

ACTORS = [
    {"actor_id": "actor-pi-thor", "actor_key": "company/pi-thor", "slot": "pi"},
    {"actor_id": "actor-qwen-thor", "actor_key": "company/qwen-thor", "slot": "qwen"},
]


def _run_pass(control, report_path: Path | None = None, grace: float = 5.0):
    return runner.run_pass(
        control,
        {"version": 1, "actors": [a["actor_key"] for a in ACTORS], "rules": [RULE]},
        MANIFEST_DIGEST,
        ACTORS,
        {"pi": "/home/culture-pi/git/culture-nodes-agent", "qwen": "/home/culture-qwen/git/repo"},
        {a["actor_key"]: {"revision": "a" * 40} for a in ACTORS},
        AGENT_ACTOR_ID,
        WORKFLOW_DIGEST,
        report_path,
        timeout=0.0,
        interval=0.0,
        grace=grace,
        sleep=_never_sleep,
        # The pass prints a line per run; nothing here asserts on it.
        out=io.StringIO(),
    )


def test_the_next_run_is_created_only_after_the_timed_out_one_is_settled():
    control = StubControlPlane()
    records = _run_pass(control)

    creates = [i for i, call in enumerate(control.calls) if call == "POST /v1alpha1/runs"]
    cancel = control.calls.index("POST /v1alpha1/runs/run-001/cancel")
    settle = control.calls.index("GET /v1alpha1/runs/run-001", cancel)
    assert len(creates) == 2, control.calls
    # The whole finding, as one assertion: the second dispatch happens after
    # the first run was cancelled AND confirmed terminal, not before.
    assert creates[1] > settle > cancel > creates[0]

    assert [r["run_state"] for r in records] == ["timed_out", "timed_out"]
    assert [r["settled_state"] for r in records] == ["cancelled", "cancelled"]
    # A timed-out run still costs one 1-rated grade; the pass is not abandoned.
    assert [r["rating"] for r in records] == [1, 1]
    assert len(control.grades) == 2


def test_a_run_that_will_not_settle_aborts_the_pass_before_the_next_dispatch():
    control = StubControlPlane(on_cancel=None)
    with pytest.raises(runner.RunnerError):
        _run_pass(control, grace=0.0)
    assert control.calls.count("POST /v1alpha1/runs") == 1
