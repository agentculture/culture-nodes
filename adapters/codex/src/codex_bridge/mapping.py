"""TaskResult -> actor-protocol mapping (PRD §13.2/§13.4, codex session
classification). Mirrors `colleague_bridge/mapping.py` near-verbatim — same
wire shapes, same ok/error/incomplete vocabulary, same rule. Only
`codex_cli.parse_session` (not this module) differs in how that
three-way status gets derived from the underlying agent backend.

This module is the one place the bridge decides what a codex session
(`codex_cli.parse_session`'s TaskResult-shaped dict) means as an actor
answer. It never touches a socket or a subprocess — every function here is
a pure translation from a parsed dict (or the absence of one) to the wire
shapes `internal/actors/protocol.go` defines, so the whole mapping table is
unit-testable without ever invoking `codex`.

The one rule every other decision in this file serves: **incomplete is
never success.** `status == "incomplete"` means codex's session ended
without ever reaching a terminal `turn.completed`/`turn.failed` event — no
terminal turn event was ever seen, whether because this bridge's own
timeout fired, the process crashed, or it was killed some other way (see
`codex_cli.py`'s module docstring for the grounding evidence, including the
measured case of a SIGTERM'd session that exited 0 cleanly with no terminal
event). This module reports it as the node's own declared `incomplete`
domain outcome ONLY when the invocation explicitly named one; otherwise it
is reported as an execution failure. It is never silently folded into
`"completed"` — no adapter-specific exemption exists for this rule.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

#: The session-classification vocabulary `codex_cli.parse_session` produces
#: (mirrors colleague contract v1's `TaskResult.status` values).
STATUS_OK = "ok"
STATUS_ERROR = "error"
STATUS_INCOMPLETE = "incomplete"

#: §13.5 error classes this module ever produces (a strict subset of
#: internal/actors.ErrorClass — the bridge only ever originates the classes
#: an actor is positioned to know about).
CLASS_EXECUTION = "execution"
CLASS_TIMEOUT = "timeout"
CLASS_ACTOR_REJECTED_INPUT = "actor_rejected_input"


@dataclass(frozen=True)
class InvocationContext:
    """The parts of a §13.1 InvocationRequest the mapping layer needs.

    A narrow view rather than the whole request dict: this module must never
    reach for a field it wasn't handed, so a caller can construct one from
    whatever transport it used (real HTTP request, a unit test literal)
    without carrying the rest of the envelope along.
    """

    run_id: str = ""
    node_run_id: str | None = None
    attempt_id: str | None = None
    #: The domain outcome to report for `status: ok`, when the invocation's
    #: own input declared one (`input.success_outcome`). `None` defers to
    #: `Config.default_success_outcome`.
    success_outcome: str | None = None
    #: The domain outcome to report for `status: incomplete`, when the
    #: invocation's input declared the node accepts that outcome
    #: (`input.incomplete_outcome`, a non-empty string). `None` (the
    #: default) means the node's contract does not declare it, so an
    #: incomplete or crashed run is reported as an execution failure
    #: instead — never as a silent success.
    incomplete_outcome: str | None = None


@dataclass(frozen=True)
class Classification:
    """What one TaskResult (or its absence) means as a protocol answer."""

    #: True for a domain outcome (§13.2 `outcome`, §13.4 `completed`); False
    #: for a technical execution failure (§13.5 `execution`/`timeout`).
    domain: bool
    #: The domain outcome name, set iff `domain` is True.
    outcome: str | None = None
    #: A human-readable failure message, set iff `domain` is False.
    message: str | None = None
    #: The §13.5 error class, set iff `domain` is False.
    error_class: str | None = None


def classify(
    task_result: dict[str, Any] | None, ctx: InvocationContext, *, default_success_outcome: str
) -> Classification:
    """Decide what *task_result* means, independent of sync vs async.

    *task_result* is `None` when the codex subprocess produced no
    parseable output at all (a crash before any JSONL line was ever
    written); every other case is what `codex_cli.parse_session` derived
    from the transcript.
    """
    if task_result is None:
        return Classification(
            domain=False,
            message="codex produced no parseable result",
            error_class=CLASS_EXECUTION,
        )

    status = task_result.get("status")
    if status == STATUS_OK:
        outcome = ctx.success_outcome or default_success_outcome
        return Classification(domain=True, outcome=outcome)

    if status == STATUS_INCOMPLETE:
        if ctx.incomplete_outcome:
            return Classification(domain=True, outcome=ctx.incomplete_outcome)
        return Classification(
            domain=False,
            message=(
                "codex's session ended without reaching a terminal turn event "
                "(killed, crashed, or timed out); the invocation declared no "
                "incomplete domain outcome, so this is reported as an execution "
                "failure rather than success — incomplete is never success"
            ),
            error_class=CLASS_EXECUTION,
        )

    if status == STATUS_ERROR:
        return Classification(
            domain=False,
            message=task_result.get("error") or "codex reported a turn failure",
            error_class=CLASS_EXECUTION,
        )

    return Classification(
        domain=False,
        message=f"codex produced an unrecognised status {status!r}",
        error_class=CLASS_EXECUTION,
    )


def usage_from_task_result(task_result: dict[str, Any] | None) -> dict[str, Any]:
    """Map codex's own `usage` (input/output tokens) onto §13.2 `Usage`.

    codex's usage is exact, API-reported token accounting (the
    `turn.completed` event's own `usage` field); the bridge never estimates
    cost, so `cost`/`currency` are always null — an actor that does not
    price its work says so with null rather than a zero that reads as
    "free" (protocol.go's own docstring, matching colleague-bridge's own
    stance).
    """
    usage = (task_result or {}).get("usage") or {}
    return {
        "input_tokens": int(usage.get("input_tokens") or 0),
        "output_tokens": int(usage.get("output_tokens") or 0),
        "cost": None,
        "currency": None,
    }


def output_from_task_result(task_result: dict[str, Any] | None) -> dict[str, Any]:
    """Map the fields the task names: `{summary, changed_files,
    artifacts_path}`. codex has no artifacts-directory convention of its
    own (unlike colleague's `.colleague/<task_id>.json`), so
    `artifacts_path` is always null here — an honest absence, not an
    invented path.
    """
    tr = task_result or {}
    return {
        "summary": tr.get("summary") or "",
        "changed_files": list(tr.get("changed_files") or []),
        "artifacts_path": None,
    }


def claim_record(
    task_result: dict[str, Any] | None,
    ctx: InvocationContext,
    *,
    actor_id: str,
    created_at: str,
) -> dict[str, Any]:
    """Build the ONE proposed ledger record the bridge ever emits.

    A `claim` record, `authority: "proposed"`, `origin.kind: "agent"` —
    codex's own final message is a completion CLAIM, not verified evidence
    (PRD §10.5); the bridge never emits `confirmed`/`observed` (no actor
    promotes its own proposal — PRD ledger authority model). Only the
    envelope fields a *proposer* is expected to set are populated (PRD
    §10.7's own worked example carries exactly `record_type`, `authority`,
    `subject_ref`, `data`); `id`/`content_digest` are left for the ledger
    runtime to assign at append.
    """
    tr = task_result or {}
    return {
        "id": "",
        "schema_version": "nodes.culture.dev/ledger/v1alpha1",
        "record_type": "claim",
        "run_id": ctx.run_id,
        "node_run_id": ctx.node_run_id,
        "attempt_id": ctx.attempt_id,
        "origin": {"kind": "agent", "actor_id": actor_id},
        "authority": "proposed",
        "subject_ref": None,
        "data": {
            "statement": tr.get("summary") or "",
            "kind": "completion-claim",
            "codex_task_id": tr.get("task_id"),
            "codex_status": tr.get("status"),
        },
        "provenance_refs": [],
        "supersedes": None,
        "created_at": created_at,
        "content_digest": "",
    }


@dataclass(frozen=True)
class SyncResponse:
    """What the bridge answers to a synchronous invocation."""

    status_code: int
    body: dict[str, Any]


def sync_response(
    task_result: dict[str, Any] | None,
    ctx: InvocationContext,
    *,
    default_success_outcome: str,
    actor_id: str,
    created_at: str,
    timed_out: bool = False,
) -> SyncResponse:
    """Build the §13.2 200 body, or an execution-failure error body."""
    if timed_out:
        return SyncResponse(
            status_code=408,
            body={
                "error": "codex did not finish within the bridge's sync timeout",
                "class": CLASS_TIMEOUT,
            },
        )

    classification = classify(task_result, ctx, default_success_outcome=default_success_outcome)
    if not classification.domain:
        return SyncResponse(
            status_code=500,
            body={"error": classification.message, "class": classification.error_class},
        )

    return SyncResponse(
        status_code=200,
        body={
            "outcome": classification.outcome,
            "output": output_from_task_result(task_result),
            "ledger_delta": {
                "records": [
                    claim_record(task_result, ctx, actor_id=actor_id, created_at=created_at)
                ]
            },
            "artifact_refs": [],
            "continuation_ref": None,
            "usage": usage_from_task_result(task_result),
        },
    )


@dataclass(frozen=True)
class TerminalEvent:
    """What the bridge's async runner posts as the terminal §13.4 event."""

    kind: str  # "completed" | "failed"
    payload: dict[str, Any]


def terminal_event(
    task_result: dict[str, Any] | None,
    ctx: InvocationContext,
    *,
    default_success_outcome: str,
    actor_id: str,
    created_at: str,
    timed_out: bool = False,
    detail: str = "",
) -> TerminalEvent:
    """Build the terminal callback payload for an asynchronous invocation."""
    if timed_out:
        return TerminalEvent(
            kind="failed",
            payload={
                "class": CLASS_TIMEOUT,
                "message": "codex did not finish within the bridge's async wait bound",
                "detail": detail,
            },
        )

    classification = classify(task_result, ctx, default_success_outcome=default_success_outcome)
    if not classification.domain:
        return TerminalEvent(
            kind="failed",
            payload={
                "class": classification.error_class,
                "message": classification.message,
                "detail": detail,
            },
        )

    return TerminalEvent(
        kind="completed",
        payload={
            "outcome": classification.outcome,
            "output": output_from_task_result(task_result),
            "ledger_delta": {
                "records": [
                    claim_record(task_result, ctx, actor_id=actor_id, created_at=created_at)
                ]
            },
            "artifact_refs": [],
            "usage": usage_from_task_result(task_result),
        },
    )
