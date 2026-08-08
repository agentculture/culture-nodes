"""stdout / stderr helpers with a strict split (stable-contract).

Rule: **results go to stdout, diagnostics and errors go to stderr.** Agents
parsing output can rely on this invariant. JSON mode routes structured
payloads to the same streams — never mixes them.
"""

from __future__ import annotations

import json
import sys
from typing import Any, TextIO

from culture_nodes.cli._errors import CliError


def emit_result(data: Any, *, json_mode: bool, stream: TextIO | None = None) -> None:
    """Write a command result to stdout (or ``stream``)."""
    s = stream if stream is not None else sys.stdout
    if json_mode:
        json.dump(data, s, ensure_ascii=False)
        s.write("\n")
        return
    text = data if isinstance(data, str) else str(data)
    s.write(text)
    if not text.endswith("\n"):
        s.write("\n")


def emit_error(err: CliError, *, json_mode: bool, stream: TextIO | None = None) -> None:
    """Write a :class:`CliError` to stderr.

    Text mode renders as two lines when a remediation is present::

        error: <message>
        hint: <remediation>

    The ``hint:`` prefix is required by the agent-first error rubric.
    """
    s = stream if stream is not None else sys.stderr
    if json_mode:
        json.dump(err.to_dict(), s, ensure_ascii=False)
        s.write("\n")
        return
    s.write(f"error: {err.message}\n")
    if err.remediation:
        s.write(f"hint: {err.remediation}\n")


def emit_diagnostic(message: str, *, stream: TextIO | None = None) -> None:
    """Write a human diagnostic (progress, summary) to stderr."""
    s = stream if stream is not None else sys.stderr
    s.write(message if message.endswith("\n") else message + "\n")


def emit_json_passthrough(raw: bytes, *, stream: TextIO | None = None) -> None:
    """Write a raw API JSON response byte-exact to stdout (or ``stream``).

    Product-verb commands (``workflow``, ``run``, ``ledger``, ``review``) use
    this in ``--json`` mode instead of :func:`emit_result`: the payload
    already arrived from the nodes API as JSON bytes, so re-parsing and
    re-encoding it through ``json.dump`` would risk reformatting (Python's
    default separators insert spaces the API's compact encoder does not
    use). Passing the original bytes straight through guarantees the CLI's
    ``--json`` output is byte-exact with what the API returned.
    """
    s = stream if stream is not None else sys.stdout
    text = raw.decode("utf-8") if raw else "null"
    s.write(text)
    if not text.endswith("\n"):
        s.write("\n")
