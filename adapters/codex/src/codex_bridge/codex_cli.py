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

import json
import os
import subprocess
from dataclasses import dataclass
from typing import Any

from codex_bridge.config import Config

#: `codex exec --sandbox` accepts exactly these three values
#: (`codex exec --help`). This bridge never passes
#: `--dangerously-bypass-approvals-and-sandbox` — an invocation may only
#: ever pick among these three, explicit, always-sandboxed modes.
SANDBOX_MODES = frozenset({"read-only", "workspace-write", "danger-full-access"})

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
    """
    if continuation_ref:
        argv = ["exec", "resume", continuation_ref, "--json"]
        if model:
            argv += ["-m", model]
        argv.append(instruction)
        return argv
    argv = ["exec", "--json", "--sandbox", sandbox, "-C", repo]
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
