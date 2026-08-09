#!/usr/bin/env python3
"""A fake `claude` CLI, standing in for the real headless Claude Code binary
in tests (and, via `scripts/run_conformance_kit.sh`, in CI — the real
`claude` needs a live authenticated Anthropic backend and costs real money
per call, unlike colleague's own offline `COLLEAGUE_ENGINE=mock`; this
script is claude-code-bridge's equivalent of that mock engine).

Understands exactly the surface `claude_cli.py` drives:

* ``claude --version`` — prints ``$FAKE_CLAUDE_VERSION (Claude Code)`` (default
  "2.1.226") and exits 0. Lets version-gate tests pin an old/new version
  without a real binary.
* ``claude -p <prompt> --output-format json ...`` — prints ONE JSON object
  shaped like a real `type: "result"` message (see
  `claude_code_bridge/mapping.py`'s module docstring for the pinned shape)
  and exits.
* ``claude -p <prompt> --output-format stream-json ...`` — prints a handful
  of JSONL progress-shaped records, then the same terminal `result` object,
  one line at a time (with a small delay between them so a real tailing
  poller has something to observe), and exits.

Every response is controlled by env vars so a test can drive exactly the
scenario it wants without touching argv parsing:

* ``FAKE_CLAUDE_VERSION`` — the version `--version` reports (default "2.1.226").
* ``FAKE_CLAUDE_SUBTYPE`` — the terminal result's `subtype` (default "success").
* ``FAKE_CLAUDE_IS_ERROR`` — "1"/"0", the terminal result's `is_error` (default "0").
* ``FAKE_CLAUDE_RESULT_TEXT`` — the terminal result's `result` text.
* ``FAKE_CLAUDE_SESSION_ID`` — the terminal result's `session_id`.
* ``FAKE_CLAUDE_EXIT_CODE`` — the process exit code (default 0).
* ``FAKE_CLAUDE_CRASH`` — "1": write garbage (no parseable JSON) to stdout
  and exit non-zero instead of a normal result — simulates a crashed
  session that must never be read as success.
* ``FAKE_CLAUDE_HANG`` — "1": sleep far longer than any test timeout,
  ignoring SIGTERM's default handling only in the sense that this script
  does NOT install a custom handler — the interpreter's default SIGTERM
  disposition (terminate) still applies, so `run_sync`'s cooperative-stop
  test can observe the process actually exiting on SIGTERM. Set
  ``FAKE_CLAUDE_IGNORE_SIGTERM=1`` alongside this to additionally ignore
  SIGTERM and prove the bridge never escalates to SIGKILL on its own.
* ``FAKE_CLAUDE_STREAM_DELAY`` — seconds between stream-json lines (default 0.05).
"""

from __future__ import annotations

import json
import os
import signal
import sys
import time


def _bool_env(name: str, default: bool = False) -> bool:
    raw = os.environ.get(name)
    if raw is None:
        return default
    return raw.strip().lower() in ("1", "true", "yes", "on")


def _terminal_result() -> dict:
    return {
        "type": "result",
        "subtype": os.environ.get("FAKE_CLAUDE_SUBTYPE", "success"),
        "duration_ms": 42,
        "duration_api_ms": 40,
        "is_error": _bool_env("FAKE_CLAUDE_IS_ERROR", False),
        "num_turns": int(os.environ.get("FAKE_CLAUDE_NUM_TURNS", "1")),
        "session_id": os.environ.get("FAKE_CLAUDE_SESSION_ID", "fake-session-0000"),
        "stop_reason": os.environ.get("FAKE_CLAUDE_STOP_REASON") or None,
        "total_cost_usd": float(os.environ.get("FAKE_CLAUDE_COST_USD", "0.0007")),
        "usage": {
            "input_tokens": int(os.environ.get("FAKE_CLAUDE_INPUT_TOKENS", "11")),
            "output_tokens": int(os.environ.get("FAKE_CLAUDE_OUTPUT_TOKENS", "7")),
        },
        "result": os.environ.get("FAKE_CLAUDE_RESULT_TEXT", "did the thing (fake)"),
        "structured_output": None,
    }


def _handle_version() -> int:
    version = os.environ.get("FAKE_CLAUDE_VERSION", "2.1.226")
    print(f"{version} (Claude Code)")
    return 0


def _handle_print(argv: list[str]) -> int:
    if _bool_env("FAKE_CLAUDE_IGNORE_SIGTERM"):
        signal.signal(signal.SIGTERM, signal.SIG_IGN)

    if _bool_env("FAKE_CLAUDE_HANG"):
        time.sleep(3600)
        return 0

    if _bool_env("FAKE_CLAUDE_CRASH"):
        # Deliberately not JSON: a crashed/killed session that never wrote a
        # parseable result — this must never be read as success.
        sys.stdout.write("fake-claude: fatal engine error, no result produced\n")
        sys.stdout.flush()
        return int(os.environ.get("FAKE_CLAUDE_EXIT_CODE", "1"))

    stream = "--output-format" in argv and "stream-json" in argv
    delay = float(os.environ.get("FAKE_CLAUDE_STREAM_DELAY", "0.05"))

    if stream:
        session_id = os.environ.get("FAKE_CLAUDE_SESSION_ID", "fake-session-0000")
        for i, note in enumerate(("thinking", "using a tool", "writing output")):
            print(
                json.dumps(
                    {
                        "type": "system",
                        "subtype": "progress",
                        "session_id": session_id,
                        "note": note,
                        "step": i,
                    }
                )
            )
            sys.stdout.flush()
            time.sleep(delay)

    print(json.dumps(_terminal_result()))
    sys.stdout.flush()
    return int(os.environ.get("FAKE_CLAUDE_EXIT_CODE", "0"))


def main(argv: list[str]) -> int:
    if "--version" in argv or (len(argv) == 1 and argv[0] in ("-v", "--version")):
        return _handle_version()
    if "-p" in argv or "--print" in argv:
        return _handle_print(argv)
    # Anything else this fake does not understand: fail loudly rather than
    # silently pretending to succeed.
    sys.stderr.write(f"fake-claude: unrecognised invocation: {argv!r}\n")
    return 2


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
