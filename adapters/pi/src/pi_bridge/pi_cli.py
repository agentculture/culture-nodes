"""Subprocess seam for pi's headless JSON print mode.

Only documented flags are emitted.  A turn is successful only when an
``agent_end`` event is present; process exit status alone is never success.

``run_sync`` follows the same never-SIGKILL stance as the sibling adapters'
sync runners (``claude_cli.py`` and ``codex_cli.py``): a child that ignores
SIGTERM past the grace period is left running and the timeout is reported
honestly, never force-killed.
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
    # The node contract's `completed` outcome and the proposed `claim` record
    # both carry a `summary` that must hold the agent's actual answer — pi
    # puts the answer in `result` (a list of text/thinking content blocks or a
    # bare string), so flatten the *visible* text into `summary`. Without this
    # the summary is empty, the claim statement is empty, and a clean pi run is
    # refused `contract_rejected` (#299). Thinking blocks are excluded — the
    # summary is the answer, not the reasoning.
    result["summary"] = _visible_text(result.get("result"))
    return result


def _visible_text(content: Any) -> str:
    """Flatten pi message content to the assistant's visible answer text.

    `content` is either a bare string or a list of content blocks
    (`{"type": "text"|"thinking", ...}`); only `text` blocks contribute, so a
    turn that ends with reasoning plus an answer yields just the answer.
    """
    if isinstance(content, str):
        return content.strip()
    if isinstance(content, list):
        parts = [
            str(block.get("text", ""))
            for block in content
            if isinstance(block, dict) and block.get("type") == "text"
        ]
        return "\n".join(p for p in parts if p).strip()
    return ""


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
        try:
            stdout, stderr = proc.communicate(timeout=max(cfg.sync_timeout_seconds * 0.2, 5.0))
        except subprocess.TimeoutExpired:
            # Still not done. Never SIGKILL — leave it running and report the
            # timeout honestly; an operator can inspect/finish reaping later.
            stdout, stderr = "", "pi did not exit after SIGTERM within the grace period"
    path = transcript_path(cfg, uuid.uuid4().hex)
    path.write_text(stdout, encoding="utf-8")
    return SyncRunResult(proc.returncode, stdout, stderr, parse_session(stdout), timed_out)


def refusal_detail(stderr: str) -> None:
    del stderr
    return None
