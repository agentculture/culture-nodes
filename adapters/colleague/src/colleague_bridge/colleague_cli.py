"""Subprocess boundary: everything that shells out to the `colleague` binary.

Pinned against colleague contract v1 (`/home/spark/git/colleague/docs/contract.md`,
read-only reference) and `docs/features/flight.md`:

* ``colleague work "<instruction>" --repo PATH --json [--background] [--role
  r] [--mode m] [--max-steps N]`` — TaskResult statuses ``ok`` | ``error`` |
  ``incomplete`` map to exit codes 0 | 1 | 2.
* ``--background`` prints ``{background, id, pid, log_dir, flight}``
  immediately and detaches; the SAME id is both the colleague `task_id` and
  the flight id (`colleague/background.py`: the child re-invocation carries
  ``COLLEAGUE_BACKGROUND_ID``, and `cmd_work` stamps it onto `task.id`
  before `--background` force-arms `--watch`), so a caller can go straight
  from the start payload to `.colleague/flight/<id>.feed.jsonl` and the
  detached child's own `--json` result on
  ``.colleague/background/<id>/stdout.log``.
* Steering/stop is cooperative — writing the flight control file, never a
  signal (see `flightfiles.py`); this module's own timeout handling for the
  SYNCHRONOUS path likewise sends SIGTERM, never SIGKILL, matching
  `docs/features/flight.md`'s "prefer a cooperative stop over a hard
  timeout" and colleague's own SIGTERM handler (which commits partial WIP
  before exiting).

Nothing here imports the `colleague` Python package — see the package
docstring for why (stdlib-only reference bridge).
"""

from __future__ import annotations

import json
import os
import subprocess
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from colleague_bridge.config import Config

#: Built-in colleague roles (colleague/roles.py BUILTIN_ROLES) — a role
#: outside this set is only valid if the target repo declares it as a
#: custom override under `.colleague/agents/<role>.md`.
BUILTIN_ROLES = frozenset({"explorer", "planner", "reviewer", "validator", "writer"})


def role_is_known(repo: str | Path, role: str) -> bool:
    """True iff *role* is a colleague built-in, or the repo declares an override."""
    if role in BUILTIN_ROLES:
        return True
    return (Path(repo) / ".colleague" / "agents" / f"{role}.md").is_file()


def _common_argv(
    instruction: str,
    repo: str,
    *,
    role: str | None,
    max_steps: int | None,
    mode: str | None,
    open_pr: bool,
    allow_dirty: bool,
) -> list[str]:
    argv = ["work", instruction, "--repo", repo, "--json"]
    if not open_pr:
        argv.append("--no-pr")
    if allow_dirty:
        argv.append("--allow-dirty")
    if role:
        argv += ["--role", role]
    if max_steps is not None:
        argv += ["--max-steps", str(max_steps)]
    if mode:
        argv += ["--mode", mode]
    return argv


def _subprocess_env(cfg: Config) -> dict[str, str]:
    env = dict(os.environ)
    env.update(cfg.colleague_env)
    return env


@dataclass
class SyncRunResult:
    """The outcome of one foreground `colleague work` invocation."""

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
    role: str | None = None,
    max_steps: int | None = None,
    mode: str | None = None,
) -> SyncRunResult:
    """Run `colleague work ...` in the foreground and wait for it to finish.

    On a timeout the child is sent SIGTERM — never SIGKILL, per the
    cooperative-stop contract — and given a grace period to exit on its own
    before this function gives up waiting (the process may still be
    committing partial WIP after that; this function does not block on it
    forever, but it never escalates to SIGKILL itself).
    """
    argv = [
        cfg.colleague_bin,
        *_common_argv(
            instruction,
            repo,
            role=role,
            max_steps=max_steps,
            mode=mode,
            open_pr=cfg.open_pr,
            allow_dirty=cfg.allow_dirty,
        ),
        "--no-watch",
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
        proc.terminate()  # SIGTERM: cooperative, colleague commits WIP and exits
        try:
            stdout, stderr = proc.communicate(timeout=max(cfg.sync_timeout_seconds * 0.2, 5.0))
        except subprocess.TimeoutExpired:
            # Still not done. Never SIGKILL — leave it running and report the
            # timeout honestly; an operator can inspect/finish reaping later.
            stdout, stderr = "", "colleague did not exit after SIGTERM within the grace period"
        timed_out = True

    task_result = _parse_task_result(stdout)
    return SyncRunResult(
        exit_code=proc.returncode,
        stdout=stdout,
        stderr=stderr,
        task_result=task_result,
        timed_out=timed_out,
    )


def _parse_task_result(stdout: str) -> dict[str, Any] | None:
    """The last non-blank line of stdout, parsed as JSON, or None.

    `--json` mode writes exactly one JSON line to stdout (colleague's own
    stdout/stderr split contract); "last non-blank line" is a small extra
    safety margin against any stray leading blank line, never against
    diagnostics — those go to stderr, not stdout, by the same contract.
    """
    for line in reversed(stdout.splitlines()):
        line = line.strip()
        if not line:
            continue
        try:
            parsed = json.loads(line)
        except ValueError:
            return None
        return parsed if isinstance(parsed, dict) else None
    return None


@dataclass
class BackgroundStart:
    """The `{background, id, pid, log_dir, flight}` start payload."""

    handle_id: str
    pid: int
    log_dir: str
    flight: str | None


class BackgroundDispatchError(Exception):
    """The `--background` parent call itself failed or produced no payload."""

    def __init__(self, message: str, *, stderr: str = ""):
        super().__init__(message)
        self.stderr = stderr


def spawn_background(
    cfg: Config,
    instruction: str,
    repo: str,
    *,
    role: str | None = None,
    max_steps: int | None = None,
    mode: str | None = None,
) -> BackgroundStart:
    """Run `colleague work --background ...` and return its start payload.

    This call is expected back almost immediately (`colleague/background.py`:
    the parent only mints a handle id and `Popen`s the detached child before
    returning) — it is bounded by
    `Config.background_dispatch_timeout_seconds` as a defensive ceiling, not
    because the normal case is slow.
    """
    argv = [
        cfg.colleague_bin,
        *_common_argv(
            instruction,
            repo,
            role=role,
            max_steps=max_steps,
            mode=mode,
            open_pr=cfg.open_pr,
            allow_dirty=cfg.allow_dirty,
        ),
        "--background",
    ]

    try:
        proc = subprocess.run(  # noqa: S603 - the sanctioned subprocess boundary
            argv,
            cwd=repo,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            stdin=subprocess.DEVNULL,
            env=_subprocess_env(cfg),
            text=True,
            timeout=cfg.background_dispatch_timeout_seconds,
        )
    except subprocess.TimeoutExpired as exc:
        raise BackgroundDispatchError(
            "colleague work --background did not return its start payload in time",
            stderr=(exc.stderr or ""),
        ) from exc

    payload = _parse_task_result(proc.stdout)
    if proc.returncode != 0 or not payload or "id" not in payload:
        raise BackgroundDispatchError(
            f"colleague work --background exited {proc.returncode} without a start payload",
            stderr=proc.stderr,
        )
    return BackgroundStart(
        handle_id=str(payload["id"]),
        pid=int(payload.get("pid") or 0),
        log_dir=str(payload.get("log_dir") or ""),
        flight=payload.get("flight"),
    )


def background_stdout_path(repo: str | Path, handle_id: str) -> Path:
    """``<repo>/.colleague/background/<handle_id>/stdout.log`` (colleague/background.py)."""
    return Path(repo) / ".colleague" / "background" / handle_id / "stdout.log"


def is_pid_alive(pid: int) -> bool:
    """Mirrors colleague/background.py's own liveness probe: signal 0, no kill."""
    if not isinstance(pid, int) or pid <= 0:
        return False
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except OSError:
        return True
    return True


def read_background_result(repo: str | Path, handle_id: str) -> dict[str, Any] | None:
    """Read the detached child's `--json` result from its stdout log, if written yet."""
    path = background_stdout_path(repo, handle_id)
    try:
        text = path.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return None
    return _parse_task_result(text)


def wait_for_background_result(
    repo: str | Path,
    handle_id: str,
    pid: int,
    *,
    poll_interval_seconds: float,
    deadline: float,
) -> dict[str, Any] | None:
    """Poll until the detached child writes a parseable result or *deadline* passes.

    *deadline* is an absolute `time.monotonic()` value, not a duration —
    callers computing several bounds (e.g. "N seconds from when the 202 was
    sent") should compute it once, not re-derive it every call.
    """
    while True:
        result = read_background_result(repo, handle_id)
        if result is not None:
            return result
        if not is_pid_alive(pid):
            # The child is gone but never finished writing a result we could
            # parse (e.g. it died between writing a partial line and
            # flushing) — one last read in case of a benign race, else give up.
            return read_background_result(repo, handle_id)
        if time.monotonic() >= deadline:
            return None
        time.sleep(poll_interval_seconds)


# Cancellation is cooperative and file-based only (`flightfiles.write_stop`).
# This module never sends a signal to a colleague process for cancellation —
# `run_sync`'s own SIGTERM-on-timeout above is the ONE signal this module
# ever sends, and only to bound a hung SYNCHRONOUS foreground call, never as
# a response to a cancellation request.
