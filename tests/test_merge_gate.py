"""scripts/merge-gate.py — the TDD merge gate as a program a code node runs.

The fixtures are real git repositories and real subprocesses, not mocks. The
whole claim the gate makes is that it measured something against a named
commit, and a test that stubbed the measurement would assert nothing — the
same reasoning tests/test_collect_handover.py records for its own fixtures.

The control plane IS stubbed, in one place and deliberately: what the control
plane does with a gate report is proven in Go against a real PostgreSQL
(internal/api/gatereports_test.go). What these tests own is the other half —
which gates the program decides are applicable, what it refuses, and that no
path through it can report a pass it did not measure.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

SCRIPT = Path(__file__).parents[1] / "scripts" / "merge-gate.py"

RUN_ID = "01M05ZGNT86MAFDHATB6W5VYPN"
VALIDATOR = "actor_merge_gate_validator"


def git(repo: Path, *args: str) -> str:
    proc = subprocess.run(
        ["git", *args],
        cwd=repo,
        capture_output=True,
        text=True,
        check=True,
        env={**os.environ, "GIT_TERMINAL_PROMPT": "0"},
    )
    return proc.stdout.strip()


@pytest.fixture
def repo(tmp_path: Path) -> Path:
    """A worktree with one base commit and one candidate commit, so the gate
    has a real changed-file set to decide applicability against."""
    root = tmp_path / "checkout"
    root.mkdir()
    git(root, "init", "-q", "-b", "main")
    git(root, "config", "user.email", "t16@example.invalid")
    git(root, "config", "user.name", "t16")
    (root / "README.md").write_text("base\n")
    git(root, "add", "README.md")
    git(root, "commit", "-q", "-m", "base")
    return root


def commit(repo: Path, path: str, body: str = "changed\n") -> str:
    target = repo / path
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(body)
    git(repo, "add", path)
    git(repo, "commit", "-q", "-m", f"change {path}")
    return git(repo, "rev-parse", "HEAD")


def run_gate(repo: Path, matrix: dict, *extra: str, env: dict | None = None):
    base = git(repo, "rev-parse", "HEAD~1")
    argv = [
        sys.executable,
        str(SCRIPT),
        "--gates",
        json.dumps(matrix),
        "--repo",
        str(repo),
        "--base",
        base,
        *extra,
    ]
    clean = {k: v for k, v in os.environ.items() if not k.startswith("NODES_")}
    return subprocess.run(argv, capture_output=True, text=True, env={**clean, **(env or {})})


def report_of(proc: subprocess.CompletedProcess) -> dict:
    assert proc.stdout, f"no report on stdout; stderr was:\n{proc.stderr}"
    return json.loads(proc.stdout)


def gate_named(report: dict, name: str) -> dict:
    for entry in report["gates"]:
        if entry["gate"] == name:
            return entry
    raise AssertionError(f"{name} not in the report: {report['gates']}")


GO_GATE = {
    "gate": "go-test",
    "suite": "go test ./...",
    "instrument": "go test",
    "reaches": ["**/*.go", "go.mod"],
    "requires": ["definitely-not-a-real-binary-t16"],
    "command": ["true"],
}


def test_a_gate_that_never_reached_the_change_is_no_source_files_not_a_pass(repo: Path):
    """A docs-only change owes the Go gate nothing. Recording that as a pass
    would be the empty-scan false green; recording it as a failure would
    manufacture a defect. It is neither: it is `no_source_files`."""
    commit(repo, "docs/notes.md")
    proc = run_gate(repo, {"gates": [GO_GATE]}, "--report-only")

    entry = gate_named(report_of(proc), "go-test")
    assert entry["not_applicable"]["reason"] == "no_source_files"
    assert "exit_code" not in entry


def test_a_missing_toolchain_names_the_files_it_owed_a_measurement_on(repo: Path):
    """Criterion 5's mechanical half, and the reason `measurement_incomplete`
    exists: a lane without the toolchain must not report a green gate."""
    commit(repo, "internal/api/gatereports.go")
    proc = run_gate(repo, {"gates": [{**GO_GATE, "reaches": ["**/*.go"]}]}, "--report-only")

    report = report_of(proc)
    entry = gate_named(report, "go-test")
    assert entry["not_applicable"]["reason"] == "instrument_unavailable"
    assert entry["not_applicable"]["uncovered_paths"] == ["internal/api/gatereports.go"]
    assert report["outcome"] == "measurement_incomplete"


def test_toolchain_availability_is_decided_after_applicability(repo: Path):
    """The ORDER matters. Reversed, a docs-only change on a host without Go
    would be `measurement_incomplete` for every Go gate — an instrument that
    was never owed a measurement is not a measurement that went missing."""
    commit(repo, "docs/notes.md")
    proc = run_gate(repo, {"gates": [GO_GATE]}, "--report-only")

    assert gate_named(report_of(proc), "go-test")["not_applicable"]["reason"] == "no_source_files"


def test_an_instrument_that_does_not_reach_a_changed_tree_says_so(repo: Path):
    """Today's coverage instrument reaches `culture_nodes` and not `internal`
    (issue #88). That gap is recorded per run, naming the uncovered files,
    rather than silently counted as satisfied."""
    commit(repo, "internal/api/gatereports.go")
    proc = run_gate(
        repo,
        {
            "gates": [
                {
                    "gate": "coverage",
                    "instrument": "coverage.py",
                    "reaches": ["culture_nodes/**"],
                    "responsible_for": ["culture_nodes/**", "internal/**"],
                    "command": ["true"],
                }
            ]
        },
        "--report-only",
    )

    entry = gate_named(report_of(proc), "coverage")
    assert entry["not_applicable"]["reason"] == "instrument_not_reaching_tree"
    assert entry["not_applicable"]["uncovered_paths"] == ["internal/api/gatereports.go"]


def test_a_gate_with_no_declared_command_is_no_test_instrument(repo: Path):
    """A tree with no declared suite is not zero failures — it is no
    measurement."""
    commit(repo, "web/src/main.ts")
    proc = run_gate(repo, {"gates": [{"gate": "web-test", "reaches": ["web/**"]}]}, "--report-only")

    assert (
        gate_named(report_of(proc), "web-test")["not_applicable"]["reason"] == "no_test_instrument"
    )


def test_an_applicable_gate_runs_and_reports_its_exit_code(repo: Path):
    commit(repo, "internal/api/gatereports.go")
    proc = run_gate(
        repo,
        {
            "gates": [
                {
                    "gate": "go-test",
                    "suite": "go test ./...",
                    "reaches": ["**/*.go"],
                    "command": [sys.executable, "-c", "raise SystemExit(1)"],
                }
            ]
        },
        "--report-only",
    )

    report = report_of(proc)
    entry = gate_named(report, "go-test")
    assert entry["exit_code"] == 1
    assert "not_applicable" not in entry
    assert report["outcome"] == "changes_required"


def test_every_gate_passing_computes_gates_passed(repo: Path):
    commit(repo, "internal/api/gatereports.go")
    proc = run_gate(
        repo,
        {
            "gates": [
                {
                    "gate": "go-test",
                    "reaches": ["**/*.go"],
                    "command": [sys.executable, "-c", "raise SystemExit(0)"],
                }
            ]
        },
        "--report-only",
    )

    assert report_of(proc)["outcome"] == "gates_passed"


def test_report_only_never_exits_zero(repo: Path):
    """A gate whose finding is not in the ledger gated nothing, so the mode
    that records nothing cannot produce the passing exit code."""
    commit(repo, "internal/api/gatereports.go")
    proc = run_gate(
        repo,
        {
            "gates": [
                {
                    "gate": "go-test",
                    "reaches": ["**/*.go"],
                    "command": [sys.executable, "-c", "raise SystemExit(0)"],
                }
            ]
        },
        "--report-only",
    )

    assert report_of(proc)["outcome"] == "gates_passed"
    assert proc.returncode == 2


def test_refuses_without_a_run_id(repo: Path):
    """A validator that cannot name its subject records nothing. The run id
    comes from the operation's own context through the runner boundary, never
    from a guess."""
    commit(repo, "internal/api/gatereports.go")
    proc = run_gate(repo, {"gates": [{"gate": "go-test", "command": ["true"]}]})

    assert proc.returncode == 2
    assert "NODES_RUN_ID" in proc.stderr


def test_refuses_when_the_finding_cannot_be_recorded(repo: Path):
    """Every gate passes and the control plane is unreachable: the answer is
    still `measurement_incomplete`, because nothing was recorded."""
    commit(repo, "internal/api/gatereports.go")
    proc = run_gate(
        repo,
        {
            "gates": [
                {
                    "gate": "go-test",
                    "reaches": ["**/*.go"],
                    "command": [sys.executable, "-c", "raise SystemExit(0)"],
                }
            ]
        },
        env={
            "NODES_RUN_ID": RUN_ID,
            "NODES_API_URL": "http://127.0.0.1:1",
            "NODES_DECISION_TOKEN": "t",
            "NODES_GATE_VALIDATOR_ACTOR_ID": VALIDATOR,
        },
    )

    assert proc.returncode == 2
    assert "gated nothing" in proc.stderr


class _StubControlPlane:
    """A control plane that records the report it was given and answers with a
    canned aggregate. It exists to prove the WIRE half — what is sent, and that
    the exit code is the control plane's rather than this program's."""

    def __init__(self, outcome: str, exit_code: int):
        self.received: dict | None = None
        self.auth: str | None = None
        stub = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802 - BaseHTTPRequestHandler's own spelling
                length = int(self.headers.get("Content-Length", "0"))
                stub.received = json.loads(self.rfile.read(length))
                stub.auth = self.headers.get("Authorization")
                stub.path = self.path
                body = json.dumps(
                    {
                        "gates": [],
                        "aggregate": {"id": "ledger_stub"},
                        "counts": {
                            "declared_gate_count": len(stub.received["gates"]),
                            "applicable_gate_count": 1,
                            "passed_gate_count": 1,
                            "failed_gate_count": 0,
                            "not_applicable_gate_count": 0,
                        },
                        "outcome": outcome,
                        "exit_code": exit_code,
                    }
                ).encode()
                self.send_response(201)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_port}"

    def __enter__(self):
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        return self

    def __exit__(self, *exc):
        self.server.shutdown()
        self.server.server_close()


def _passing_matrix() -> dict:
    return {
        "gates": [
            {
                "gate": "go-test",
                "suite": "go test ./...",
                "reaches": ["**/*.go"],
                "command": [sys.executable, "-c", "raise SystemExit(0)"],
            }
        ]
    }


def test_posts_the_report_and_exits_with_the_recorded_code(repo: Path):
    commit(repo, "internal/api/gatereports.go")
    with _StubControlPlane("gates_passed", 0) as stub:
        proc = run_gate(
            repo,
            _passing_matrix(),
            env={
                "NODES_RUN_ID": RUN_ID,
                "NODES_NODE_RUN_ID": "01M05ZGNT8QW2W1M5PAPXQ8N3C",
                "NODES_API_URL": stub.url,
                "NODES_DECISION_TOKEN": "shhh",
                "NODES_GATE_VALIDATOR_ACTOR_ID": VALIDATOR,
            },
        )

    assert proc.returncode == 0, proc.stderr
    assert stub.auth == "Bearer shhh"
    assert stub.path == f"/v1alpha1/runs/{RUN_ID}/gate-reports"
    sent = stub.received
    assert sent["validator_actor_id"] == VALIDATOR
    assert sent["node_run_ref"] == "01M05ZGNT8QW2W1M5PAPXQ8N3C"
    assert len(sent["commit_sha"]) == 40
    assert len(sent["base_sha"]) == 40
    assert sent["changed_files"] == ["internal/api/gatereports.go"]
    # The counts and the outcome are the control plane's to compute; a gate
    # that could assert its own aggregate could assert a green one.
    assert "counts" not in sent
    assert "outcome" not in sent


def test_a_disagreement_with_the_control_plane_reaches_a_person(repo: Path):
    """The node routes on an edge and a reader reads the ledger. If the two
    disagree about the outcome, neither answer may be taken — it exits
    `measurement_incomplete` so a person reconciles them."""
    commit(repo, "internal/api/gatereports.go")
    with _StubControlPlane("changes_required", 1) as stub:
        proc = run_gate(
            repo,
            _passing_matrix(),
            env={
                "NODES_RUN_ID": RUN_ID,
                "NODES_API_URL": stub.url,
                "NODES_DECISION_TOKEN": "shhh",
                "NODES_GATE_VALIDATOR_ACTOR_ID": VALIDATOR,
            },
        )

    assert proc.returncode == 2
    assert "recorded outcome" in proc.stderr


def test_refuses_a_matrix_with_no_gates(repo: Path):
    commit(repo, "internal/api/gatereports.go")
    proc = run_gate(repo, {"gates": []}, "--report-only")

    assert proc.returncode == 2
    assert "no gates" in proc.stderr


def test_a_gate_runs_in_its_declared_subdirectory(repo: Path):
    """The web build runs in `web/`, not at the repo root."""
    commit(repo, "web/src/main.ts")
    (repo / "web" / "marker").write_text("here\n")
    proc = run_gate(
        repo,
        {
            "gates": [
                {
                    "gate": "web-build",
                    "reaches": ["web/**"],
                    "cwd": "web",
                    "command": [
                        sys.executable,
                        "-c",
                        "import os,sys; sys.exit(0 if os.path.exists('marker') else 3)",
                    ],
                }
            ]
        },
        "--report-only",
    )

    assert gate_named(report_of(proc), "web-build")["exit_code"] == 0


def test_a_gate_cannot_measure_outside_the_worktree(repo: Path):
    """A matrix is graph content, but a working directory that escapes the tree
    under measurement would be measuring something else."""
    commit(repo, "web/src/main.ts")
    proc = run_gate(
        repo,
        {
            "gates": [
                {"gate": "web-build", "reaches": ["web/**"], "cwd": "../..", "command": ["true"]}
            ]
        },
        "--report-only",
    )

    assert proc.returncode == 2
    assert "escapes the worktree" in proc.stderr
