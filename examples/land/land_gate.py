#!/usr/bin/env python3
"""The land node's GATE and its single version bump (loop-closure task t7;
spec c3/c7/c15/c36, honesty h9/h12/h16; issues #315, #316, #309 step 2).

land.py reaches this module at its `gate` step, after the handover commits
have been rebased onto the target branch tip in a scratch worktree and
BEFORE anything is pushed. What runs here is the operator's pre-push chain
from CLAUDE.md, in that worktree, in this order:

    tests        the target pytest selection      LAND_GATE_TESTS
                                                  (default `uv run pytest -n auto -q`)
    go_lint      go test ./tests/lint/...          (the Go guards, file-length included)
    lint_all     scripts/lint-all.sh <job>         LAND_GATE_JOB (default `root`),
                 run with LINT_ALL_SKIP=triage (LAND_LINT_ALL_SKIP): that step
                 needs an authenticated `gh`, which culture-land has not got
    file_length  the 1000-line hard limit over tracked source, in-process
                 (tests/lint/filelength_test.go's rule, so a host whose Go
                 guard is misconfigured still cannot land a 1400-line file)

then EXACTLY ONE version bump for the whole landing -- `bump.py <part>` fed a
JSON changelog on STDIN whose `changed` entries name the findings landed
(LAND_FINDINGS_JSON, else the subjects of the landed commits), committed as
one commit ON TOP of the rebased handover commits. The handover commits are
never rewritten, and the bump commit is what land.py pushes. A handover
that already bumps pyproject's version (the pre-t7 behaviour, where a fix
session read the bump rule itself) is landed as it is, with the bump step
recorded as skipped -- one bump per landing means one, not two.

# What the ledger sees

The `gate` step record carries every chain step with its argv, exit code,
duration and the tail of its output, plus the bump (old, new, commit,
findings). A RED step is a derived routing record in internal/repair's shape
(router `land_routing`, reason `gate_failed`, the failing step's name and
tail in the rationale), the step is recorded as `routed`, and land.py stops:
nothing is pushed and the branch is exactly where it was. A step that hangs
past LAND_GATE_STEP_TIMEOUT_SECONDS (default 600) is the same red gate with
`timed_out: true` -- a hung suite is a person's problem, not the lander's.

RED is a MEASURED failure, and only lint-all may say anything else. Its own
exit-code policy is 1 for "I measured a finding" and 2 for "I could not
measure that step" (an unauthenticated `gh`, an unreachable GitHub -- the
same code merge-gate.py uses for measurement_incomplete). Exit 2 makes the
gate's outcome `measurement_incomplete`, naming the steps the script's own
summary reported as UNRUNNABLE, and the landing PROCEEDS: routing a person
to a defect nobody found, on a chain whose every measured step was green,
would make every production landing red for a fact about the host. Exit 1
still routes, and so does exit 2 from any other step.

That amnesty is granted to the CLAIM, not to the exit code: exit 2 reads as
"could not measure" only when the script NAMED at least one step it could not
run. lint-all exits 2 for a usage error too -- an unknown job, a `cd` that
failed -- and that run linted nothing at all, so an exit 2 that names nothing
is a red gate like any other.

# The lint job, declared and refused by name

LINT_ALL_JOBS restates the five jobs scripts/lint-all.sh runs, and an
LAND_GATE_JOB outside it is refused BEFORE the chain starts
(`lint_job_unknown`, exit 2), the way a missing toolchain is. Left to the
chain, `LAND_GATE_JOB=roott` would be exit 2 from the script's own usage
error -- and the paragraph above is why that could otherwise have landed a
handover on which no linter ever ran.

# The two host preconditions, declared and refused by name

This module owns both facts about the HOST that a landing cannot proceed
without, and land.py reaches the first of them BEFORE its fetch rather than
at the gate: `check_workspace` measures that the job started IN culture-land's
CHECKOUT (a git repository with an `origin`). That is a deployment fact and
not a graph field -- `operation.workspaceRef` is a JSON Pointer into the run's
own surfaces and no node upstream of `land` produces the checkout as an
artifact, while a container-backed runner's workspace is a copy it destroys on
the way out, which cannot hold a branch lease that has to outlive the job
(examples/land/workflow.yaml, "THE CHECKOUT THIS NODE RUNS IN"). So a runner
that started this job in an empty directory is `workspace_not_a_checkout`
naming the path, exit 2 -- not `fatal: not a git repository` three git calls
into the fetch.

# Toolchains, declared and refused by name

REQUIRED_TOOLCHAINS names the four binaries the chain needs and what each is
for. They are checked on PATH before the first step starts; a missing one is
a `toolchain_missing` record naming the binary, a `refused` gate step, and
an environment refusal (exit 2) -- so "Go is not on culture-land's PATH on
thor" (spec s26, 2026-09-07) reads as that sentence in the ledger instead
of `go: command not found` three steps into a tail. The deploy reports the
same fact on the host side (deploy/prod/lanes/land-toolchain.sh).

# Why the gate's inputs ride the environment (recorded deviation)

The plan names `gate_tests`, `gate_job` and `findings` as node inputs.
land.py's input contract admits exactly its six keys and refuses any other
(read_inputs), and widening it is an edit outside the one hook body this
task may touch while t8 edits the same file. So the three arrive as optional
environment values with the production defaults built in -- and `findings`
defaults to a fact git already has, the landed commits' subjects, so a
production landing needs none of them set. Admitting them as bound inputs is
the follow-up once both tasks have merged.

# What this never does

No push (land.py's step), no `--force`, no merge, no HTTP, no amend. The
only writes are inside the scratch worktree: bump.py's edits and the one
commit that captures them.
"""

from __future__ import annotations

import json
import os
import re
import shlex
import shutil
import subprocess  # noqa: S404 # nosec B404 - fixed binaries, argv lists, no shell
import sys
import time
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable

#: The binaries the chain needs, and what each is for -- the declaration the
#: workflow prose repeats and deploy/prod/lanes/land-toolchain.sh probes.
REQUIRED_TOOLCHAINS: dict[str, str] = {
    "go": "go test ./tests/lint/... (the Go lint guards, file-length among them)",
    "uv": "the target pytest selection (uv run pytest ...) and scripts/lint-all.sh's linters",
    "node": "the runtime markdownlint-cli2 needs (scripts/lint-all.sh root's markdownlint step)",
    "markdownlint-cli2": "scripts/lint-all.sh root's markdownlint step",
}

DEFAULT_GATE_TESTS = "uv run pytest -n auto -q"
DEFAULT_GATE_JOB = "root"

#: The job names scripts/lint-all.sh runs, restated here the way
#: MAX_SOURCE_FILE_LINES restates the Go guard's rule -- so the gate can
#: refuse an unknown one BEFORE it runs the script. It must, because the
#: script answers a typo the way it answers an unauthenticated `gh`: exit 2,
#: which read as measurement_incomplete would land a handover on which lint
#: never ran at all. tests/test_land_gate.py pins this against the script's
#: own `--list`, so widening the list there cannot leave the gate behind.
LINT_ALL_JOBS = ("root", "adapter-codex", "adapter-claude-code", "adapter-pi", "adapter-qwen")
DEFAULT_BUMP_PART = "patch"
DEFAULT_STEP_TIMEOUT_SECONDS = 600.0
BUMP_TIMEOUT_SECONDS = 60.0

GO_LINT_ARGV = ("go", "test", "./tests/lint/...")
LINT_ALL_SCRIPT = "scripts/lint-all.sh"
BUMP_SCRIPT = ".claude/skills/version-bump/scripts/bump.py"

#: scripts/lint-all.sh's own exit-code policy: 1 is "I measured a finding",
#: 2 is "I could not measure that step" -- the same code merge-gate.py uses
#: for measurement_incomplete and _errors.py reserves for an environment
#: error. Only lint_all is granted it here: a pytest run or a Go build that
#: exits 2 is a failure like any other.
MEASUREMENT_INCOMPLETE_EXIT = 2
OUTCOME_MEASUREMENT_INCOMPLETE = "measurement_incomplete"

#: The lint-all steps the gate waives before it starts, as LINT_ALL_SKIP.
#: `triage` needs an authenticated `gh` and a reachable GitHub, which the
#: culture-land account has NEITHER of -- unwaived it exits 2 on every
#: production landing. LAND_LINT_ALL_SKIP overrides it; an empty value
#: waives nothing (and lint-all then reports triage as UNRUNNABLE, which is
#: recorded rather than red).
DEFAULT_LINT_ALL_SKIP = "triage"

#: The step names scripts/lint-all.sh's own output says could not run --
#: per step (`<<< UNRUNNABLE <name> (exit 2 ...`) and in its summary
#: (`UNRUNNABLE: a b`). Parsed rather than assumed, so the record names what
#: the script named.
UNRUNNABLE_STEP_LINE = re.compile(r"^<<< UNRUNNABLE (\S+)", re.MULTILINE)
UNRUNNABLE_SUMMARY_LINE = re.compile(r"^UNRUNNABLE:\s*(.+)$", re.MULTILINE)

#: The land-specific routing reason this module adds to land.py's vocabulary.
REASON_GATE_FAILED = "gate_failed"
RATIONALE_GATE = (
    "the gate step {step} ({argv}) exited {code} on the rebased handover {handover}; nothing was "
    "pushed and the branch is untouched -- a red gate is a person's decision, not a retry. "
    "Tail:\n{tail}"
)

#: tests/lint/filelength_test.go's rule, restated so the gate has it without Go.
MAX_SOURCE_FILE_LINES = 1000
SOURCE_FILE_EXTENSIONS = frozenset(
    {".c", ".cc", ".go", ".h", ".js", ".jsx", ".mjs", ".py", ".sh", ".sql", ".ts", ".tsx"}
)

TAIL_LINES = 40
TAIL_CHARS = 4000

BUMP_AUTHOR = {
    "GIT_AUTHOR_NAME": "culture-nodes land node",
    "GIT_AUTHOR_EMAIL": "land@culture-nodes.invalid",
}

GitFn = Callable[..., subprocess.CompletedProcess]


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def land_module(ctx: Any):
    """The land.py module THIS ctx came from -- `__main__` under the runner,
    `land` under the tests. Importing it by name would mint a second copy
    whose Refusal/Routed classes land.main() does not catch."""
    return sys.modules[type(ctx).__module__]


#: The caller's own Refusal class, handed in by land.py's gate hook exactly as
#: land_reply.py takes it (t8). `sys.modules[...]` by name is only a fallback:
#: when two copies of land.py have been loaded in one process (the test suite
#: does that), the name resolves to the newest copy and a refusal raised from
#: here would sail past the caller's `except Refusal`.
_REFUSAL: type[Exception] | None = None


def refusal_class(ctx: Any) -> type[Exception]:
    return _REFUSAL or land_module(ctx).Refusal


def tail(text: str, redact: Callable[[str], str]) -> str:
    lines = text.splitlines()[-TAIL_LINES:]
    return redact("\n".join(lines)[-TAIL_CHARS:])


# -- configuration -----------------------------------------------------------


@dataclass(frozen=True)
class GateConfig:
    tests_argv: tuple[str, ...]
    job: str
    bump_part: str
    step_timeout: float
    findings_override: tuple[str, ...] | None
    lint_all_skip: str


def gate_config(env: dict[str, str] | None = None) -> GateConfig:
    env = os.environ if env is None else env
    tests = shlex.split(env.get("LAND_GATE_TESTS") or DEFAULT_GATE_TESTS)
    override = None
    raw = env.get("LAND_FINDINGS_JSON")
    if raw:
        parsed = json.loads(raw)
        if not isinstance(parsed, list) or not all(isinstance(f, str) and f for f in parsed):
            raise ValueError("LAND_FINDINGS_JSON must be a JSON list of non-empty strings")
        override = tuple(parsed)
    return GateConfig(
        tests_argv=tuple(tests),
        job=env.get("LAND_GATE_JOB") or DEFAULT_GATE_JOB,
        bump_part=env.get("LAND_BUMP_PART") or DEFAULT_BUMP_PART,
        step_timeout=float(
            env.get("LAND_GATE_STEP_TIMEOUT_SECONDS") or DEFAULT_STEP_TIMEOUT_SECONDS
        ),
        findings_override=override,
        lint_all_skip=env.get("LAND_LINT_ALL_SKIP", DEFAULT_LINT_ALL_SKIP).strip(),
    )


def missing_toolchains(env: dict[str, str] | None = None) -> list[str]:
    path = (os.environ if env is None else env).get("PATH", "")
    return [name for name in REQUIRED_TOOLCHAINS if shutil.which(name, path=path) is None]


# -- the chain ----------------------------------------------------------------


@dataclass
class StepResult:
    name: str
    argv: list[str]
    exit_code: int | None
    duration_s: float
    tail: str
    timed_out: bool = False
    #: The step answered "I could not measure" rather than "this is wrong"
    #: (see MEASUREMENT_INCOMPLETE_EXIT), with the names it could not run.
    incomplete: bool = False
    unrunnable: list[str] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return self.exit_code == 0 and not self.timed_out

    @property
    def red(self) -> bool:
        """A measured failure -- the only thing that routes a landing. An
        unmeasured step is recorded and named, never routed: routing
        `gate_failed` on it would send a person to look at a defect that was
        never found, on a landing whose every measured step was green."""
        return not self.ok and not self.incomplete


def unrunnable_steps(text: str) -> list[str]:
    """The step names scripts/lint-all.sh said this environment could not
    run, from the script's own per-step lines and its summary."""
    names = set(UNRUNNABLE_STEP_LINE.findall(text))
    for line in UNRUNNABLE_SUMMARY_LINE.findall(text):
        names.update(line.split())
    return sorted(names)


def run_step(
    name: str,
    argv: tuple[str, ...],
    cwd: Path,
    timeout: float,
    redact: Callable[[str], str],
    *,
    env: dict[str, str] | None = None,
    incomplete_exit: int | None = None,
) -> StepResult:
    started = time.monotonic()
    try:
        proc = subprocess.run(  # nosec B603 - operator-declared argv, no shell
            list(argv),
            cwd=str(cwd),
            capture_output=True,
            text=True,
            timeout=timeout,
            check=False,
            stdin=subprocess.DEVNULL,
            env={**os.environ, **env} if env else None,
        )
    except subprocess.TimeoutExpired as exc:
        text = (
            (exc.stdout or b"").decode("utf-8", "replace")
            if isinstance(exc.stdout, bytes)
            else (exc.stdout or "")
        )
        note = f"\n[gate] {name} exceeded {timeout:g}s and was killed"
        return StepResult(
            name, list(argv), None, time.monotonic() - started, tail(text + note, redact), True
        )
    text = proc.stdout + proc.stderr
    # "I could not measure" is a claim the script has to SUPPORT: the exit
    # code alone is not the fact -- scripts/lint-all.sh exits 2 for a usage
    # error too, and that one linted nothing. So the step is incomplete only
    # when the script named at least one step it could not run.
    unrunnable = unrunnable_steps(text)
    incomplete = (
        incomplete_exit is not None and proc.returncode == incomplete_exit and bool(unrunnable)
    )
    return StepResult(
        name,
        list(argv),
        proc.returncode,
        time.monotonic() - started,
        tail(text, redact),
        incomplete=incomplete,
        unrunnable=unrunnable if incomplete else [],
    )


def count_lines(contents: bytes) -> int:
    lines = contents.count(b"\n")
    if contents and not contents.endswith(b"\n"):
        lines += 1
    return lines


def oversized_files(git: GitFn, worktree: Path) -> list[str]:
    """Tracked source files over the hard limit, `path: N lines` each."""
    listed = git(worktree, "ls-files", "-z").stdout
    hits: list[str] = []
    for rel in listed.split("\0"):
        if not rel or Path(rel).suffix not in SOURCE_FILE_EXTENSIONS:
            continue
        try:
            lines = count_lines((worktree / rel).read_bytes())
        except OSError:
            continue
        if lines > MAX_SOURCE_FILE_LINES:
            hits.append(f"{rel}: {lines} lines")
    return sorted(hits)


def file_length_step(git: GitFn, worktree: Path) -> StepResult:
    started = time.monotonic()
    hits = oversized_files(git, worktree)
    text = (
        "\n".join(hits)
        if hits
        else f"every tracked source file is within {MAX_SOURCE_FILE_LINES} lines"
    )
    argv = ["<in-process>", f"max_lines={MAX_SOURCE_FILE_LINES}"]
    return StepResult("file_length", argv, 1 if hits else 0, time.monotonic() - started, text)


# -- the bump -----------------------------------------------------------------


def landed_findings(git: GitFn, worktree: Path, base: str, cfg: GateConfig) -> list[str]:
    if cfg.findings_override is not None:
        return list(cfg.findings_override)
    subjects = git(worktree, "log", "--reverse", "--format=%s", f"{base}..HEAD").stdout
    return [line for line in subjects.splitlines() if line.strip()]


def handover_already_bumps(git: GitFn, worktree: Path, base: str) -> bool:
    diff = git(worktree, "diff", base, "HEAD", "--", "pyproject.toml").stdout
    return any(line.startswith("+version = ") for line in diff.splitlines())


def bump_version(
    git: GitFn, worktree: Path, part: str, changelog: dict[str, list[str]]
) -> tuple[str, str, str]:
    """Run the repository's own bump.py in the worktree, the changelog JSON
    on STDIN (bump.py blocks on a TTY-less stdin with nothing to read; this
    always gives it something). Returns (old, new, bump.py's stdout)."""
    script = worktree / BUMP_SCRIPT
    if not script.is_file():
        raise FileNotFoundError(f"{BUMP_SCRIPT} is not in the landed tree")
    proc = subprocess.run(  # nosec B603 - the repository's own script, argv list
        [sys.executable, str(script), part],
        cwd=str(worktree),
        input=json.dumps(changelog),
        capture_output=True,
        text=True,
        timeout=BUMP_TIMEOUT_SECONDS,
        check=False,
    )
    if proc.returncode != 0:
        raise RuntimeError(
            f"bump.py {part} exited {proc.returncode}: {(proc.stderr or proc.stdout).strip()}"
        )
    arrow = [line for line in proc.stdout.splitlines() if " -> " in line]
    if not arrow:
        raise RuntimeError(f"bump.py printed no `old -> new` line: {proc.stdout.strip()}")
    old, new = (part.strip() for part in arrow[-1].split("->", 1))
    return old, new, proc.stdout


def commit_bump(git: GitFn, worktree: Path, subject: str, body: str) -> str:
    git(worktree, "add", "-u")
    git(worktree, "commit", "--quiet", "-m", subject, "-m", body, env=dict(BUMP_AUTHOR))
    return git(worktree, "rev-parse", "HEAD").stdout.strip()


# -- the gate -----------------------------------------------------------------


def emit(ctx: Any, record: dict[str, Any]) -> None:
    """A record outside land.py's step/result/routing trio, on the same
    stream with the same join keys (Records._emit is the one writer)."""
    ctx.records._emit({**record, **ctx.records.common})  # noqa: SLF001


def refuse_missing(ctx: Any, land: Any, missing: list[str]) -> None:
    for name in missing:
        emit(
            ctx,
            {
                "record": "toolchain_missing",
                "binary": name,
                "needed_for": REQUIRED_TOOLCHAINS[name],
                "path": os.environ.get("PATH", ""),
                "at": now_iso(),
            },
        )
    ctx.records.step(
        "gate",
        "refused",
        reason="toolchain_missing",
        missing=missing,
        required=list(REQUIRED_TOOLCHAINS),
    )
    raise refusal_class(ctx)(
        f"gate toolchain missing on this host: {', '.join(missing)}",
        "install it on the runner host for the culture-land account (deploy.sh's "
        "land_toolchain_check reports the same fact per binary); installing is a "
        "counted hand-turn",
        land.EXIT_ENVIRONMENT,
    )


#: An unknown LAND_GATE_JOB, refused by name before the chain starts. The
#: script itself answers a typo with exit 2 (`error: unknown job`, no step
#: run) and exit 2 is the gate's "could not measure" code, so left to the
#: chain a typo'd job would land a handover that lint never looked at.
REASON_LINT_JOB_UNKNOWN = "lint_job_unknown"
LINT_JOB_HINT = (
    "set LAND_GATE_JOB to one of: {known} (scripts/lint-all.sh --list) -- the script exits 2 on "
    "an unknown job WITHOUT running a linter, and exit 2 is the code it uses for a step it could "
    "not measure, so an unlinted landing would read as a merely incomplete one"
)


def refuse_lint_job(ctx: Any, land: Any, job: str) -> None:
    emit(
        ctx,
        {
            "record": REASON_LINT_JOB_UNKNOWN,
            "job": job,
            "known": list(LINT_ALL_JOBS),
            "script": LINT_ALL_SCRIPT,
            "at": now_iso(),
        },
    )
    ctx.records.step(
        "gate", "refused", reason=REASON_LINT_JOB_UNKNOWN, job=job, known=list(LINT_ALL_JOBS)
    )
    raise refusal_class(ctx)(
        f"{job!r} is not a job {LINT_ALL_SCRIPT} runs",
        LINT_JOB_HINT.format(known=", ".join(LINT_ALL_JOBS)),
        land.EXIT_ENVIRONMENT,
    )


#: The remediation lines the two workspace refusals carry. Both name the
#: DEPLOYMENT, because neither is anything a graph or a re-run can fix.
WORKSPACE_HINT = (
    "deploy the land runner service so its jobs run IN culture-land's checkout and hand that "
    "path to the operation as NODES_WORKSPACE (examples/land/workflow.yaml, 'THE CHECKOUT THIS "
    "NODE RUNS IN'); a code node cannot declare it"
)
ORIGIN_HINT = (
    "point the land checkout's `origin` at the repository this node pushes to -- it is the "
    "remote the credential is scoped to and the one the branch tip is read from"
)


def refuse_workspace(ctx: Any, land: Any, reason: str, message: str, hint: str) -> None:
    ctx.records.step("workspace", "refused", reason=reason, workspace=str(ctx.workspace))
    raise refusal_class(ctx)(message, hint, land.EXIT_ENVIRONMENT)


def check_workspace(ctx: Any, refusal: type[Exception] | None = None) -> dict[str, Any]:
    """land.py's `workspace` step -- the host precondition it reaches BEFORE
    the fetch. Returns the step's fields; refuses by name when the job did not
    start in a checkout (see this module's docstring for why that can only be
    measured, never declared)."""
    global _REFUSAL
    _REFUSAL = refusal
    land = land_module(ctx)
    ws = ctx.workspace
    common = ("rev-parse", "--path-format=absolute", "--git-common-dir")
    git_dir = land.git(ws, *common, check=False)
    if git_dir.returncode != 0:
        refuse_workspace(
            ctx, land, "workspace_not_a_checkout", f"{ws} is not a git checkout", WORKSPACE_HINT
        )
    origin = land.git(ws, "remote", "get-url", land.PUSH_REMOTE, check=False)
    if origin.returncode != 0:
        refuse_workspace(
            ctx,
            land,
            "origin_not_configured",
            f"{ws} has no {land.PUSH_REMOTE} remote",
            ORIGIN_HINT,
        )
    return {
        "outcome": "ok",
        "workspace": str(ws),
        "git_dir": git_dir.stdout.strip(),
        "origin": land.redact(origin.stdout.strip()),
    }


def route_red(ctx: Any, land: Any, failed: StepResult, done: list[StepResult]) -> None:
    rationale = RATIONALE_GATE.format(
        step=failed.name,
        argv=" ".join(failed.argv),
        code="timeout" if failed.timed_out else failed.exit_code,
        handover=land.short(ctx.landed_commit),
        tail=failed.tail,
    )
    ctx.route(
        "gate",
        REASON_GATE_FAILED,
        rationale,
        failing_step=failed.name,
        exit_code=failed.exit_code,
        timed_out=failed.timed_out,
        tail=failed.tail,
        steps=[asdict(s) for s in done],
        rebased_commit=ctx.landed_commit,
        target_tip=ctx.target_tip,
    )


def run_gate(ctx: Any, *, refusal: type[Exception] | None = None) -> dict[str, Any]:
    """land.py's gate hook. Returns the `gate` step's fields on ok/skipped;
    on a missing toolchain or a red step it writes the step record itself
    and raises land's Refusal / Routed. `refusal` is the caller's own class."""
    global _REFUSAL
    _REFUSAL = refusal
    land = land_module(ctx)
    if ctx.worktree is None:
        return {"outcome": "skipped", "reason": "already_on_branch", "bump": None}
    try:
        cfg = gate_config()
    except (ValueError, json.JSONDecodeError) as exc:
        raise refusal_class(ctx)(
            f"gate configuration: {exc}", "fix the land account's environment"
        ) from exc
    missing = missing_toolchains()
    if missing:
        refuse_missing(ctx, land, missing)
    if cfg.job not in LINT_ALL_JOBS:
        refuse_lint_job(ctx, land, cfg.job)

    wt: Path = ctx.worktree
    lint_env = {"LINT_ALL_SKIP": cfg.lint_all_skip} if cfg.lint_all_skip else {}
    chain: tuple[tuple[str, tuple[str, ...], dict[str, str], int | None], ...] = (
        ("tests", cfg.tests_argv, {}, None),
        ("go_lint", GO_LINT_ARGV, {}, None),
        ("lint_all", ("bash", LINT_ALL_SCRIPT, cfg.job), lint_env, MEASUREMENT_INCOMPLETE_EXIT),
    )
    done: list[StepResult] = []
    for name, argv, env, incomplete_exit in chain:
        res = run_step(
            name, argv, wt, cfg.step_timeout, land.redact, env=env, incomplete_exit=incomplete_exit
        )
        done.append(res)
        if res.red:
            route_red(ctx, land, res, done)
    res = file_length_step(land.git, wt)
    done.append(res)
    if res.red:
        route_red(ctx, land, res, done)

    base = ctx.target_tip
    if handover_already_bumps(land.git, wt, base):
        bump: dict[str, Any] = {"skipped": "handover_already_bumps"}
    else:
        findings = landed_findings(land.git, wt, base, cfg)
        if not findings:
            findings = [f"handover {land.short(ctx.handover_commit)} from run {ctx.inputs.run_id}"]
        changelog = {"changed": [f"{ctx.inputs.work_item}: {finding}" for finding in findings]}
        try:
            old, new, _stdout = bump_version(land.git, wt, cfg.bump_part, changelog)
        except (OSError, RuntimeError, subprocess.TimeoutExpired) as exc:
            raise refusal_class(ctx)(
                f"version bump failed: {exc}", "the gate was green; bump.py itself broke"
            ) from exc
        subject = (
            f"land {ctx.inputs.work_item}: version {old} -> {new} ({len(findings)} finding(s))"
        )
        body = "\n".join(
            [
                f"Landed by the land node (run {ctx.land.run_id}) from run {ctx.inputs.run_id}",
                f"onto {ctx.inputs.target_branch}; one bump for the whole landing (#316).",
                "",
                *(f"- {finding}" for finding in findings),
            ]
        )
        commit = commit_bump(land.git, wt, subject, body)
        ctx.landed_commit = commit
        bump = {
            "old": old,
            "new": new,
            "part": cfg.bump_part,
            "commit": commit,
            "findings": findings,
        }

    toolchains = {name: shutil.which(name) for name in REQUIRED_TOOLCHAINS}
    # A step that could not measure is named in the record and does not route:
    # the landing proceeds knowing exactly which check nobody ran, rather than
    # reading "green" for a chain that partly did not happen (#146's shape).
    incomplete = [
        {"step": s.name, "exit_code": s.exit_code, "unrunnable": s.unrunnable}
        for s in done
        if s.incomplete
    ]
    return {
        "outcome": OUTCOME_MEASUREMENT_INCOMPLETE if incomplete else "ok",
        "steps": [asdict(s) for s in done],
        "bump": bump,
        "landed_commit": ctx.landed_commit,
        "toolchains": toolchains,
        "measurement_incomplete": incomplete,
        "lint_all_skip": cfg.lint_all_skip,
    }
