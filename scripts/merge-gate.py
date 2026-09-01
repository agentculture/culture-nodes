#!/usr/bin/env python3
"""The TDD merge gate, as a program a code node runs — not a command an
operator types and a tick an operator reads.

    scripts/merge-gate.py --gates '<matrix json>'
    scripts/merge-gate.py --gates @gates.json --report-only
    scripts/merge-gate.py --gates @gates.json --check-matrix

# What this replaces

Build, vet, test, five adapter suites, the web build and six linters, typed by
the operator and read by the operator, every time. Nine times in the #87 batch
and six more hand-turns in the #128/#137 cycle. An operator reading a green
tick is not evidence of anything: it leaves nothing behind that names the
suite, the exit code, or the commit, and a week later nobody can tell a suite
that passed from a suite that never ran.

# What it produces

One `derived`, validator-origin ledger record per declared gate, plus an
aggregate the CONTROL PLANE computes over them (PRD §10.4: a test suite is a
deterministic validator, and §10.4 admits derived records from exactly those).
The counts and the outcome are not this program's to assert — it reports what
each instrument did, and POST /v1alpha1/runs/{id}/gate-reports does the
arithmetic. That is deliberate: a gate that could report its own aggregate
could report a green one over a report that measured nothing.

# The three answers, and why there are three

    exit 0   gates_passed             every applicable gate measured and passed
    exit 1   changes_required         an applicable gate missed its threshold
    exit 2   measurement_incomplete   a gate that should have measured did not

A gate that can only say pass/fail has to fold "I could not measure this" into
one of them, and both foldings are lies with consequences. Folded into the
passing edge it is the empty-scan false green — a lane with no Go toolchain
reports nothing and the merge looks verified. Folded into the failing edge it
manufactures a defect, and the repair router (issue #102) would then act on it.

These three codes are one contract with three other places: this docstring,
`internal/worker/code.go`'s gate vocabulary, `internal/handover/gate.go`'s
`GateExitCode`, and `examples/merge-gate/workflow.yaml`'s declared outcomes.
`TestGateExitCodesMatchTheLedgerVocabulary` keeps two of them in step; the
other two are prose, and this line is the reminder that they move together.

# Applicability, in the order it is decided

Per gate, and the ORDER is the design:

 1. `responsible_for` matched nothing in the changed set → `not_applicable`
    / `no_source_files`. The change touches nothing this instrument is
    defined over. A docs-only change is this for every code gate.
 2. a required tool is missing on this host → `not_applicable` /
    `instrument_unavailable`, naming the files it owed a measurement on. This
    is the reason that makes the whole report `measurement_incomplete`.
 3. `reaches` matched nothing while `responsible_for` did → `not_applicable` /
    `instrument_not_reaching_tree`. Today's coverage and complexity
    instruments over `internal/`, the adapters and `web/` are exactly this
    (issue #88).
 4. otherwise the gate RUNS, and its exit code is the finding.

Step 1 is deliberately ahead of step 2. Reversed, a docs-only change on a host
without Go would report `measurement_incomplete` for every Go gate — an
instrument that was never owed a measurement is not a measurement that went
missing.

# Where it runs, and where it does not

The gate's declared suites need Go, Node/npm and uv together. Measured this
cycle: only spark has all three. thor has node, npm and uv but `go` is
off-PATH; orin has neither node nor npm at all (confirmed by a dispatched
session reading its own PATH, run 01M05ZGNT86MAFDHATB6W5VYPN).

So making the gate a node moves it out of the operator's SESSION — it becomes
a ledgered derived record instead of a typed command and a read tick — and NOT
off the operator's HOST. This program does not pretend otherwise: on a lane
missing a toolchain it emits `instrument_unavailable`, names the files, and the
run reaches a person. It never reports a pass it did not measure.

# Refusing to gate silently

If the records cannot be recorded, this exits 2. A gate whose finding is not in
the ledger gated nothing, and exiting 0 in that case would be the same green
tick with extra steps.

Environment (all supplied by the runner boundary or the deployment, none
inferred):

    NODES_RUN_ID          the run these records are about. The runner boundary
                          forwards it from the operation's own context
                          (internal/runners.ContextEnvironment); this program
                          never accepts one from its own arguments' guesswork.
    NODES_NODE_RUN_ID     narrows the records to this node run (optional).
    NODES_ATTEMPT_ID      narrows the records to this attempt (optional).
    NODES_API_URL         the control plane.
    NODES_ACTOR_MERGE_GATE_TOKEN
                          the gate's OWN credential: the bearer of the
                          registered `company/merge-gate` agent actor (its row
                          names this variable in metadata.auth_token_env, the
                          same way every dispatched actor's row does). The
                          control plane resolves it to that actor and stamps
                          every record of the report agent-origin, proposed —
                          the gate's claim about the suites it ran, decided by
                          a person like any other claim. The human decision
                          secret is never read here (login-from-anywhere t11,
                          spec c45).
    NODES_GATE_VALIDATOR_ACTOR_ID
                          optional. A registered non-human validator identity
                          to attribute the records to instead; under the agent
                          credential the control plane overrides it with the
                          credential's own actor and says so in its reply.
"""

from __future__ import annotations

import argparse
import fnmatch
import json
import os
import shutil
import subprocess  # noqa: S404 # nosec B404 - argv lists from a pinned matrix, no shell
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

#: The published exit-status contract. See the module docstring.
GATES_PASSED = "gates_passed"
CHANGES_REQUIRED = "changes_required"
MEASUREMENT_INCOMPLETE = "measurement_incomplete"

GATE_EXIT_CODES = {
    GATES_PASSED: 0,
    CHANGES_REQUIRED: 1,
    MEASUREMENT_INCOMPLETE: 2,
}

#: The closed not-applicable vocabulary, mirroring internal/handover/gate.go.
REASON_NOT_REACHING = "instrument_not_reaching_tree"
REASON_NO_TEST_INSTRUMENT = "no_test_instrument"
REASON_UNAVAILABLE = "instrument_unavailable"
REASON_NO_SOURCE_FILES = "no_source_files"
REASON_NO_TESTS_EXECUTED = "no_tests_executed"

#: Every key this program reads out of a gate entry — the whole vocabulary, and
#: nothing beyond it. A matrix is graph content pinned by the published
#: workflow's digest, so an author reads it as the declaration of what gets
#: measured; a key nobody reads declares nothing while looking exactly like it
#: does. `tests/test_merge_gate.py` re-derives this set by walking this module's
#: own AST, so a key taught to the parser without being added here fails rather
#: than quietly becoming a second, undeclared vocabulary.
KNOWN_GATE_KEYS = frozenset(
    {
        "gate",
        "suite",
        "instrument",
        "requires",
        "reaches",
        "responsible_for",
        "command",
        "measurement",
        "repair",
        "cwd",
        "timeout_seconds",
        "version_command",
    }
)

GIT_TIMEOUT_SECONDS = 60.0
HTTP_TIMEOUT_SECONDS = 60.0
DEFAULT_SUITE_TIMEOUT_SECONDS = 1800.0
VERSION_TIMEOUT_SECONDS = 30.0


class Refusal(Exception):
    """A refusal to gate, carrying the exit code it produces.

    The default is 2 — `measurement_incomplete` — and that default is the
    point. Every way this program can fail to do its job is a way it failed to
    MEASURE, and a gate that could not measure must reach a person rather than
    an edge that means something specific about the change.
    """

    def __init__(self, message: str, hint: str, code: int = 2) -> None:
        super().__init__(message)
        self.hint = hint
        self.code = code


def path_matches(pattern: str, path: str) -> bool:
    """Match one repo-relative path against one declared glob.

    Deliberately small, and only three shapes because only three are used:
    `dir/**` (anything under a tree), `**/x.y` (any file named that, at any
    depth), and an ordinary fnmatch pattern. A full globbing implementation
    would be more code and more places for an applicability decision to be
    subtly wrong, which is the one thing this file cannot afford.
    """
    if pattern.endswith("/**"):
        root = pattern[:-3]
        return path == root or path.startswith(root + "/")
    if pattern.startswith("**/"):
        tail = pattern[3:]
        return fnmatch.fnmatch(path, tail) or fnmatch.fnmatch(path.rsplit("/", 1)[-1], tail)
    return fnmatch.fnmatch(path, pattern)


def select(paths: list[str], patterns: list[str]) -> list[str]:
    """The subset of paths matching any pattern, order preserved."""
    return [p for p in paths if any(path_matches(pattern, p) for pattern in patterns)]


def git(repo: Path, *args: str) -> str:
    proc = subprocess.run(  # noqa: S603 # nosec B603 - fixed binary, argv list, no shell
        ["git", *args],
        cwd=str(repo),
        capture_output=True,
        text=True,
        timeout=GIT_TIMEOUT_SECONDS,
        env={**os.environ, "GIT_TERMINAL_PROMPT": "0"},
        check=False,
    )
    if proc.returncode != 0:
        raise Refusal(
            f"git {' '.join(args)} failed in {repo}: {proc.stderr.strip()}",
            "the gate reads its own changed-file basis from git; without it there is no set of "
            "files to decide applicability against, and every gate's verdict would be about "
            "an unknown subject",
        )
    return proc.stdout.strip()


def git_optional(repo: Path, *args: str) -> str:
    proc = subprocess.run(
        ["git", *args],
        cwd=repo,
        capture_output=True,
        text=True,
        check=False,
        env={**os.environ, "GIT_TERMINAL_PROMPT": "0"},
    )
    return proc.stdout.strip() if proc.returncode == 0 else ""


def full_sha(repo: Path, revision: str) -> str:
    sha = git(repo, "rev-parse", revision)
    if len(sha) != 40 or any(c not in "0123456789abcdef" for c in sha):
        raise Refusal(
            f"{revision!r} resolved to {sha!r}, which is not a full 40-character lowercase hex commit id",
            "a verdict that does not name what it tested is not evidence; resolve the revision "
            "in the worktree the suites will actually run in",
        )
    return sha


def load_matrix(spec: str) -> dict[str, Any]:
    """Read the pinned gate matrix, inline or from a file.

    The matrix is GRAPH content: it is pinned in the code node's argv and
    addressed by the published workflow's digest, exactly like every threshold
    in it. That is not decoration — a matrix read out of the tree under test
    would let a change relax the instruments that measure it, which is the one
    thing a gate must not permit. `@file` exists for authoring and for this
    program's own tests, never for the dispatched operation.
    """
    raw = spec
    if spec.startswith("@"):
        try:
            raw = Path(spec[1:]).read_text(encoding="utf-8")
        except OSError as exc:
            raise Refusal(
                f"cannot read the gate matrix at {spec[1:]}: {exc}",
                "pass the matrix inline as JSON so it travels with the pinned operation",
            ) from exc
    try:
        matrix = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise Refusal(
            f"the gate matrix is not valid JSON: {exc}",
            'the matrix is a JSON object: {"base": "...", "gates": [...]}',
        ) from exc
    if (
        not isinstance(matrix, dict)
        or not isinstance(matrix.get("gates"), list)
        or not matrix["gates"]
    ):
        raise Refusal(
            "the gate matrix declares no gates",
            "a report over zero gates has no counts to be counts of, and a merge decided on one "
            "would be decided on nothing",
        )
    for position, gate in enumerate(matrix["gates"], start=1):
        check_gate_declaration(gate, position)
    return matrix


def check_gate_declaration(gate: Any, position: int) -> None:
    """Refuse a gate entry this program cannot read as written.

    An unrecognised key is not a harmless extra (issue #148). The matrix is
    graph content: it is pinned in the code node's argv and addressed by the
    published workflow's digest, so what it says is what a reader believes is
    being measured — and a key nobody reads says nothing while looking exactly
    like it does. The reported instance carried `tools` and `threshold`,
    neither of which this parser has ever read: the tools were never required
    of the host, the threshold was never compared against anything, and the
    gate reported `not_applicable` over an empty set. Nothing complained, and a
    run was decided on it.

    So the refusal happens HERE, at load, before a changed-file set exists and
    long before any instrument runs. A malformed matrix is not a gate that
    measured badly; it is a gate whose declaration was never true, and there is
    nothing it could measure that would make it worth reporting.
    """
    vocabulary = ", ".join(sorted(KNOWN_GATE_KEYS))
    if not isinstance(gate, dict):
        raise Refusal(
            f"gate #{position} in the matrix is {type(gate).__name__}, not an object",
            f"every entry in `gates` is a JSON object declaring one gate; the keys this "
            f"program reads are: {vocabulary}",
        )
    if not gate.get("gate"):
        raise Refusal(
            f"gate #{position} in the matrix has no name",
            "every gate must be named, or its record cannot be counted or queried",
        )
    unknown = sorted(set(gate) - KNOWN_GATE_KEYS)
    if unknown:
        named = ", ".join(repr(key) for key in unknown)
        raise Refusal(
            f"gate {gate['gate']!r} declares {named}, which this gate program never reads",
            f"a key nobody reads declares nothing while looking like it does; either the key "
            f"is a typo or the measurement it names does not exist. The keys this program "
            f"reads are: {vocabulary}",
        )


def instrument_version(gate: dict[str, Any], repo: Path) -> str:
    """Best-effort instrument version. Absence is expected and is not a fault:
    an unavailable instrument has no version to report, which is precisely the
    situation being recorded."""
    command = gate.get("version_command")
    if not command:
        return ""
    try:
        proc = (
            subprocess.run(  # noqa: S603 # nosec B603 - argv list from the pinned matrix, no shell
                command,
                cwd=str(repo),
                capture_output=True,
                text=True,
                timeout=VERSION_TIMEOUT_SECONDS,
                check=False,
            )
        )
    except (OSError, subprocess.SubprocessError):
        return ""
    if proc.returncode != 0:
        return ""
    return proc.stdout.strip().splitlines()[0] if proc.stdout.strip() else ""


def missing_tools(gate: dict[str, Any]) -> list[str]:
    return [tool for tool in gate.get("requires", []) if shutil.which(tool) is None]


def not_applicable(
    gate: dict[str, Any],
    reason: str,
    uncovered: list[str],
    considered: list[str],
    version: str = "",
) -> dict[str, Any]:
    entry: dict[str, Any] = {
        "gate": gate["gate"],
        "not_applicable": {"reason": reason, "uncovered_paths": uncovered},
        "changed_files_considered": considered,
    }
    for key in ("suite", "instrument"):
        if gate.get(key):
            entry[key] = gate[key]
    if version:
        entry["instrument_version"] = version
    return entry


def run_gate(
    gate: dict[str, Any], repo: Path, considered: list[str], version: str
) -> dict[str, Any]:
    """Run one gate's declared command and report what it did.

    A command that cannot be launched at all is `instrument_unavailable`, not
    a failing gate: nothing measured the change, and reporting a failure would
    manufacture a defect. A command that ran and timed out IS a failure — it
    ran, against this change, and did not finish.
    """
    command = gate.get("command")
    if not command:
        return not_applicable(gate, REASON_NO_TEST_INSTRUMENT, considered, considered, version)

    timeout = float(gate.get("timeout_seconds", DEFAULT_SUITE_TIMEOUT_SECONDS))
    # `cwd` is repo-relative and joined here rather than accepted whole: a
    # matrix is graph content, but a graph that could name an absolute path
    # would be one an operator could point at a directory outside the tree
    # under measurement.
    workdir = (repo / gate["cwd"]).resolve() if gate.get("cwd") else repo
    if not str(workdir).startswith(str(repo)):
        raise Refusal(
            f"gate {gate['gate']!r} declares cwd {gate['cwd']!r}, which escapes the worktree",
            "a gate measures the change it was given; a working directory outside it is measuring "
            "something else",
        )
    try:
        run_command = list(command)
        go_json = len(run_command) >= 2 and run_command[:2] == ["go", "test"]
        if go_json and "-json" not in run_command:
            run_command.insert(2, "-json")
        proc = (
            subprocess.run(  # noqa: S603 # nosec B603 - argv list from the pinned matrix, no shell
                run_command,
                cwd=str(workdir),
                capture_output=True,
                text=True,
                timeout=timeout,
                check=False,
            )
        )
        exit_code = proc.returncode
    except FileNotFoundError:
        return not_applicable(gate, REASON_UNAVAILABLE, considered, considered, version)
    except subprocess.TimeoutExpired:
        exit_code = 124

    if exit_code == 0 and go_json:
        executed = False
        for line in proc.stdout.splitlines():
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            if event.get("Test") and event.get("Action") in {"pass", "fail"}:
                executed = True
                break
        if not executed:
            return not_applicable(gate, REASON_NO_TESTS_EXECUTED, considered, considered, version)

    entry: dict[str, Any] = {
        "gate": gate["gate"],
        "suite": gate.get("suite") or gate["gate"],
        "command": list(command),
        "exit_code": exit_code,
        "changed_files_considered": considered,
    }
    if gate.get("instrument"):
        entry["instrument"] = gate["instrument"]
    if version:
        entry["instrument_version"] = version
    measurement = {k: v for k, v in gate.get("measurement", {}).items() if v is not None}
    if measurement:
        entry["measurement"] = measurement
    if gate.get("repair"):
        entry["repair"] = gate["repair"]
    return entry


def evaluate(matrix: dict[str, Any], repo: Path, changed: list[str]) -> list[dict[str, Any]]:
    """Decide and run every declared gate. See the module docstring for why
    the four steps are in this order."""
    entries: list[dict[str, Any]] = []
    for gate in matrix["gates"]:
        # Named, an object, and carrying only keys this parser reads: all of
        # that was settled by `check_gate_declaration` at load, before a
        # changed-file set existed to decide anything against.
        reaches = gate.get("reaches", ["**/*"])
        responsible = gate.get("responsible_for", reaches)

        owed = select(changed, responsible)
        if not owed:
            entries.append(not_applicable(gate, REASON_NO_SOURCE_FILES, [], changed))
            continue

        version = instrument_version(gate, repo)
        missing = missing_tools(gate)
        if missing:
            entries.append(not_applicable(gate, REASON_UNAVAILABLE, owed, changed, version))
            continue

        covered = select(changed, reaches)
        if not covered:
            entries.append(not_applicable(gate, REASON_NOT_REACHING, owed, changed, version))
            continue

        entries.append(run_gate(gate, repo, covered, version))
    return entries


def local_outcome(entries: list[dict[str, Any]]) -> str:
    """The outcome this program computes, mirroring
    internal/handover.GateResults.Outcome.

    It is computed only to be CHECKED against the control plane's, never to be
    reported in its place: the recorded aggregate is the one a reader sees, and
    the node must route on the same answer the ledger holds. Which is exactly
    why the PRECEDENCE has to be the same one, and why every entry is counted
    before any of them decides (issue #153).

    Deciding inside the loop made the first decisive entry the verdict: one
    failing gate and one unavailable instrument reported `changes_required`
    written in that order and `measurement_incomplete` written in the other.
    The two answers are not near neighbours — one routes the run back for
    repair over a defect that was measured, the other says nothing was measured
    and reaches a person — and a matrix's authoring order is not evidence about
    a change. Failures dominate, and are counted across ALL entries first; then
    an unavailable instrument; then a report that measured nothing.
    """
    applicable = 0
    failed = 0
    unavailable = False
    for entry in entries:
        skipped = entry.get("not_applicable")
        if skipped is None:
            applicable += 1
            if entry["exit_code"] != 0:
                failed += 1
        elif skipped["reason"] in {REASON_UNAVAILABLE, REASON_NO_TESTS_EXECUTED}:
            unavailable = True
    if failed > 0:
        return CHANGES_REQUIRED
    if unavailable:
        return MEASUREMENT_INCOMPLETE
    if applicable == 0:
        return MEASUREMENT_INCOMPLETE
    return GATES_PASSED


def env_or_refuse(name: str, hint: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise Refusal(f"{name} is not set", hint)
    return value


def post_report(report: dict[str, Any], run_id: str) -> dict[str, Any]:
    base = env_or_refuse(
        "NODES_API_URL",
        "grant NODES_API_URL to the operation so the gate can record what it measured; "
        "a gate whose finding is not in the ledger gated nothing",
    )
    token = env_or_refuse(
        "NODES_ACTOR_MERGE_GATE_TOKEN",
        "grant NODES_ACTOR_MERGE_GATE_TOKEN, the merge gate's own actor credential "
        "(install-secrets.sh mints it; register-actor.sh binds it to company/merge-gate). "
        "The human decision token is not read here: an agent posts under its own principal",
    )
    url = base.rstrip("/") + f"/v1alpha1/runs/{run_id}/gate-reports"
    request = (
        urllib.request.Request(  # noqa: S310 # nosec B310 - http(s) URL from deployment config
            url,
            data=json.dumps(report).encode("utf-8"),
            headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}"},
            method="POST",
        )
    )
    try:
        with urllib.request.urlopen(
            request, timeout=HTTP_TIMEOUT_SECONDS
        ) as resp:  # noqa: S310 # nosec B310
            return json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", "replace")
        raise Refusal(
            f"the control plane refused the gate report ({exc.code}): {body}",
            "the report is refused rather than half-recorded; fix what it names and re-run the gate",
        ) from exc
    except (urllib.error.URLError, OSError, json.JSONDecodeError) as exc:
        raise Refusal(
            f"the gate report could not be delivered to {url}: {exc}",
            "a gate whose finding was never recorded gated nothing, so this is "
            "measurement_incomplete rather than a pass",
        ) from exc


def render(result: dict[str, Any], entries: list[dict[str, Any]]) -> None:
    counts = result["counts"]
    print(
        f"{counts['applicable_gate_count']} of {counts['declared_gate_count']} declared gate(s) "
        f"measured this change: {counts['passed_gate_count']} passed, "
        f"{counts['failed_gate_count']} failed, "
        f"{counts['not_applicable_gate_count']} not applicable."
    )
    for entry in entries:
        skipped = entry.get("not_applicable")
        if skipped is None:
            state = "PASS" if entry["exit_code"] == 0 else f"FAIL (exit {entry['exit_code']})"
            print(f"  {state:<16} {entry['gate']}")
        else:
            uncovered = skipped["uncovered_paths"]
            names = ", ".join(uncovered[:3]) + ("..." if len(uncovered) > 3 else "")
            print(
                f"  {'NOT APPLICABLE':<16} {entry['gate']}  {skipped['reason']}"
                + (f"  uncovered: {names}" if uncovered else "")
            )
    print(f"outcome: {result['outcome']} (exit {result['exit_code']})")
    print(f"aggregate record: {result['aggregate']['id']}")
    print(
        "a not-applicable gate is NOT a pass: it measured nothing, and the aggregate counts it "
        "separately so an empty scan cannot look green."
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="merge-gate.py",
        description="Run a pinned gate matrix against a change and record what each instrument did.",
    )
    parser.add_argument(
        "--gates",
        required=True,
        help="the pinned gate matrix as JSON, or @path for authoring and tests",
    )
    parser.add_argument("--repo", default=".", help="the worktree the gates run in (default: cwd)")
    parser.add_argument("--base", help="what to diff against; overrides the matrix's own `base`")
    parser.add_argument("--run", help="the run these records are about; defaults to $NODES_RUN_ID")
    parser.add_argument(
        "--check-matrix",
        action="store_true",
        help="check the matrix's shape and key vocabulary, print what it declares, and exit "
        "without measuring or recording anything — the authoring check, and what "
        "scripts/validate-examples.sh runs so a malformed matrix fails in CI rather than "
        "on a live dispatch",
    )
    parser.add_argument(
        "--report-only",
        action="store_true",
        help="compute the report, print it as JSON, record NOTHING, and exit 2 — because a gate "
        "whose finding is not in the ledger gated nothing",
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        matrix = load_matrix(args.gates)

        if args.check_matrix:
            # Deliberately exits 0. This mode makes no claim about a change —
            # it says the declaration is one this program can read, which is a
            # precondition for measuring and never a substitute for it.
            print(f"{len(matrix['gates'])} declared gate(s), every key one this program reads:")
            for gate in matrix["gates"]:
                print(f"  {gate['gate']}")
            print("nothing was measured and nothing was recorded (--check-matrix)")
            return 0

        repo = Path(args.repo).resolve()
        base_ref = args.base or matrix.get("base")
        if not base_ref:
            raise Refusal(
                "no base revision was declared",
                "declare `base` in the pinned matrix (or pass --base): without it there is no "
                "changed-file set, and applicability would be decided against nothing",
            )
        candidate = full_sha(repo, "HEAD")
        base = full_sha(repo, base_ref)
        changed = [
            line
            for line in git(repo, "diff", "--name-only", f"{base}..{candidate}").splitlines()
            if line
        ]

        entries = evaluate(matrix, repo, changed)
        report = {
            "commit_sha": candidate,
            "base_sha": base,
            "changed_files": changed,
            "gates": entries,
        }
        merge_head = git_optional(repo, "rev-parse", "--verify", "HEAD^2")
        if merge_head:
            report["package_commit_sha"] = merge_head

        if args.report_only:
            json.dump(
                {**report, "outcome": local_outcome(entries)}, sys.stdout, indent=2, sort_keys=True
            )
            sys.stdout.write("\n")
            print(
                "nothing was recorded (--report-only), so this is not a gate verdict",
                file=sys.stderr,
            )
            return GATE_EXIT_CODES[MEASUREMENT_INCOMPLETE]

        run_id = args.run or env_or_refuse(
            "NODES_RUN_ID",
            "the runner boundary forwards the run id from the operation's own context "
            "(internal/runners.ContextEnvironment); a validator that cannot name its subject "
            "records nothing",
        )
        # The credential IS the identity: the control plane resolves the
        # bearer to the registered agent actor and stamps the records from
        # it. A separately granted validator id is forwarded when present.
        if os.environ.get("NODES_GATE_VALIDATOR_ACTOR_ID", "").strip():
            report["validator_actor_id"] = os.environ["NODES_GATE_VALIDATOR_ACTOR_ID"].strip()
        for key, name in (
            ("node_run_ref", "NODES_NODE_RUN_ID"),
            ("attempt_ref", "NODES_ATTEMPT_ID"),
        ):
            if os.environ.get(name):
                report[key] = os.environ[name]

        result = post_report(report, run_id)
        render(result, entries)

        expected = local_outcome(entries)
        if result["outcome"] != expected:
            raise Refusal(
                f"the control plane recorded outcome {result['outcome']!r} where this gate computed "
                f"{expected!r}",
                "the node must route on the same answer the ledger holds; this exits "
                "measurement_incomplete so a person reconciles the two rather than a run taking an "
                "edge the records do not support",
            )
        return int(result["exit_code"])
    except Refusal as refusal:
        print(f"error: {refusal}", file=sys.stderr)
        print(f"hint: {refusal.hint}", file=sys.stderr)
        return refusal.code


if __name__ == "__main__":
    raise SystemExit(main())
