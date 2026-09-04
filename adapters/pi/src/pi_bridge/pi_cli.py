"""Subprocess seam for pi's headless JSON print mode.

Only documented flags are emitted.  A turn is successful only when an
``agent_end`` event is present; process exit status alone is never success.
"""

from __future__ import annotations

import json
import os
import signal
import subprocess
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from pi_bridge.config import Config

SANDBOX_MODES = frozenset({"read-only", "workspace-write"})
SANDBOX_WORKSPACE_WRITE = "workspace-write"
ACP_MODES: frozenset[str] = frozenset()
REFUSAL_EXIT_CODE = 64


class SpawnError(RuntimeError):
    pass


class PiAgentMissingError(SpawnError):
    pass


@dataclass
class SyncRunResult:
    exit_code: int | None
    stdout: str
    stderr: str
    task_result: dict[str, Any] | None
    timed_out: bool


def _env(cfg: Config) -> dict[str, str]:
    env = dict(os.environ)
    env.update(cfg.pi_env)
    return env


def build_argv(cfg: Config, instruction: str, *, model: str | None = None) -> list[str]:
    argv = [cfg.pi_bin, "--mode", "json", "-p", instruction, "--no-session", "-a"]
    if cfg.provider:
        argv += ["--provider", cfg.provider]
    selected_model = model or cfg.model
    if selected_model:
        argv += ["--model", selected_model]
    return argv


def transcript_path(cfg: Config, invocation_id: str) -> Path:
    path = cfg.state_path / "pi-transcripts" / f"{invocation_id}.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    return path


def parse_session(stdout: str) -> dict[str, Any] | None:
    events: list[dict[str, Any]] = []
    for line in stdout.splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if isinstance(event, dict):
            events.append(event)
    if not events:
        return None

    message_end: dict[str, Any] | None = None
    agent_end: dict[str, Any] | None = None
    for event in events:
        if event.get("type") == "message_end":
            message_end = event
        elif event.get("type") == "agent_end":
            agent_end = event

    result: dict[str, Any] = {"status": "incomplete", "termination_reason": "incomplete"}
    if message_end:
        message = message_end.get("message") or {}
        usage = message.get("usage") or message_end.get("usage") or {}
        result["usage"] = {
            "input_tokens": usage.get("input", usage.get("input_tokens")),
            "output_tokens": usage.get("output", usage.get("output_tokens")),
        }
        result["model"] = message.get("model") or message_end.get("model")
        result["result"] = message.get("content") or message.get("text") or ""
    if agent_end is not None:
        result["status"] = "ok"
        result["termination_reason"] = "agent_end"
        result["messages"] = agent_end.get("messages", [])
        if result.get("result") == "" and result["messages"]:
            final = result["messages"][-1]
            result["result"] = final.get("content") or final.get("text") or ""
    return result


def spawn(
    cfg: Config,
    instruction: str,
    repo: str,
    *,
    model: str | None = None,
    sandbox: str | None = None,
    mode: str | None = None,
    continuation_ref: str | None = None,
    writable_git: bool = False,
) -> subprocess.Popen:
    del sandbox, mode, continuation_ref, writable_git
    try:
        return subprocess.Popen(
            build_argv(cfg, instruction, model=model),
            cwd=repo,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            env=_env(cfg),
            start_new_session=True,
        )
    except OSError as exc:
        raise SpawnError(f"could not start pi binary {cfg.pi_bin!r}: {exc}") from exc


def terminate_group(proc: subprocess.Popen) -> None:
    if proc.poll() is None:
        try:
            os.killpg(proc.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass


def run_sync(
    cfg: Config,
    instruction: str,
    repo: str,
    *,
    model: str | None = None,
    sandbox: str | None = None,
    mode: str | None = None,
    continuation_ref: str | None = None,
    writable_git: bool = False,
) -> SyncRunResult:
    proc = spawn(
        cfg,
        instruction,
        repo,
        model=model,
        sandbox=sandbox,
        mode=mode,
        continuation_ref=continuation_ref,
        writable_git=writable_git,
    )
    timed_out = False
    try:
        stdout, stderr = proc.communicate(timeout=cfg.sync_timeout_seconds)
    except subprocess.TimeoutExpired:
        timed_out = True
        terminate_group(proc)
        stdout, stderr = proc.communicate(timeout=5)
    path = transcript_path(cfg, uuid.uuid4().hex)
    path.write_text(stdout, encoding="utf-8")
    return SyncRunResult(proc.returncode, stdout, stderr, parse_session(stdout), timed_out)


def refusal_detail(stderr: str) -> None:
    del stderr
    return None
