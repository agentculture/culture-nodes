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

import ast
import importlib.util
import itertools
import json
import os
import subprocess
import sys
import threading
import types
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


#: The two decisive shapes an entry can take, keyed by what they say about the
#: change. A matrix that declares both has one honest answer, and the order the
#: author happened to write them in is not part of it.
DECISIVE_GATES = {
    "failing": {
        "gate": "go-test",
        "reaches": ["**/*.go"],
        "command": [sys.executable, "-c", "raise SystemExit(1)"],
    },
    "unavailable": {
        "gate": "markdownlint",
        "reaches": ["**/*.go"],
        "requires": ["definitely-not-a-real-binary-t16"],
        "command": ["true"],
    },
}

GATE_ORDERS = list(itertools.permutations(sorted(DECISIVE_GATES)))


@pytest.mark.parametrize("order", GATE_ORDERS, ids=["-then-".join(o) for o in GATE_ORDERS])
def test_a_failure_dominates_an_unavailable_instrument_in_any_declared_order(
    repo: Path, order: tuple[str, ...]
):
    """Issue #153. A matrix holding one failing gate and one unavailable
    instrument says two things at once, and only one of them can be the run's
    outcome. `internal/handover.GateResults.Outcome` settles it by counting
    failures across every entry BEFORE looking for an unavailable instrument,
    so a failure dominates however the matrix is written.

    `local_outcome` used to return from inside its loop, which made whichever
    decisive entry appeared first the verdict: the same two gates reported
    `changes_required` in one order and `measurement_incomplete` in the other.
    That is not a near-miss. `changes_required` is a domain outcome that routes
    the run back for repair; `measurement_incomplete` says nothing was measured
    and reaches a person. Declaration order decided which, and the node routes
    on this answer.
    """
    commit(repo, "internal/api/gatereports.go")
    proc = run_gate(repo, {"gates": [DECISIVE_GATES[name] for name in order]}, "--report-only")

    report = report_of(proc)
    assert gate_named(report, "go-test")["exit_code"] == 1
    assert (
        gate_named(report, "markdownlint")["not_applicable"]["reason"] == "instrument_unavailable"
    )
    assert report["outcome"] == "changes_required", (
        f"declared in the order {order}, the same two gates reported " f"{report['outcome']!r}"
    )


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


def test_gate_measures_feature_plus_package_not_green_package_alone(repo: Path):
    """A package and feature are independently green; their merge is red.
    The posted subject is the merge commit and the handover binding remains
    the package commit, so the control-plane verdict is about the combination."""
    (repo / "gate.py").write_text(
        "from pathlib import Path\n"
        "raise SystemExit(Path('feature').exists() and Path('package').exists())\n"
    )
    git(repo, "add", "gate.py")
    git(repo, "commit", "-q", "-m", "gate fixture")
    base = git(repo, "rev-parse", "HEAD")

    git(repo, "switch", "-q", "-c", "package", base)
    commit(repo, "package")
    package_sha = git(repo, "rev-parse", "HEAD")
    assert subprocess.run([sys.executable, "gate.py"], cwd=repo).returncode == 0

    git(repo, "switch", "-q", "-c", "feature", base)
    commit(repo, "feature")
    assert subprocess.run([sys.executable, "gate.py"], cwd=repo).returncode == 0
    git(repo, "merge", "-q", "--no-ff", "package", "-m", "candidate")
    candidate_sha = git(repo, "rev-parse", "HEAD")

    matrix = {"gates": [{"gate": "combination", "command": [sys.executable, "gate.py"]}]}
    proc = run_gate(repo, matrix, "--report-only")
    report = report_of(proc)
    assert report["outcome"] == "changes_required"
    assert report["commit_sha"] == candidate_sha
    assert report["package_commit_sha"] == package_sha
    assert report["gates"][0]["exit_code"] == 1


def test_go_test_all_skips_is_measurement_incomplete(repo: Path, tmp_path: Path):
    commit(repo, "internal/example/example.go", "package example\n")
    fake_bin = tmp_path / "bin"
    fake_bin.mkdir()
    fake_go = fake_bin / "go"
    fake_go.write_text(
        "#!/bin/sh\n"
        "printf '%s\\n' "
        '\'{"Action":"run","Package":"example","Test":"TestSkipped"}\' '
        '\'{"Action":"skip","Package":"example","Test":"TestSkipped"}\' '
        '\'{"Action":"pass","Package":"example"}\'\n'
    )
    fake_go.chmod(0o755)
    proc = run_gate(
        repo,
        {
            "gates": [
                {"gate": "go-test", "reaches": ["**/*.go"], "command": ["go", "test", "./..."]}
            ]
        },
        "--report-only",
        env={"PATH": f"{fake_bin}:{os.environ['PATH']}"},
    )

    report = report_of(proc)
    assert report["outcome"] == "measurement_incomplete"
    assert gate_named(report, "go-test")["not_applicable"]["reason"] == "no_tests_executed"


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


FULL_VOCABULARY_MATRIX = {
    "base": "origin/main",
    "gates": [
        {
            "gate": "go-test",
            "suite": "go test ./...",
            "instrument": "go test",
            "version_command": ["go", "version"],
            "requires": ["go"],
            "reaches": ["**/*.go", "go.mod"],
            "responsible_for": ["**/*.go", "go.mod"],
            "command": ["go", "test", "./..."],
            "measurement": {"unit": "failures", "threshold": {"maximum": 0}},
            "repair": {"requires_grants": ["network-egress"]},
            "cwd": ".",
            "timeout_seconds": 1800,
        }
    ],
}


def _gate_module() -> types.ModuleType:
    """Import scripts/merge-gate.py by path — the filename's hyphen makes it
    un-importable by name, and it is a program rather than a package member."""
    spec = importlib.util.spec_from_file_location("merge_gate_under_test", SCRIPT)
    assert spec is not None
    assert spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _keys_the_parser_reads() -> set[str]:
    """Every string literal the module reads off a `gate` mapping, found by
    walking its own AST rather than by anybody remembering to write it down."""
    keys: set[str] = set()

    def is_gate(node: ast.AST) -> bool:
        return isinstance(node, ast.Name) and node.id == "gate"

    for node in ast.walk(ast.parse(SCRIPT.read_text(encoding="utf-8"))):
        if isinstance(node, ast.Subscript) and is_gate(node.value):
            index = node.slice
            if isinstance(index, ast.Constant) and isinstance(index.value, str):
                keys.add(index.value)
        elif (
            isinstance(node, ast.Call)
            and isinstance(node.func, ast.Attribute)
            and node.func.attr == "get"
            and is_gate(node.func.value)
            and node.args
        ):
            first = node.args[0]
            if isinstance(first, ast.Constant) and isinstance(first.value, str):
                keys.add(first.value)
    return keys


def test_the_declared_key_vocabulary_is_the_one_the_parser_actually_reads():
    """Issue #148's other half. Refusing unknown keys is only honest if the
    known set is the set the code reads; a hand-maintained list drifts the
    moment a key is taught to the parser and not to it, and the refusal then
    rejects a legitimate matrix by name.

    So the set is re-derived here from the module's own AST. The scan sees
    literal access (`gate["x"]`, `gate.get("x")`) and NOT dynamic access — the
    `for key in ("suite", "instrument")` loop in `not_applicable` is invisible
    to it, and both of those keys are only in this comparison because they are
    also read literally in `run_gate`. It is a floor on the vocabulary, not a
    proof of it, and it fails on exactly the drift #148 is about: a key added
    to the parser and not to `KNOWN_GATE_KEYS`.
    """
    module = _gate_module()

    assert _keys_the_parser_reads() == set(module.KNOWN_GATE_KEYS)
    assert len(module.KNOWN_GATE_KEYS) == 12, (
        "the gate vocabulary changed size; that is a deliberate change to what a pinned "
        "matrix may declare, so update this count along with KNOWN_GATE_KEYS"
    )


def test_a_gate_carrying_keys_the_parser_never_reads_is_refused_by_name(repo: Path):
    """Issue #148. The reported matrix carried `tools` and `threshold`, neither
    of which the parser has ever read: the tools were never required of the
    host, the threshold was never compared against anything, and the gate
    reported `not_applicable` over an empty uncovered set. Nothing complained.

    A key nobody reads declares nothing while looking exactly like it does, and
    the matrix is pinned graph content — what it says is what a reader believes
    was measured.
    """
    commit(repo, "internal/api/gatereports.go")
    proc = run_gate(
        repo,
        {"gates": [{**GO_GATE, "tools": ["go"], "threshold": 0}]},
        "--report-only",
    )

    assert proc.returncode == 2
    # Separate assertions: the refusal must name EVERY unknown key, so a
    # failure that names one and not the other has to say which one is missing.
    assert "'threshold'" in proc.stderr
    assert "'tools'" in proc.stderr
    hint = next(line for line in proc.stderr.splitlines() if line.startswith("hint:"))
    for key in _gate_module().KNOWN_GATE_KEYS:
        assert key in hint, f"the hint must list the valid set; {key!r} is missing"


def test_an_unknown_key_is_refused_before_anything_is_measured(repo: Path):
    """The refusal is at load, before a changed-file set exists — a malformed
    matrix is not a gate that measured badly, it is a gate whose declaration
    was never true, and nothing it could measure would be worth reporting."""
    commit(repo, "docs/notes.md")
    proc = run_gate(
        repo,
        {
            "gates": [
                {
                    "gate": "go-test",
                    "reaches": ["**/*.go"],
                    "command": [sys.executable, "-c", "raise SystemExit(0)"],
                    "tools": ["go"],
                }
            ]
        },
        "--report-only",
    )

    assert proc.returncode == 2
    assert proc.stdout == "", "a refused matrix must not also produce a report"


def test_an_unnamed_gate_is_refused(repo: Path):
    commit(repo, "internal/api/gatereports.go")
    proc = run_gate(
        repo, {"gates": [{"reaches": ["**/*.go"], "command": ["true"]}]}, "--report-only"
    )

    assert proc.returncode == 2
    assert "no name" in proc.stderr


def test_check_matrix_accepts_the_whole_vocabulary_and_measures_nothing(repo: Path):
    """The positive half, and the one scripts/validate-examples.sh runs first:
    a guard that refused everything would satisfy the negative check alone."""
    commit(repo, "internal/api/gatereports.go")
    proc = run_gate(repo, FULL_VOCABULARY_MATRIX, "--check-matrix")

    assert proc.returncode == 0, proc.stderr
    assert "go-test" in proc.stdout
    assert "nothing was measured" in proc.stdout


def test_check_matrix_refuses_an_unknown_key(repo: Path):
    commit(repo, "internal/api/gatereports.go")
    broken = {
        "base": "origin/main",
        "gates": [{**FULL_VOCABULARY_MATRIX["gates"][0], "tools": ["go"]}],
    }
    proc = run_gate(repo, broken, "--check-matrix")

    assert proc.returncode == 2
    assert "'tools'" in proc.stderr


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
