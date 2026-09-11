"""Subprocess boundary: everything that shells out to the `codex` binary.

Pinned against codex-cli 0.144.6 (`codex exec --help`, and real, grounded
`codex exec --json` transcripts captured on the machine this adapter was
built on — see README.md's "What a codex session's JSONL looks like" for
the full un-trimmed evidence). This module owns two things: the exact argv
`codex exec` is invoked with (`_common_argv`), and — the load-bearing piece
of this whole adapter — turning a captured JSONL transcript into a
TaskResult-shaped dict (`parse_session`) that `mapping.py` classifies
exactly the way it classifies colleague's own `TaskResult`.

The three shapes `parse_session` distinguishes, grounded against real
output:

* **ok** — a `turn.completed` event was seen (an explicit terminal signal
  that the turn finished). `codex exec` also exits 0 in this case, but the
  exit code is NOT what this module trusts; the terminal event is.
* **error** — a `turn.failed` event was seen. `codex exec` also exits
  non-zero here, again incidentally, not authoritatively.
* **incomplete** — the transcript ends WITHOUT ever reaching a terminal
  turn event. This is deliberately the default/fallback classification, not
  a special case: a session killed by this bridge's own SIGTERM-on-timeout,
  a session that crashed for any other reason, or (measured directly while
  grounding this adapter) a session that caught SIGTERM and exited cleanly
  with code 0 while mid-turn — ALL of these produce a transcript with no
  terminal event, and ALL of them are "incomplete", never "ok". There is no
  branch anywhere in this module that promotes an exit code of 0 to success
  on its own; only `turn.completed`'s presence does that. This is the
  concrete mechanism behind this task's acceptance criterion: an incomplete
  or crashed codex session maps to failure, never success.

Only when NOT ONE JSON line parses at all (stdout was empty, or garbage —
e.g. the codex binary itself failed to start) does `parse_session` return
`None`, mirroring colleague-bridge's own "no parseable result at all" case
in `colleague_cli._parse_task_result`.

Nothing here imports any codex Python package — codex-cli is a standalone
binary (npm/pip wrapper or a bare executable); this module only ever
shells out to it (stdlib-only reference bridge, matching
`colleague_bridge`'s own stance).
"""

from __future__ import annotations

import dataclasses
import json
import os
import subprocess
import tempfile
from dataclasses import dataclass
from typing import Any

from codex_bridge import liveness
from codex_bridge.config import Config

#: `codex exec --sandbox` accepts exactly these three values
#: (`codex exec --help`). This bridge never passes
#: `--dangerously-bypass-approvals-and-sandbox` — an invocation may only
#: ever pick among these three, explicit, always-sandboxed modes.
SANDBOX_MODES = frozenset({"read-only", "workspace-write", "danger-full-access"})

#: The sandbox mode `.git` write can be widened within. Only this one: under
#: `read-only` a writable `.git` would contradict the mode outright, and under
#: `danger-full-access` nothing is confined to widen.
SANDBOX_WORKSPACE_WRITE = "workspace-write"


def git_writable_override(repo: str) -> str:
    """The one `-c` override that makes `.git` writable inside a
    `workspace-write` session (task t6, issue #91, deviation d6).

    MEASURED, not guessed: under plain `--sandbox workspace-write` on
    codex-cli 0.147.0 the worktree is writable and `.git` is NOT — `fetch`,
    `commit` and `update-ref` all fail with `Read-only file system` while
    editing files works, which reads like a code problem and is a sandbox
    carve-out. Adding this single scoped entry lifts exactly that, and a full
    write-tree/commit-tree/update-ref then succeeds (commit df7d974 at
    `refs/culture-nodes/probe`, on thor).

    It is deliberately a per-dispatch OPT-IN, never a default: a package that
    hands over no ref has no reason to write `.git`, and handing every session
    that authority to save a flag would widen the sandbox for the majority to
    serve the minority. The widening is also scoped to `.git` alone — it is
    not `danger-full-access`, which #91 established is not needed here.
    """
    return f'sandbox_workspace_write={{writable_roots=["{repo.rstrip("/")}/.git"]}}'


#: JSONL event types this module treats as terminal for a turn. Anything
#: else (thread.started, turn.started, item.started, item.completed,
#: standalone "error" notices, ...) is non-terminal — informative for
#: progress reporting, never authoritative for the session's outcome.
_TERMINAL_OK = "turn.completed"
_TERMINAL_FAILED = "turn.failed"


def _common_argv(
    instruction: str,
    repo: str,
    *,
    model: str | None,
    sandbox: str,
    continuation_ref: str | None = None,
    writable_git: bool = False,
) -> list[str]:
    """The `codex exec` argv this bridge generates, minus the binary name
    itself (`Config.codex_bin` is prepended by the caller). Mirrors
    `colleague_cli._common_argv`'s role in that module: the ONE place the
    real command line is assembled, so a test can assert the exact
    argument list without spawning anything.

    *continuation_ref* (task t5): codex's own resume verb is a SEPARATE
    subcommand, `codex exec resume <SESSION_ID> [PROMPT]`
    (`codex exec resume --help`, verified against codex-cli 0.147.0 on
    PATH while building this) — not a flag layered onto plain `exec`. Its
    flag surface is narrower than `exec`'s own: no `-C`/`--cd` and no
    `-s`/`--sandbox` — a resumed session already knows its working
    directory and sandbox policy from when it first started, so passing
    either would be asserting something resume does not accept (confirmed
    against the real binary's own `--help`, which lists neither for the
    `resume` subcommand). `-C repo` remains unnecessary for another reason
    too: the subprocess itself is spawned with `cwd=repo` (see `run_sync`/
    `spawn`), so the OS-level working directory is right either way — `-C`
    is codex's own internal echo of that fact for a fresh session, not the
    only way this bridge controls it.

    *writable_git* (task t6) opts THIS dispatch into a writable `.git`, which
    a session must have to create the handover ref its changes travel on
    (`preserve.handover_ref`). It applies only to a fresh `workspace-write`
    session, for the same reason `--sandbox` itself does not appear on the
    resume line: a resumed session already carries the sandbox policy it
    started with, so a dispatch that will hand over a ref has to say so on
    its FIRST turn rather than discovering the need mid-session.
    """
    if continuation_ref:
        argv = ["exec", "resume", continuation_ref, "--json"]
        if model:
            argv += ["-m", model]
        argv.append(instruction)
        return argv
    argv = ["exec", "--json", "--sandbox", sandbox, "-C", repo]
    if writable_git and sandbox == SANDBOX_WORKSPACE_WRITE:
        argv += ["-c", git_writable_override(repo)]
    if model:
        argv += ["-m", model]
    argv.append(instruction)
    return argv


def _subprocess_env(cfg: Config) -> dict[str, str]:
    env = dict(os.environ)
    env.update(cfg.codex_env)
    return env


def parse_session(stdout: str) -> dict[str, Any] | None:
    """Turn a full captured `codex exec --json` stdout transcript into a
    TaskResult-shaped dict: `{status, summary, changed_files, usage,
    task_id, error}` — the same shape `mapping.py` expects, deliberately
    matching colleague's own `TaskResult` vocabulary (`ok`/`error`/
    `incomplete`) so `mapping.classify()` needs no codex-specific branch.

    Unlike `colleague_cli._parse_task_result` (which only ever reads the
    LAST stdout line, because colleague's `--json` mode prints exactly one
    JSON object), this function scans every line: codex's `--json` mode
    streams one event per line for the whole session, and the single event
    that decides success/failure/incompleteness — a terminal
    `turn.completed` or `turn.failed` — is not necessarily the last line
    written to a pipe that may have been read mid-stream, and is
    NEVER present at all in the crashed/incomplete case this function
    exists to get right.

    Returns `None` only when not one line parsed as a JSON object — codex
    produced no parseable output whatsoever (the "no parseable result"
    case, mirroring colleague-bridge's own).
    """
    saw_any_json = False
    thread_id: str | None = None
    model: str | None = None
    messages: list[str] = []
    changed_files: list[str] = []
    usage: dict[str, Any] = {}
    error_message: str | None = None
    termination_reason: str | None = None
    terminal_status: str | None = None  # None | "ok" | "error"

    for line in stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        saw_any_json = True

        kind = event.get("type")

        # Usage can accompany a failed turn, and newer event-stream shapes
        # may publish a running total before a terminal event. Keep the most
        # recent non-empty provider report regardless of event kind: that
        # preserves failed-turn accounting and also lets a transcript that
        # ends incomplete retain counts already emitted before it stopped.
        reported_usage = event.get("usage")
        if isinstance(reported_usage, dict) and reported_usage:
            usage = reported_usage

        # Do not infer the model from argv/config: only provider-reported
        # stream metadata belongs in usage telemetry. A model nested in the
        # usage report and a top-level event model are both honest sources.
        reported_model = event.get("model")
        if not isinstance(reported_model, str) or not reported_model:
            reported_model = (
                reported_usage.get("model") if isinstance(reported_usage, dict) else None
            )
        if isinstance(reported_model, str) and reported_model:
            model = reported_model

        if kind == "thread.started":
            thread_id = event.get("thread_id")
            continue

        if kind == "item.completed":
            item = event.get("item") or {}
            item_type = item.get("type")
            if item_type == "agent_message":
                text = item.get("text")
                if text:
                    messages.append(str(text))
            elif item_type == "file_change" and item.get("status") == "completed":
                for change in item.get("changes") or []:
                    path = change.get("path") if isinstance(change, dict) else None
                    if path and path not in changed_files:
                        changed_files.append(path)
            elif item_type == "error":
                # An item-level error notice; captured as a fallback message
                # only — it does not, by itself, decide the terminal status.
                msg = item.get("message")
                if msg and error_message is None:
                    error_message = str(msg)
            continue

        if kind == "error":
            # A standalone top-level error notice (observed alongside, and
            # ahead of, an eventual turn.failed in real output). Captured as
            # a fallback message only; never itself terminal.
            msg = event.get("message")
            if msg and error_message is None:
                error_message = str(msg)
            continue

        if kind == _TERMINAL_OK:
            terminal_status = "ok"
            reason = event.get("reason") or event.get("stop_reason")
            if isinstance(reason, str) and reason:
                termination_reason = reason
            continue

        if kind == _TERMINAL_FAILED:
            terminal_status = "error"
            reason = event.get("reason") or event.get("stop_reason")
            if isinstance(reason, str) and reason:
                termination_reason = reason
            err = event.get("error")
            if isinstance(err, dict) and err.get("message"):
                error_message = str(err["message"])
            elif err:
                error_message = str(err)
            continue

    if not saw_any_json:
        return None

    summary = messages[-1] if messages else ""

    if terminal_status == "ok":
        return {
            "status": "ok",
            "summary": summary,
            "changed_files": changed_files,
            "usage": usage,
            "task_id": thread_id,
            "error": None,
            "model": model,
            "termination_reason": termination_reason,
        }

    if terminal_status == "error":
        return {
            "status": "error",
            "summary": summary,
            "changed_files": changed_files,
            "usage": usage,
            "task_id": thread_id,
            "error": error_message or "codex reported a turn failure",
            "model": model,
            "termination_reason": termination_reason,
        }

    # No terminal event was ever seen: killed, crashed, or the bridge's own
    # timeout fired — regardless of process exit code. Never "ok".
    return {
        "status": "incomplete",
        "summary": summary,
        "changed_files": changed_files,
        "usage": usage,
        "task_id": thread_id,
        "error": None,
        "model": model,
        "termination_reason": termination_reason,
    }


@dataclass
class SyncRunResult:
    """The outcome of one foreground `codex exec` invocation."""

    exit_code: int | None
    stdout: str
    stderr: str
    task_result: dict[str, Any] | None
    timed_out: bool


def run_sync(
    cfg: Config,
    instruction: str,
    repo: str,
    *,
    model: str | None = None,
    sandbox: str | None = None,
    continuation_ref: str | None = None,
    writable_git: bool = False,
) -> SyncRunResult:
    """Run `codex exec ...` in the foreground and wait for it to finish.

    On a timeout the child is sent SIGTERM — never SIGKILL, mirroring
    `colleague_cli.run_sync`'s own cooperative-stop stance. Grounded
    evidence (README.md) shows codex-cli 0.144.6 responds to SIGTERM by
    exiting quickly and cleanly (exit code 0) WITHOUT ever emitting a
    terminal turn event — which is exactly why `parse_session` above never
    trusts exit code, only the presence of that event.
    """
    argv = [
        cfg.codex_bin,
        *_common_argv(
            instruction,
            repo,
            model=model,
            sandbox=sandbox or cfg.default_sandbox,
            continuation_ref=continuation_ref,
            writable_git=writable_git,
        ),
    ]

    proc = subprocess.Popen(  # noqa: S603 - the sanctioned subprocess boundary
        argv,
        cwd=repo,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        stdin=subprocess.DEVNULL,
        env=_subprocess_env(cfg),
        text=True,
    )
    try:
        stdout, stderr = proc.communicate(timeout=cfg.sync_timeout_seconds)
        timed_out = False
    except subprocess.TimeoutExpired:
        proc.terminate()  # SIGTERM: cooperative, never SIGKILL
        try:
            stdout, stderr = proc.communicate(timeout=max(cfg.sync_timeout_seconds * 0.2, 5.0))
        except subprocess.TimeoutExpired:
            # Still not done. Never SIGKILL — leave it running and report
            # the timeout honestly; an operator can inspect/finish reaping
            # later.
            stdout, stderr = "", "codex did not exit after SIGTERM within the grace period"
        timed_out = True

    task_result = parse_session(stdout)
    return SyncRunResult(
        exit_code=proc.returncode,
        stdout=stdout,
        stderr=stderr,
        task_result=task_result,
        timed_out=timed_out,
    )


class SpawnError(Exception):
    """The `codex exec` subprocess itself could not be started at all (e.g.
    the binary is missing) — distinct from any classification `codex`
    itself might report once running. Mirrors colleague-bridge's own
    `BackgroundDispatchError` role: `server.py` maps this to a 503."""

    def __init__(self, message: str):
        super().__init__(message)


def spawn(
    cfg: Config,
    instruction: str,
    repo: str,
    *,
    model: str | None = None,
    sandbox: str | None = None,
    continuation_ref: str | None = None,
    writable_git: bool = False,
) -> subprocess.Popen:
    """Start `codex exec ...` in the background and return the live
    `Popen` handle immediately (near-instant — `Popen` never blocks on the
    child).

    Unlike `colleague_cli.spawn_background` (which shells out to a
    `colleague work --background` PARENT call that detaches an unrelated
    child colleague-bridge has to re-discover by PID + result file), this
    bridge owns the `codex exec` subprocess directly for its entire
    lifetime — codex has no equivalent detach-and-reattach flag — so the
    async runner can read this process's stdout pipe as it streams and
    terminate it directly for cancellation, with no file-based
    control-plane convention to mirror.

    *continuation_ref* (task t5): threaded through the same way `run_sync`
    does — the async path is the one long, therefore resume-worth-it,
    sessions actually take.
    """
    argv = [
        cfg.codex_bin,
        *_common_argv(
            instruction,
            repo,
            model=model,
            sandbox=sandbox or cfg.default_sandbox,
            continuation_ref=continuation_ref,
            writable_git=writable_git,
        ),
    ]
    try:
        return subprocess.Popen(  # noqa: S603 - the sanctioned subprocess boundary
            argv,
            cwd=repo,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            stdin=subprocess.DEVNULL,
            env=_subprocess_env(cfg),
            text=True,
            bufsize=1,  # line-buffered: a JSONL consumer wants one line per read
        )
    except OSError as exc:
        raise SpawnError(f"could not start {cfg.codex_bin!r}: {exc}") from exc


# --- lane liveness (issue #308, task t9) ------------------------------------
#
# Two things, both about the one failure the codex lanes have already paid
# for twice: a spent refresh token behind a healthy-looking bridge.
#
# `liveness_probe` is the CHECK-mode measurement (spec decision c24): the
# cheapest `codex exec` that still has to refresh the token — read-only, no
# repo, a one-line instruction, bounded at `Config.liveness_probe_timeout_
# seconds`. `codex login status` is deliberately not consulted: whether it
# says "Logged in" while the refresh token is spent was the open question
# (spec v1), and a dry exec answers the question the router actually asks.
#
# `with_credential_refusal` is the LOCK-mode hook and the class the control
# plane reads: the live failure printed the sentence to STDERR and emitted no
# terminal turn event, so `parse_session` alone says "no parseable result" and
# mapping reports a generic execution failure. Attaching the refusal to the
# task result before mapping sees it is what makes `credential_spent` a
# class of its own without teaching `mapping.py` about stderr.

#: One line, one token's worth of work. Not a shell command: `--sandbox
#: read-only` confines whatever the model does with it anyway.
LIVENESS_PROBE_INSTRUCTION = "Reply with exactly: OK"


def liveness_probe_argv(cwd: str) -> list[str]:
    """The dry probe's argv, minus the binary. `--skip-git-repo-check`
    because the probe runs in an empty scratch directory: it measures the
    session, not a checkout."""
    return [
        "exec",
        "--json",
        "--sandbox",
        "read-only",
        "--skip-git-repo-check",
        "-C",
        cwd,
        LIVENESS_PROBE_INSTRUCTION,
    ]


def liveness_probe(cfg: Config, *, timeout_seconds: float | None = None) -> dict[str, Any]:
    """Measure this lane's session with one dry exec; never raises.

    A completed turn is `session_ok=true`; the spent-credential sentence
    anywhere in the output is `session_ok=false reason=refresh_token_spent`;
    a never-logged-in lane's `401 Unauthorized: Missing bearer` is
    `session_ok=false reason=not_logged_in` (code-review finding 8 — it
    carries none of the spent-token phrasing, so it used to read as an
    unclassified failure and every reader was free to call the lane live);
    a timeout, a missing binary, or any other failure is `null` with a
    reason that says so — a probe that could not run is not evidence the
    lane is dead, and a false negative parks a lane that works.
    """
    mode = liveness.parse_mode(cfg.liveness_mode)
    budget = cfg.liveness_probe_timeout_seconds if timeout_seconds is None else timeout_seconds
    with tempfile.TemporaryDirectory(prefix="codex-liveness-") as scratch:
        try:
            completed = subprocess.run(  # noqa: S603 - the sanctioned subprocess boundary
                [cfg.codex_bin, *liveness_probe_argv(scratch)],
                cwd=scratch,
                capture_output=True,
                stdin=subprocess.DEVNULL,
                env=_subprocess_env(cfg),
                text=True,
                timeout=budget,
                check=False,
            )
        except subprocess.TimeoutExpired:
            return liveness.liveness_fact(
                session_ok=None, reason=liveness.REASON_PROBE_TIMEOUT, mode=mode
            )
        except OSError:
            return liveness.liveness_fact(
                session_ok=None, reason=liveness.REASON_PROBE_FAILED, mode=mode
            )
    if liveness.credential_spent(completed.stdout, completed.stderr):
        return liveness.liveness_fact(
            session_ok=False, reason=liveness.REASON_REFRESH_TOKEN_SPENT, mode=mode
        )
    task_result = parse_session(completed.stdout)
    if task_result is not None and task_result.get("status") == "ok":
        return liveness.liveness_fact(session_ok=True, reason=liveness.REASON_OK, mode=mode)
    # No completed turn. A 401 is a measured verdict — this lane has no
    # session — while anything else stays the honest non-answer.
    if liveness.not_logged_in(
        str((task_result or {}).get("error") or ""), completed.stdout, completed.stderr
    ):
        return liveness.liveness_fact(
            session_ok=False, reason=liveness.REASON_NOT_LOGGED_IN, mode=mode
        )
    return liveness.liveness_fact(session_ok=None, reason=liveness.REASON_PROBE_FAILED, mode=mode)


def _spent_sentence(*texts: str) -> str:
    """The line that carried the sentence, so the message a human reads is
    the engine's own words rather than this bridge's paraphrase."""
    for text in texts:
        for line in (text or "").splitlines():
            if liveness.credential_spent(line):
                return line.strip()
    return "codex refused the session: the refresh token is spent"


def credential_refusal(
    task_result: dict[str, Any] | None,
    *texts: str,
    liveness_state: "liveness.LivenessState | None" = None,
) -> dict[str, Any] | None:
    """Return *task_result* rewritten as a `status: error` whose `error` is
    the spent-credential sentence when *texts* (stdout, stderr) or the
    result's own error carry it; otherwise *task_result* unchanged. A
    completed turn is never rewritten. Locks *liveness_state* when given —
    the LOCK-mode half of decision q7, and the OR that CHECK mode keeps.
    """
    if task_result is not None and task_result.get("status") == "ok":
        return task_result
    own_error = str((task_result or {}).get("error") or "")
    if not liveness.credential_spent(own_error, *texts):
        return task_result
    if liveness_state is not None:
        liveness_state.lock(liveness.REASON_REFRESH_TOKEN_SPENT)
    refused = dict(
        task_result or {"summary": "", "changed_files": [], "usage": {}, "task_id": None}
    )
    refused["status"] = "error"
    refused["error"] = _spent_sentence(own_error, *texts)
    return refused


def with_credential_refusal(
    result: SyncRunResult, *, liveness_state: "liveness.LivenessState | None" = None
) -> SyncRunResult:
    """`credential_refusal` over a foreground run's captured output."""
    refused = credential_refusal(
        result.task_result, result.stdout, result.stderr, liveness_state=liveness_state
    )
    if refused is result.task_result:
        return result
    return dataclasses.replace(result, task_result=refused)
