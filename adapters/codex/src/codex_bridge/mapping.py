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

Task t10's `workspace_measured` block: every `sync_response`/`terminal_event`
body carries a `workspace_measured` key, STRUCTURALLY SEPARATE from
`output` (which carries codex's own model-claimed `changed_files` — see
`output_from_task_result`). `workspace_measured` is never built here — this
module stays a pure translation with no subprocess of its own — it is
measured by `workspace.py` (via real `git` calls bracketing the session)
and simply passed through by the `workspace_measured` keyword argument
below. The shape, identical across `claude_code_bridge`, `codex_bridge`,
and `colleague_bridge`:

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


def usage_from_task_result(task_result: dict[str, Any] | None) -> dict[str, Any] | None:
    """Map provider-reported codex usage onto §13.2 `Usage`.

    Input and output are the block-presence sentinel. If either count was
    not reported, returning ``None`` makes it structurally impossible for a
    caller to manufacture the old 0/0 block. Real zero counts remain real
    because presence is checked with ``is None``, never truthiness.

    Codex does not price its own work here, so `cost`/`currency` stay null.
    Cache reads, reasoning output, model metadata, and the provider thread
    are independently optional within an otherwise real usage block.
    """
    task = task_result or {}
    usage = task.get("usage") or {}
    input_tokens = usage.get("input_tokens")
    output_tokens = usage.get("output_tokens")
    if input_tokens is None or output_tokens is None:
        return None

    mapped: dict[str, Any] = {
        "input_tokens": int(input_tokens),
        "output_tokens": int(output_tokens),
        "cost": None,
        "currency": None,
    }

    cached_input_tokens = usage.get("cached_input_tokens")
    if cached_input_tokens is not None:
        mapped["cached_input_tokens"] = int(cached_input_tokens)

    reasoning_tokens = usage.get("reasoning_output_tokens")
    if reasoning_tokens is not None:
        mapped["reasoning_tokens"] = int(reasoning_tokens)

    model = task.get("model")
    if isinstance(model, str) and model:
        mapped["model"] = model

    thread_id = task.get("task_id")
    if isinstance(thread_id, str) and thread_id:
        mapped["thread_id"] = thread_id

    return mapped


def _attach_provider_telemetry(
    payload: dict[str, Any], task_result: dict[str, Any] | None
) -> dict[str, Any]:
    """Attach independently optional usage and termination telemetry.

    Keeping this single seam for success and failure payloads is what makes
    the no-fabricated-zeros rule structural: no caller decides that a parsed
    result is enough to imply usage. Only ``usage_from_task_result`` can add
    that block, and it returns ``None`` until both required counts exist.
    """
    usage = usage_from_task_result(task_result)
    if usage is not None:
        payload["usage"] = usage

    reason = (task_result or {}).get("termination_reason")
    if isinstance(reason, str) and reason:
        payload["termination_reason"] = reason

    return payload


def declared_result_override(task_result):
    """§13.2 lets the RESULT name the outcome; the session declares it by
    making its final message exactly {"outcome": "<name>", "output": {...}}.
    The bridge passes both through verbatim and the ENGINE's contract
    validation stays the enforcer (an undeclared outcome or a schema
    mismatch is contract_rejected there, never guessed here). Any other
    final-message shape keeps today's envelope. Identical helper in all
    three bridges (all-backends rule; deviation d4 of the
    attempts-evidence-humans-loops build — two-outcome nodes were
    undrivable because bridges hardcoded the outcome).
    """
    import json as _json

    tr = task_result or {}
    text = (tr.get("summary") or "").strip()
    if not (text.startswith("{") and text.endswith("}")):
        return None
    try:
        parsed = _json.loads(text)
    except (ValueError, TypeError):
        return None
    if not isinstance(parsed, dict):
        return None
    outcome = parsed.get("outcome")
    output = parsed.get("output")
    if isinstance(outcome, str) and outcome and isinstance(output, dict):
        return outcome, output
    return None


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
        body = {
            "error": "codex did not finish within the bridge's sync timeout",
            "class": CLASS_TIMEOUT,
            "workspace_measured": measured,
        }
        return SyncResponse(
            status_code=408, body=_attach_provider_telemetry(body, task_result)
        )

    classification = classify(task_result, ctx, default_success_outcome=default_success_outcome)
    if not classification.domain:
        body = {
            "error": classification.message,
            "class": classification.error_class,
            "workspace_measured": measured,
        }
        # Usage and the provider reason are independent: a failed turn can
        # report either one without forcing the other into existence.
        return SyncResponse(
            status_code=500, body=_attach_provider_telemetry(body, task_result)
        )

    declared = declared_result_override(task_result)
    body = {
        "outcome": declared[0] if declared else classification.outcome,
        "output": declared[1] if declared else output_from_task_result(task_result),
        "ledger_delta": {
            "records": [
                claim_record(task_result, ctx, actor_id=actor_id, created_at=created_at)
            ]
        },
        "artifact_refs": [],
        "continuation_ref": None,
        "workspace_measured": measured,
    }
    return SyncResponse(
        status_code=200, body=_attach_provider_telemetry(body, task_result)
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
        payload = {
            "class": CLASS_TIMEOUT,
            "message": "codex did not finish within the bridge's async wait bound",
            "detail": detail,
            "workspace_measured": measured,
        }
        return TerminalEvent(
            kind="failed", payload=_attach_provider_telemetry(payload, task_result)
        )

    classification = classify(task_result, ctx, default_success_outcome=default_success_outcome)
    if not classification.domain:
        payload = {
            "class": classification.error_class,
            "message": classification.message,
            "detail": detail,
            "workspace_measured": measured,
        }
        return TerminalEvent(
            kind="failed", payload=_attach_provider_telemetry(payload, task_result)
        )

    _declared = declared_result_override(task_result)
    payload = {
        "outcome": _declared[0] if _declared else classification.outcome,
        "output": _declared[1] if _declared else output_from_task_result(task_result),
        "ledger_delta": {
            "records": [
                claim_record(task_result, ctx, actor_id=actor_id, created_at=created_at)
            ]
        },
        "artifact_refs": [],
        "workspace_measured": measured,
    }
    return TerminalEvent(
        kind="completed", payload=_attach_provider_telemetry(payload, task_result)
    )
