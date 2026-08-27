"""The driver child: one qwen --acp process, driven as the ACP client.

Plan task t2 of qwen-bridge-acp (module split; the driver lived in the
monolithic `qwen_cli` before). Invoked as
`python -m qwen_bridge.qwen_cli` - the facade module's __main__ block
dispatches here - with one turn's worth of argv (see
dispatch._driver_argv for the exact line). One qwen --acp process per
invocation (spec c17's process model).

The turn sequence, and where each refusal lives:

1. `initialize` (id 1) - the handshake gate (gate.validate_initialize,
   spec c19/h16) runs BEFORE serving: an unsupported protocolVersion or
   an agentInfo mismatch refuses with a distinct message.
2. `session/new` (id 2) - the sessionId + the agent's measured
   availableModes.
3. `session/set_mode` (id 3) - the mode policy (gate.resolve_acp_mode,
   spec c18/h15): set from the input/preflight policy, NEVER the
   measured default.
4. `session/prompt` (id 4) - the turn itself: the measured stream shapes
   (session/update notifications, the lenient ext answers) are echoed to
   stdout as they arrive; the session/prompt response - result
   {stopReason, _meta} or a JSON-RPC error - ends the turn. EOF without
   a terminal ends it too (the agent died: the classifier reports
   incomplete, never ok).

The agent-to-client JSON-RPC stream is ECHOED to stdout (the transcript
the classifier and the async runner read, one message per line) and
EVERYTHING (both directions) is retained in the local transcript file
(c21). Refusals ride STDERR as wire.REFUSAL_MARKER lines with
wire.REFUSAL_EXIT_CODE - stdout is the transcript stream the
classifier parses, so a refusal line on it would poison the
classification.

On SIGTERM the driver attempts the MEASURED cooperative stop - the
session/cancel NOTIFICATION (no id, ACP v1) - and lets the cancelled
terminal (when the agent answers it, as measured) reach the transcript
before it exits 0; a watchdog bounds the drain so a hung agent cannot
hold the window open forever. The cancelled terminal classifies as the
cancellation outcome; a death without a terminal classifies incomplete
- the terminal event decides, never the exit code.
"""

from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import threading
from pathlib import Path
from typing import Any, Sequence

from qwen_bridge.acp import errors, gate, wire
from qwen_bridge.acp.transport import AgentLink


class _Driver:
    """One qwen --acp process, driven as the ACP client, with the
    agent-to-client JSON-RPC stream echoed to stdout (the transcript
    the classifier and the async runner read) and EVERYTHING (both
    directions) retained in the local transcript file (c21)."""

    def __init__(
        self,
        *,
        qwen_bin: str,
        cwd: str,
        instruction: str,
        mode: str | None,
        model: str | None,
        sandbox: str | None,
        state_dir: str,
        run_id: str,
        qwen_env: dict[str, str] | None,
    ) -> None:
        self._qwen_bin = qwen_bin
        self._cwd = cwd
        self._instruction = instruction
        self._mode = mode
        # dispatch context only: the ACP seam takes no --sandbox flag (the
        # session mode is the approval-policy surface, spec c18) and the
        # first cut does not forward input.model onto the wire either
        # (the host's own settings.json model is measured in the
        # session/new response) - the fields ride for argv-shape
        # compatibility, mirroring the codex sibling's own stance of
        # carrying what the parent sends.
        self._model = model
        self._sandbox = sandbox
        self._run_id = run_id
        self._qwen_env = qwen_env or {}
        self._transcript_file = Path(state_dir) / "acp-transcripts" / f"{run_id}.jsonl"
        self._link: AgentLink | None = None
        self._cancelled = False  # True once SIGTERM asked for cooperative cancel
        self._session_id: str | None = None
        self._proc: subprocess.Popen | None = None

    # -- plumbing ------------------------------------------------------------

    def _refuse(self, detail: str) -> None:
        # the refusal marker rides STDERR (never stdout): stdout is the
        # transcript stream the classifier parses
        sys.stderr.write(f"{wire.REFUSAL_MARKER} {detail}\n")
        sys.stderr.flush()

    def _on_sigterm(self, signum: int, frame: Any) -> None:
        # PEP 475 would otherwise retry the interrupted readline; raising
        # hands the decision to the read loop, which knows how far the
        # turn got.
        raise errors.DriverTerminated

    def _cooperative_cancel(self) -> None:
        """SIGTERM arrived: attempt the MEASURED cooperative stop - the
        session/cancel NOTIFICATION (no id, ACP v1) - and let the
        cancelled terminal (when the agent answers it, as measured)
        reach the transcript before the driver exits 0. A watchdog
        bounds the drain so a hung agent cannot hold the bridge's own
        timeout grace open forever."""
        if self._session_id is not None and not self._cancelled:
            self._cancelled = True
            assert self._link is not None
            self._link.send(
                {
                    "jsonrpc": "2.0",
                    "method": wire.METHOD_SESSION_CANCEL,
                    "params": {"sessionId": self._session_id},
                }
            )
            watchdog = threading.Timer(wire.CANCEL_DRAIN_SECONDS, self._force_stop)
            watchdog.daemon = True
            watchdog.start()

    def _force_stop(self) -> None:
        if self._proc is not None and self._proc.poll() is None:
            self._proc.terminate()
        # os._exit on purpose: a hung agent is holding the pipe open; the
        # turn's already-captured stream (the cancelled terminal, when it
        # arrived) is what the parent classifies, and exit 0 keeps the
        # parent's own timeout bookkeeping honest.
        os._exit(0)

    def _drain_after_cancel(self) -> None:
        assert self._link is not None
        self._link.drain(4)

    # -- the turn --------------------------------------------------------------

    def run(self) -> int:
        signal.signal(signal.SIGTERM, self._on_sigterm)
        env = dict(os.environ)
        env.update(self._qwen_env)
        try:
            self._proc = subprocess.Popen(  # noqa: S603 - the sanctioned boundary
                [self._qwen_bin, "--acp"],
                cwd=self._cwd,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                text=True,
                bufsize=1,
                env=env,
            )
        except OSError as exc:
            self._refuse(f"qwen-agent-missing: could not start {self._qwen_bin!r}: {exc}")
            return wire.REFUSAL_EXIT_CODE

        self._link = AgentLink(self._proc, transcript_path=self._transcript_file)
        sys.stderr.write(f"{wire.TRANSCRIPT_MARKER} {self._transcript_file}\n")
        sys.stderr.flush()

        try:
            try:
                return self._drive()
            except errors.DriverTerminated:
                # SIGTERM landed at a read site (the handler raises): the
                # cooperative-cancel path is the only sane reaction to it,
                # wherever in the turn it surfaces.
                self._cooperative_cancel()
                self._drain_after_cancel()
                return 0
            except errors.AcpPolicyError as exc:
                # a pre-serve refusal (handshake gate or mode policy,
                # spec c19/h15/h16) at any stage of _drive
                self._refuse(str(exc))
                return wire.REFUSAL_EXIT_CODE
        finally:
            if self._proc.poll() is None:
                self._proc.terminate()
                try:
                    self._proc.wait(timeout=2.0)
                except subprocess.TimeoutExpired:
                    pass

    def _drive(self) -> int:
        assert self._link is not None
        # 1. initialize (id 1) - the handshake gate runs BEFORE serving.
        self._link.send(
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": wire.METHOD_INITIALIZE,
                "params": {
                    "protocolVersion": 1,
                    # the v1 client obligations, stated on the wire: the
                    # first cut implements no fs/terminal handlers
                    "clientCapabilities": {
                        "fs": {"readTextFile": False, "writeTextFile": False},
                        "terminal": False,
                    },
                },
            }
        )
        init_response = self._link.wait_response(1)
        if init_response is None:
            return 0  # the agent died before the handshake: no transcript
        if "error" in init_response:
            self._refuse(
                f"the agent answered initialize with a JSON-RPC error: {init_response['error']}"
            )
            return wire.REFUSAL_EXIT_CODE
        gate.validate_initialize(init_response.get("result") or {})

        # 2. session/new (id 2) - sessionId + the measured modes.
        self._link.send(
            {
                "jsonrpc": "2.0",
                "id": 2,
                "method": wire.METHOD_SESSION_NEW,
                "params": {"cwd": self._cwd, "mcpServers": []},
            }
        )
        new_response = self._link.wait_response(2)
        if new_response is None:
            return 0
        if "error" in new_response:
            self._refuse(f"the agent refused session/new: {new_response['error']}")
            return wire.REFUSAL_EXIT_CODE
        new_result = new_response.get("result") or {}
        session_id = new_result.get("sessionId")
        if not isinstance(session_id, str) or not session_id:
            self._refuse("session/new returned no sessionId — refusing to serve")
            return wire.REFUSAL_EXIT_CODE
        self._session_id = session_id
        modes = new_result.get("modes") or {}
        available_modes = [
            entry.get("id")
            for entry in (modes.get("availableModes") or [])
            if isinstance(entry, dict)
        ]

        # 3. the mode policy (h15): set from the input/preflight
        # policy, never the measured default.
        chosen_mode = gate.resolve_acp_mode(self._mode, available_modes)
        self._link.send(
            {
                "jsonrpc": "2.0",
                "id": 3,
                "method": wire.METHOD_SESSION_SET_MODE,
                "params": {"sessionId": session_id, "modeId": chosen_mode},
            }
        )
        set_mode_response = self._link.wait_response(3)
        if set_mode_response is None:
            return 0
        if "error" in set_mode_response:
            error_detail = set_mode_response["error"]
            self._refuse(f"the agent refused session/set_mode ({chosen_mode!r}): {error_detail}")
            return wire.REFUSAL_EXIT_CODE

        # 4. session/prompt (id 4) - the turn itself.
        self._link.send(
            {
                "jsonrpc": "2.0",
                "id": 4,
                "method": wire.METHOD_SESSION_PROMPT,
                "params": {
                    "sessionId": self._session_id,
                    "prompt": [{"type": "text", "text": self._instruction}],
                },
            }
        )
        # read the turn to its terminal (wait_response(4)): the measured
        # stream shapes are echoed to stdout as they arrive; EOF without
        # a terminal means the agent died mid-turn - the classifier
        # reports incomplete, never ok.
        self._link.wait_response(4)
        return 0


# ---------------------------------------------------------------------------
# the driver entry point (python -m qwen_bridge.qwen_cli)
# ---------------------------------------------------------------------------


def _driver_main(argv: Sequence[str]) -> int:
    """Parse the driver child's argv (see dispatch._driver_argv for the
    exact line) and run one turn. A malformed argv is a pre-serve
    refusal, not a turn result - the marker line rides stderr and the
    exit code is REFUSAL_EXIT_CODE."""
    options: dict[str, str] = {}
    i = 0
    while i < len(argv):
        arg = argv[i]
        if arg in (
            "--qwen-bin",
            "--cwd",
            "--instruction",
            "--mode",
            "--model",
            "--sandbox",
            "--state-dir",
            "--run-id",
            "--qwen-env",
        ):
            if i + 1 >= len(argv):
                sys.stderr.write(f"{wire.REFUSAL_MARKER} driver argv: missing value for {arg}\n")
                sys.stderr.flush()
                return wire.REFUSAL_EXIT_CODE
            options[arg.lstrip("-")] = argv[i + 1]
            i += 2
        else:
            sys.stderr.write(f"{wire.REFUSAL_MARKER} driver argv: unrecognised argument {arg!r}\n")
            sys.stderr.flush()
            return wire.REFUSAL_EXIT_CODE
    required = ("qwen-bin", "cwd", "instruction", "state-dir", "run-id")
    for key in required:
        if not options.get(key):
            sys.stderr.write(f"{wire.REFUSAL_MARKER} driver argv: missing --{key}\n")
            sys.stderr.flush()
            return wire.REFUSAL_EXIT_CODE
    qwen_env: dict[str, str] | None = None
    if options.get("qwen-env"):
        try:
            raw = json.loads(options["qwen-env"])
            if isinstance(raw, dict):
                qwen_env = {str(k): str(v) for k, v in raw.items()}
        except ValueError:
            sys.stderr.write(
                f"{wire.REFUSAL_MARKER} driver argv: --qwen-env is not a JSON object\n"
            )
            sys.stderr.flush()
            return wire.REFUSAL_EXIT_CODE
    driver = _Driver(
        qwen_bin=options["qwen-bin"],
        cwd=options["cwd"],
        instruction=options["instruction"],
        mode=options.get("mode") or None,
        model=options.get("model") or None,
        sandbox=options.get("sandbox") or None,
        state_dir=options["state-dir"],
        run_id=options["run-id"],
        qwen_env=qwen_env,
    )
    return driver.run()
