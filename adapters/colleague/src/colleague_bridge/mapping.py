"""TaskResult -> actor-protocol mapping (PRD §13.2/§13.4, colleague contract v1).

This module is the one place the bridge decides what a colleague
`TaskResult` (colleague contract v1, `docs/contract.md`) means as an actor
answer. It never touches a socket or a subprocess — every function here is a
pure translation from a parsed JSON dict (or the absence of one) to the
wire shapes `internal/actors/protocol.go` defines, so the whole mapping
table is unit-testable without ever invoking `colleague`.

The one rule every other decision in this file serves: **incomplete is
never success.** `TaskResult.status == "incomplete"` means colleague's step
budget ran out (or a stop landed with no re-nudge) — the summary is a
best-effort partial, not an authoritative result (contract.md's own exit-code
table). This module reports it as the node's own declared `incomplete`
domain outcome ONLY when the invocation explicitly named one; otherwise it
is reported as an execution failure. It is never silently folded into
`"completed"`.

Task t10's `workspace_measured` block: every `sync_response`/`terminal_event`
body carries a `workspace_measured` key, STRUCTURALLY SEPARATE from
`output` (which carries colleague's own model-claimed `changed_files` and
`artifacts_path` — see `output_from_task_result`). `workspace_measured` is
never built here — this module stays a pure translation with no subprocess
of its own — it is measured by `workspace.py` (via real `git` calls
bracketing the session) and simply passed through by the
`workspace_measured` keyword argument below. The shape, identical across
`claude_code_bridge`, `codex_bridge`, and `colleague_bridge`:

    {
      "measured": bool,
      "repo": str | None,
      "reason": str | None,       # unmeasured reason, or partial-probe note while measured
      "branch": str | None,
      "head_before": str | None,  # git rev-parse HEAD, captured before dispatch
      "head_after": str | None,   # git rev-parse HEAD, captured after
      "status_porcelain": str | None,  # git status --porcelain, captured after
      "changed_files": list[str],      # bridge-measured, never model-claimed
      "diffstat": str | None,          # git diff --stat vs head_before
    }

A caller that omits `workspace_measured` (e.g. an older test literal) gets
`measured: False` with an honest reason rather than a crash or a fabricated
value — real dispatch through `server.py`/`async_runner.py` always supplies
a real measurement.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

#: colleague contract v1 status values (docs/contract.md "Exit-code
#: semantics"). Anything else is a status this bridge does not recognise.
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
    #: incomplete run is reported as an execution failure instead —
    #: never as a silent success.
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

    *task_result* is `None` when the colleague subprocess produced no
    parseable result at all (a crash before any artifact/JSON was written);
    every other case is what `TaskResult.status` says.
    """
    if task_result is None:
        return Classification(
            domain=False,
            message="colleague produced no parseable result",
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
                "colleague reported status=incomplete (step-budget exhaustion or a "
                "stop with no re-nudge success); the invocation declared no "
                "incomplete domain outcome, so this is reported as an execution "
                "failure rather than success — incomplete is never success"
            ),
            error_class=CLASS_EXECUTION,
        )

    if status == STATUS_ERROR:
        return Classification(
            domain=False,
            message=task_result.get("error") or "colleague reported status=error",
            error_class=CLASS_EXECUTION,
        )

    return Classification(
        domain=False,
        message=f"colleague produced an unrecognised status {status!r}",
        error_class=CLASS_EXECUTION,
    )


def _default_workspace_measured() -> dict[str, Any]:
    """The `workspace_measured` fallback when a caller builds a response
    without supplying one. Mirrors `workspace.unmeasured()`'s shape exactly
    (duplicated rather than imported: this module never depends on the
    subprocess-touching `workspace` module, per its own docstring)."""
    return {
        "measured": False,
        "repo": None,
        "reason": "no workspace measurement was supplied to the mapping layer",
        "branch": None,
        "head_before": None,
        "head_after": None,
        "status_porcelain": None,
        "changed_files": [],
        "diffstat": None,
    }


def usage_from_task_result(task_result: dict[str, Any] | None) -> dict[str, Any]:
    """Map `TaskResult.usage` (prompt/completion/total) onto §13.2 `Usage`.

    colleague's usage is exact, API-reported token accounting
    (docs/contract.md); the bridge never estimates cost, so `cost`/`currency`
    are always null — an actor that does not price its work says so with
    null rather than a zero that reads as free (protocol.go's own docstring).
    """
    usage = (task_result or {}).get("usage") or {}
    return {
        "input_tokens": int(usage.get("prompt_tokens") or 0),
        "output_tokens": int(usage.get("completion_tokens") or 0),
        "cost": None,
        "currency": None,
    }


def output_from_task_result(task_result: dict[str, Any] | None) -> dict[str, Any]:
    """Map the fields the task names: `{summary, changed_files, artifacts_path}`."""
    tr = task_result or {}
    return {
        "summary": tr.get("summary") or "",
        "changed_files": list(tr.get("changed_files") or []),
        "artifacts_path": tr.get("artifacts_path"),
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
    colleague's own summary is a completion CLAIM, not verified evidence
    (PRD §10.5); the bridge never emits `confirmed`/`observed` (no actor
    promotes its own proposal — PRD ledger authority model). Only the
    envelope fields a *proposer* is expected to set are populated (PRD
    §10.7's own worked example carries exactly `record_type`, `authority`,
    `subject_ref`, `data`); `id`/`content_digest` are left for the ledger
    runtime to assign at append (schemas/ledger/envelope.schema.json's own
    comment: "computed by the ledger runtime ... at append time").
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
            "colleague_task_id": tr.get("task_id"),
            "colleague_status": tr.get("status"),
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
    workspace_measured: dict[str, Any] | None = None,
) -> SyncResponse:
    """Build the §13.2 200 body, or an execution-failure error body.

    *workspace_measured* (task t10) is measured by `workspace.py` around
    the dispatch and attached to EVERY branch below, success or failure —
    the workspace may have changed even when the session itself did not
    succeed, so this is never conditioned on `classification.domain`.
    """
    measured = (
        workspace_measured if workspace_measured is not None else _default_workspace_measured()
    )

    if timed_out:
        return SyncResponse(
            status_code=408,
            body={
                "error": "colleague did not finish within the bridge's sync timeout",
                "class": CLASS_TIMEOUT,
                "workspace_measured": measured,
            },
        )

    classification = classify(task_result, ctx, default_success_outcome=default_success_outcome)
    if not classification.domain:
        body = {
            "error": classification.message,
            "class": classification.error_class,
            "workspace_measured": measured,
        }
        # Issue #32: a failed session still burned real tokens. When colleague
        # produced a parseable terminal result, its API-reported usage rides
        # the failure body; a result-less crash stays usage-less — absent,
        # never fabricated zeros.
        if task_result is not None:
            body["usage"] = usage_from_task_result(task_result)
        return SyncResponse(status_code=500, body=body)

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
            "workspace_measured": measured,
        },
    )


@dataclass(frozen=True)
class TerminalEvent:
    """What the bridge's async poller posts as the terminal §13.4 event."""

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
    workspace_measured: dict[str, Any] | None = None,
) -> TerminalEvent:
    """Build the terminal callback payload for an asynchronous invocation.

    *workspace_measured* (task t10) is attached to EVERY branch below, the
    same way `sync_response` does — see its own docstring for why.
    """
    measured = (
        workspace_measured if workspace_measured is not None else _default_workspace_measured()
    )

    if timed_out:
        return TerminalEvent(
            kind="failed",
            payload={
                "class": CLASS_TIMEOUT,
                "message": "colleague did not finish within the bridge's async wait bound",
                "detail": detail,
                "workspace_measured": measured,
            },
        )

    classification = classify(task_result, ctx, default_success_outcome=default_success_outcome)
    if not classification.domain:
        payload = {
            "class": classification.error_class,
            "message": classification.message,
            "detail": detail,
            "workspace_measured": measured,
        }
        # Issue #32: same rule as sync_response — real usage from a parseable
        # terminal result rides the failed payload; a result-less crash stays
        # usage-less rather than reporting fabricated zeros.
        if task_result is not None:
            payload["usage"] = usage_from_task_result(task_result)
        return TerminalEvent(kind="failed", payload=payload)

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
            "workspace_measured": measured,
        },
    )
