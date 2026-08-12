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


def format_usage_lines(usage: dict[str, Any]) -> list[str]:
    """Render a §13.2 ``Usage`` rollup honestly, text-mode.

    ``input_tokens``/``output_tokens``/``attempts_reported``/
    ``attempts_not_reported`` are required fields on the API's ``Usage``
    schema — present (possibly genuinely zero) whenever a ``usage`` object
    is present at all, so they always render. ``cost``/``currency``/
    ``cost_by_currency`` are optional — reported only when at least one
    attempt in scope actually priced its work — so they render only when
    present, never synthesized as zero.

    Callers must only invoke this when the ``usage`` key itself is present
    in a payload (e.g. absent from ``GET /v1alpha1/runs`` list rows, which
    the API deliberately does not compute per-row) — this function has no
    way to distinguish "zero usage" from "no usage reported" on its own.
    """
    lines = [
        f"usage.input_tokens: {usage.get('input_tokens', 0)}",
        f"usage.output_tokens: {usage.get('output_tokens', 0)}",
    ]
    cost = usage.get("cost")
    if cost is not None:
        currency = usage.get("currency")
        lines.append(f"usage.cost: {cost} {currency}" if currency else f"usage.cost: {cost}")
    for entry in usage.get("cost_by_currency") or []:
        amount = entry.get("cost")
        currency = entry.get("currency")
        lines.append(
            f"usage.cost_by_currency: {amount} {currency}"
            if currency
            else f"usage.cost_by_currency: {amount}"
        )
    lines.append(f"usage.attempts_reported: {usage.get('attempts_reported', 0)}")
    lines.append(f"usage.attempts_not_reported: {usage.get('attempts_not_reported', 0)}")
    return lines


# Shared --json flag help text (S1192: one definition, many parsers).
JSON_FLAG_HELP = "Emit structured JSON."
