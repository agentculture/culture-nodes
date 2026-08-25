"""The parent-side dispatch API: `spawn` / `run_sync` for one ACP turn.

Plan task t2 of qwen-bridge-acp (module split; the dispatch lived in the
monolithic `qwen_cli` before). This is the boundary the t1-ported core
reads: `async_runner.py` calls `spawn` (the async path) and
`classifier.parse_session` on the streamed stdout; `server.py` calls
`run_sync` (the sync path) and reads the `SyncRunResult`.

The parent spawns ONE driver child per invocation - the bridge's own
module run as a command (`[sys.executable, -m, qwen_bridge.qwen_cli]`,
see `_driver_argv`): the child owns the qwen --acp process and the ACP
client side (driver._Driver), the parent owns the process lifetime and
the classification (the terminal event decides, never the exit code -
spec c4/h3, the codex crash-case rule).

The binary probe (probe.locate_qwen_bin) runs HERE, in the bridge
process, before any Popen (spec c19's boot refusal, plan t3's h5): a
missing qwen raises errors.QwenAgentMissingError with the distinct
message and NO invoke is served.
"""

from __future__ import annotations

import json
import re
import subprocess
import sys
import uuid
from dataclasses import dataclass
from typing import Any

from qwen_bridge.acp import classifier, errors, probe, wire
from qwen_bridge.config import Config


@dataclass
class SyncRunResult:
    """The outcome of one foreground ACP turn (the codex sibling's
    SyncRunResult role, extended with the seam's own facts):

    * exit_code / stdout / stderr / task_result / timed_out - the
      codex-shaped core the ported server/async_runner read
      (task_result is classifier.parse_session's classification of
      stdout; timed_out is the bridge's own deadline, not the turn's
      state).
    * seam_facts      - the SeamFacts dict the capability surface (t3)
      consumes by value.
    * transcript_path - the local transcript file retaining the FULL
      both-directions stream (c21's debug retention).
    * refusal         - the distinct refusal message when the driver
      refused before serving (a QwenSeamRefusal is ALSO raised, so the
      server's 503 mapping fires; this field is for introspection).
    """

    exit_code: int | None
    stdout: str
    stderr: str
    task_result: dict[str, Any] | None
    timed_out: bool
    seam_facts: dict[str, Any] | None = None
    transcript_path: str | None = None
    refusal: str | None = None


_REFUSAL_LINE_RE = re.compile(rf"^{re.escape(wire.REFUSAL_MARKER)}\s*(.+)$", re.M)
_TRANSCRIPT_LINE_RE = re.compile(rf"^{re.escape(wire.TRANSCRIPT_MARKER)}\s*(\S+)$", re.M)


def _driver_argv(
    qwen_bin: str,
    instruction: str,
    repo: str,
    *,
    model: str | None,
    sandbox: str | None,
    mode: str | None,
    state_dir: str,
    run_id: str,
    qwen_env: dict[str, str],
) -> list[str]:
    """The exact driver-child argv (the ONE place it is assembled, so a
    test can assert it without spawning anything - the codex sibling's
    _common_argv discipline). The child is the bridge's own module run
    as a command: `[sys.executable, -m, qwen_bridge.qwen_cli]` - no
    separate script file to ship, and the stdlib-only constraint holds
    (the driver imports only this package and the config for types)."""
    argv = [
        sys.executable,
        "-m",
        "qwen_bridge.qwen_cli",
        "--qwen-bin",
        qwen_bin,
        "--cwd",
        repo,
        "--instruction",
        instruction,
        "--mode",
        mode or "",
        "--state-dir",
        state_dir,
        "--run-id",
        run_id,
    ]
    if model:
        argv += ["--model", model]
    if sandbox:
        argv += ["--sandbox", sandbox]
    if qwen_env:
        argv += ["--qwen-env", json.dumps(qwen_env)]
    return argv


def spawn(
    cfg: Config,
    instruction: str,
    repo: str,
    *,
    model: str | None = None,
    sandbox: str | None = None,
    continuation_ref: str | None = None,
    writable_git: bool = False,
    mode: str | None = None,
) -> subprocess.Popen:
    """Start ONE qwen --acp session in the background (the async path's
    entry) and return the live driver-child Popen immediately.

    The binary probe runs HERE, in the bridge process, before any Popen
    (spec c19's boot refusal, plan t3's h5): a missing qwen raises
    QwenAgentMissingError with the distinct message and NO invoke is
    served.

    *continuation_ref* is accepted for the t1-ported core's signature
    compatibility and is NOT implemented in the first cut (frame park
    v2 / the plan risk register): the dispatch cold-starts, and the
    cross-process session/load behavior belongs to the named h14 probe
    alone. This is documented, not silent - a resume a node expects is
    cold-started by design until park v2 lands, and the plan keeps that
    work on the sibling bridges meanwhile.

    *mode* is the input/preflight policy's ACP session mode (h15): the
    driver resolves it against the agent's measured availableModes at
    session creation and fails closed when it is missing or unoffered -
    the bridge never serves a session in the agent's measured default.
    """
    del (
        continuation_ref,
        writable_git,
    )  # first cut: cold start, no .git widening (see module docstring)
    qwen_bin = probe.locate_qwen_bin(cfg)
    argv = _driver_argv(
        qwen_bin,
        instruction,
        repo,
        model=model,
        sandbox=sandbox,
        mode=mode,
        state_dir=cfg.state_dir,
        run_id=uuid.uuid4().hex,
        qwen_env=cfg.qwen_env,
    )
    try:
        return subprocess.Popen(  # noqa: S603 - the sanctioned subprocess boundary
            argv,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            stdin=subprocess.DEVNULL,
            text=True,
            bufsize=1,
        )
    except OSError as exc:
        raise errors.SpawnError(f"could not start the qwen ACP driver child: {exc}") from exc


def run_sync(
    cfg: Config,
    instruction: str,
    repo: str,
    *,
    model: str | None = None,
    sandbox: str | None = None,
    continuation_ref: str | None = None,
    writable_git: bool = False,
    mode: str | None = None,
) -> SyncRunResult:
    """Run one ACP turn in the foreground and wait for it to finish.

    On the bridge's own timeout the driver child is sent SIGTERM - never
    SIGKILL, mirroring the codex sibling's cooperative-stop stance.
    The driver's own SIGTERM handler then attempts the MEASURED
    cooperative stop (the session/cancel notification) and lets the
    cancelled terminal reach the transcript when the agent answers it -
    which is why a timed-out-but-cancelled turn classifies as the
    cancellation outcome, and a timed-out turn whose agent died anyway
    classifies as incomplete: the terminal event decides, never the
    exit code.

    A driver refusal (exit REFUSAL_EXIT_CODE + the stderr marker -
    handshake or mode policy, spec c19/h15/h16) raises QwenSeamRefusal
    with the distinct message: the serve was refused BEFORE it was
    served, so nothing is reported as a turn result.
    """
    proc = spawn(
        cfg,
        instruction,
        repo,
        model=model,
        sandbox=sandbox,
        continuation_ref=continuation_ref,
        writable_git=writable_git,
        mode=mode,
    )
    try:
        stdout, stderr = proc.communicate(timeout=cfg.sync_timeout_seconds)
        timed_out = False
    except subprocess.TimeoutExpired:
        proc.terminate()  # SIGTERM: cooperative, never SIGKILL
        try:
            stdout, stderr = proc.communicate(timeout=max(cfg.sync_timeout_seconds * 0.2, 5.0))
        except subprocess.TimeoutExpired:
            # Still not done. Never SIGKILL - leave it running and report
            # the timeout honestly, the codex sibling's own stance.
            stdout, stderr = (
                "",
                "the qwen ACP driver did not exit after SIGTERM within the grace period",
            )
        timed_out = True

    refusal: str | None = None
    marker = _REFUSAL_LINE_RE.search(stderr or "")
    if proc.returncode == wire.REFUSAL_EXIT_CODE and marker:
        refusal = marker.group(1).strip()
        raise errors.QwenSeamRefusal(refusal)
    if proc.returncode == wire.REFUSAL_EXIT_CODE and not marker:
        # the driver signalled a refusal it did not write: an honest
        # refusal, not a turn result
        raise errors.QwenSeamRefusal(
            "qwen-acp-refusal: the driver refused before serving (no marker line - driver fault)"
        )

    task_result = classifier.parse_session(stdout)
    transcript_match = _TRANSCRIPT_LINE_RE.search(stderr or "")
    return SyncRunResult(
        exit_code=proc.returncode,
        stdout=stdout,
        stderr=stderr,
        task_result=task_result,
        timed_out=timed_out,
        seam_facts=task_result.get("seam_facts") if task_result else None,
        transcript_path=transcript_match.group(1) if transcript_match else None,
        refusal=refusal,
    )


def git_writable_override(repo: str) -> str:
    """The handover opt-in's `.git` widening, for the t1-ported core's
    API compatibility (capabilities.py names it; the driver does not
    consume it in the first cut).

    The HONEST first-cut answer: the qwen ACP seam has no mechanism for
    it. Codex's widening is a bwrap `sandbox_workspace_write` carve-out
    on a --sandbox flag the ACP seam does not take - the ACP session
    runs with the authority its resolved mode grants, on a host the
    image (t6) and the operator's sandboxing confine. Returning the
    empty string (no override) rather than inventing a flag is the
    fail-closed behavior: a handover dispatch on a mode that cannot
    write .git fails visibly in-session instead of silently getting a
    widening nobody measured. The plan's handover carve for this backend
    rides on park v3's mode decision (t5/t7 document it).
    """
    del repo
    return ""
