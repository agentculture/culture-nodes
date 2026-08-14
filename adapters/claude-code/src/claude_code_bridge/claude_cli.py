"""Subprocess boundary: everything that shells out to the `claude` binary.

Pinned against the headless "print mode" surface of the Claude Code CLI (the
same subprocess boundary `claude_agent_sdk`'s own
`SubprocessCLITransport` drives — see that package's
`_internal/transport/subprocess_cli.py`, which this module is deliberately
NOT a dependency on: this bridge shells out to the `claude` binary directly,
the same discipline `adapters/colleague/src/colleague_bridge/colleague_cli.py`
uses for `colleague`):

* ``claude -p "<instruction>" --output-format json --permission-mode <mode>
  [--add-dir <repo>] [--agent <role>] [--model <model>] [--max-turns N]`` —
  prints exactly ONE JSON object to stdout: a `type: "result"` message
  (`subtype: "success"` and `is_error: false` on success; any other
  `subtype`/`is_error` combination is a failure — see `mapping.py`). Run in
  the foreground for a SYNCHRONOUS invocation.
* ``claude -p "<instruction>" --output-format stream-json --verbose ...`` —
  prints a JSONL stream of progress records followed by the same terminal
  `result` object, one line at a time as the session runs. Used for the
  ASYNCHRONOUS path: this bridge spawns it as a detached background process
  itself (claude's own CLI has no `--background`/start-payload protocol the
  way colleague's does — see `spawn_background`'s docstring) and tails its
  stdout the way `adapters/colleague` tails `colleague`'s flight feed (see
  `flightfiles.py`).
* Cancellation is a direct, cooperative SIGTERM to the subprocess — never
  SIGKILL. Unlike colleague, the `claude` CLI has no external file-based
  control plane of its own to poll for a stop request; this bridge's own
  `flightfiles.py` control file is instead polled BY this bridge's own async
  poller (`async_runner.py`), which is what turns a written stop request
  into the SIGTERM. See flightfiles.py's module docstring for the full
  explanation of that deviation from colleague's native flight protocol.

Nothing here imports `claude_agent_sdk` or any other PyPI package — see the
package docstring for why (stdlib-only reference bridge).
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import threading
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from claude_code_bridge import flightfiles
from claude_code_bridge.config import Config, ConfigError

#: Matches the leading `X.Y.Z` claude prints as the first token of
#: `claude --version`'s output (observed shape: "2.1.226 (Claude Code)").
_VERSION_RE = re.compile(r"^(\d+)\.(\d+)\.(\d+)")


def parse_version(raw: str) -> tuple[int, int, int] | None:
    """Extract a leading `(major, minor, patch)` from a `claude --version`
    line, or None if the string does not start with one."""
    m = _VERSION_RE.match(raw.strip())
    if not m:
        return None
    return (int(m.group(1)), int(m.group(2)), int(m.group(3)))


class ClaudeVersionProbeError(Exception):
    """`claude --version` could not be run, or its output could not be
    parsed as a version. Fail closed: a bridge that cannot prove the CLI it
    is about to shell out to is new enough must refuse, the same as one it
    has proven is too old — see `ensure_supported_version`."""


class UnsupportedClaudeVersionError(Exception):
    """The resolved `claude` binary reports a version below this bridge's
    pinned minimum. Names BOTH versions in the message — an operator reading
    this in a log needs to know what they have and what they need without
    cross-referencing anything else."""

    def __init__(self, detected: str, minimum: str) -> None:
        self.detected = detected
        self.minimum = minimum
        super().__init__(
            f"claude CLI version {detected} is below this bridge's pinned minimum "
            f"{minimum}; refusing to dispatch rather than run against a CLI version "
            f"this bridge has not been validated against"
        )


def probe_version(cfg: Config) -> str:
    """Run `<claude_bin> --version` and return its raw stripped output
    (e.g. "2.1.226 (Claude Code)"). Raises ClaudeVersionProbeError if the
    binary cannot be run at all, times out, or produces empty output."""
    try:
        proc = subprocess.run(  # noqa: S603 - the sanctioned subprocess boundary
            [cfg.claude_bin, "--version"],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            stdin=subprocess.DEVNULL,
            env=_subprocess_env(cfg),
            text=True,
            timeout=15,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise ClaudeVersionProbeError(f"could not run {cfg.claude_bin!r} --version: {exc}") from exc
    raw = (proc.stdout or "").strip()
    if not raw:
        stderr = (proc.stderr or "").strip()
        raise ClaudeVersionProbeError(
            f"{cfg.claude_bin!r} --version produced no output (stderr: {stderr!r})"
        )
    return raw


def ensure_supported_version(cfg: Config) -> None:
    """Refuse to proceed unless the resolved `claude` binary reports a
    version at or above `cfg.min_claude_version`.

    Called at the top of both `run_sync` and `spawn_background` — the one
    choke point every dispatch path passes through, the same way
    `mapping.classify()` is the one choke point "incomplete is never
    success" is enforced at. A version that cannot be determined is treated
    as too old (fail closed): an adapter that cannot prove its CLI is new
    enough has no basis to claim it is.
    """
    minimum = parse_version(cfg.min_claude_version)
    if minimum is None:
        raise ConfigError(
            f"min_claude_version {cfg.min_claude_version!r} is not a valid X.Y.Z version"
        )

    raw = probe_version(cfg)
    detected = parse_version(raw)
    if detected is None:
        raise ClaudeVersionProbeError(
            f"could not parse a version out of {cfg.claude_bin!r} --version output: {raw!r}"
        )
    if detected < minimum:
        raise UnsupportedClaudeVersionError(detected=raw, minimum=cfg.min_claude_version)


#: Built-in claude-code subagent names this bridge recognises without a repo
#: override. Unlike colleague, the `claude` CLI ships no fixed built-in role
#: set of its own — `--agent <name>` only resolves against a project's own
#: `.claude/agents/<name>.md` definition (or one supplied via `--agents`,
#: which this bridge does not use). Kept as an explicit empty set — rather
#: than omitting the concept — so the parallel with
#: `colleague_cli.role_is_known` stays visible: this module still validates
#: an `input.role`/`--agent`, it simply has nothing built in to validate it
#: against besides what the target repo declares.
BUILTIN_AGENTS: frozenset[str] = frozenset()


def role_is_known(repo: str | Path, role: str) -> bool:
    """True iff *role* is a claude-code built-in (none, today — see
    BUILTIN_AGENTS), or the repo declares `.claude/agents/<role>.md`."""
    if role in BUILTIN_AGENTS:
        return True
    return (Path(repo) / ".claude" / "agents" / f"{role}.md").is_file()


def _common_argv(
    instruction: str,
    *,
    output_format: str,
    permission_mode: str,
    role: str | None,
    max_steps: int | None,
    model: str | None,
    verbose: bool = False,
    continuation_ref: str | None = None,
) -> list[str]:
    argv = [
        "-p",
        instruction,
        "--output-format",
        output_format,
        "--permission-mode",
        permission_mode,
    ]
    if continuation_ref:
        argv += ["--resume", continuation_ref]
    if role:
        argv += ["--agent", role]
    if max_steps is not None:
        argv += ["--max-turns", str(max_steps)]
    if model:
        argv += ["--model", model]
    if verbose:
        argv.append("--verbose")
    return argv


def _subprocess_env(cfg: Config) -> dict[str, str]:
    env = dict(os.environ)
    env.update(cfg.claude_env)
    return env


@dataclass
class SyncRunResult:
    """The outcome of one foreground `claude -p` invocation."""

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
    model: str | None = None,
    continuation_ref: str | None = None,
) -> SyncRunResult:
    """Run `claude -p ...` in the foreground and wait for it to finish.

    On a timeout the child is sent SIGTERM — never SIGKILL, per the
    cooperative-stop discipline this bridge shares with `adapters/colleague`
    — and given a grace period to exit on its own before this function gives
    up waiting.
    """
    ensure_supported_version(cfg)

    argv = [
        cfg.claude_bin,
        *_common_argv(
            instruction,
            output_format="json",
            permission_mode=cfg.permission_mode,
            role=role,
            max_steps=max_steps,
            model=model or (cfg.model or None),
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
            # Still not done. Never SIGKILL — leave it running and report the
            # timeout honestly; an operator can inspect/finish reaping later.
            stdout, stderr = "", "claude did not exit after SIGTERM within the grace period"
        timed_out = True

    task_result = parse_task_result(stdout)
    return SyncRunResult(
        exit_code=proc.returncode,
        stdout=stdout,
        stderr=stderr,
        task_result=task_result,
        timed_out=timed_out,
    )


def parse_task_result(stdout: str) -> dict[str, Any] | None:
    """The last non-blank line of stdout, parsed as JSON, or None.

    `--output-format json` writes exactly one JSON line to stdout; "last
    non-blank line" is the same small safety margin
    `colleague_cli._parse_task_result` takes against a stray leading blank
    line. A crashed session that wrote no JSON at all (or wrote something
    that is not a JSON object — e.g. a plain-text engine error) parses as
    None, which `mapping.classify()` reports as an execution failure, never
    success (`tests/test_claude_cli.py::test_crashed_session_never_success`
    exercises exactly this).
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


def find_terminal_result(lines: list[str]) -> dict[str, Any] | None:
    """Scan a list of raw stream-json lines for the terminal `type: "result"`
    record, returning the LAST one found (a session should only ever emit
    one, but "last" is a defensive choice consistent with
    `parse_task_result`'s own "last line wins" stance)."""
    found: dict[str, Any] | None = None
    for line in lines:
        line = line.strip()
        if not line:
            continue
        try:
            parsed = json.loads(line)
        except ValueError:
            continue
        if isinstance(parsed, dict) and parsed.get("type") == "result":
            found = parsed
    return found


@dataclass
class BackgroundStart:
    """The start handle for a detached background `claude -p` invocation.

    Unlike colleague's `--background`, which prints its own
    `{background, id, pid, log_dir, flight}` start payload, the `claude` CLI
    has no such protocol in headless print mode — this bridge mints the
    handle id itself and manages the detached process directly (see
    `spawn_background`).
    """

    handle_id: str
    pid: int
    log_path: str


class BackgroundDispatchError(Exception):
    """Spawning the detached background `claude -p` process itself failed."""

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
    model: str | None = None,
    continuation_ref: str | None = None,
) -> BackgroundStart:
    """Start `claude -p ... --output-format stream-json` as a detached
    background process and return immediately with its handle.

    `start_new_session=True` detaches the child from this process's session
    (the claude equivalent of colleague's own OS-level background detach),
    so the child survives this bridge process restarting; a restarted
    bridge does not currently re-attach to it (no acceptance criterion here
    requires that — see README.md), but the child itself is not killed by
    the bridge going away either.

    *continuation_ref* (task t5): the async path resumes exactly the same
    way the sync path does (`run_sync`) — a long-running turn is precisely
    the one most likely to answer asynchronously, so leaving this parameter
    off `spawn_background` would have made resume unreachable on the path
    that needs it most (ADR 0010's own framing of the gap it closed for the
    *result* side of this same asymmetry).
    """
    ensure_supported_version(cfg)

    handle_id = f"cc_{uuid.uuid4().hex}"
    log_path = flightfiles.feed_path(cfg.state_dir, handle_id)
    log_path.parent.mkdir(parents=True, exist_ok=True)

    argv = [
        cfg.claude_bin,
        *_common_argv(
            instruction,
            output_format="stream-json",
            permission_mode=cfg.permission_mode,
            role=role,
            max_steps=max_steps,
            model=model or (cfg.model or None),
            verbose=True,
            continuation_ref=continuation_ref,
        ),
    ]

    try:
        with open(log_path, "wb") as log_file:
            proc = subprocess.Popen(  # noqa: S603 - the sanctioned subprocess boundary
                argv,
                cwd=repo,
                stdout=log_file,
                stderr=subprocess.PIPE,
                stdin=subprocess.DEVNULL,
                env=_subprocess_env(cfg),
                start_new_session=True,
            )
    except OSError as exc:
        raise BackgroundDispatchError(
            f"claude -p --output-format stream-json could not be started: {exc}"
        ) from exc

    # `start_new_session=True` detaches the child from this process's
    # session, but it is still OUR child for wait()/reaping purposes — left
    # un-reaped, a finished child sits as a zombie, and a zombie's pid still
    # answers `kill(pid, 0)` (is_pid_alive) as if it were live, which would
    # make the async poller's "the process is gone without a result" check
    # never fire. A daemon thread that blocks on proc.wait() reaps it the
    # moment it exits, without making spawn_background itself block.
    threading.Thread(
        target=proc.wait, name=f"claude-code-bridge-reap-{handle_id}", daemon=True
    ).start()

    return BackgroundStart(handle_id=handle_id, pid=proc.pid, log_path=str(log_path))


def is_pid_alive(pid: int) -> bool:
    """Signal 0, no kill — mirrors `colleague_cli.is_pid_alive`."""
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


def read_background_result(cfg: Config, handle_id: str) -> dict[str, Any] | None:
    """Read the detached child's stream-json log and return its terminal
    `result` record, if written yet."""
    path = flightfiles.feed_path(cfg.state_dir, handle_id)
    try:
        text = path.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return None
    return find_terminal_result(text.splitlines())


# Cancellation is cooperative SIGTERM only (see async_runner.py, which sends
# it once flightfiles.stop_requested() observes a written control file).
# This module never sends a signal to a claude process on its own initiative
# except run_sync's own SIGTERM-on-timeout above, which bounds a hung
# SYNCHRONOUS foreground call and is never escalated to SIGKILL.
