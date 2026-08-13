"""claude `type: "result"` -> actor-protocol mapping (PRD §13.2/§13.4).

This module is the one place the bridge decides what a claude headless
`ResultMessage` (the JSON object `claude -p --output-format json` prints, or
the terminal line of `--output-format stream-json`) means as an actor
answer. It never touches a socket or a subprocess — every function here is a
pure translation from a parsed JSON dict (or the absence of one) to the wire
shapes `internal/actors/protocol.go` defines, mirroring
`adapters/colleague/src/colleague_bridge/mapping.py`'s own role for
colleague's `TaskResult`.

Pinned shape (the same wire shape `claude_agent_sdk.types.ResultMessage` /
`_internal/message_parser.py`'s `"result"` case decode — this bridge does
not depend on that package, it is simply the best public reference for the
CLI's own JSON output this bridge was built against, the same way
colleague_bridge/mapping.py is pinned against colleague's `docs/contract.md`):

    {
      "type": "result",
      "subtype": "success" | "error_max_turns" | "error_during_execution" | ...,
      "is_error": bool,
      "num_turns": int,
      "session_id": str,
      "stop_reason": str | null,
      "total_cost_usd": float | null,
      "usage": {"input_tokens": int, "output_tokens": int, ...} | null,
      "result": str | null
    }

The one rule every other decision in this file serves: **incomplete is
never success.** `subtype == "error_max_turns"` means claude's turn budget
ran out before it produced a final answer — a best-effort partial, not an
authoritative result. This module reports it as the node's own declared
`incomplete` domain outcome ONLY when the invocation explicitly named one
(`input.incomplete_outcome`); otherwise it is reported as an execution
failure. It is never silently folded into `"completed"`. The same is true, a
fortiori, of a session that crashed and produced no parseable result at
all — `result is None` is exactly as much "not success" as an explicit
error subtype is.

Task t10's `workspace_measured` block: every `sync_response`/`terminal_event`
body carries a `workspace_measured` key, STRUCTURALLY SEPARATE from
`output` (which is claude's own model-claimed `changed_files`, always empty
for this backend — see `output_from_result`). `workspace_measured` is never
built here — this module stays a pure translation with no subprocess of its
own — it is measured by `workspace.py` (via real `git` calls bracketing the
session) and simply passed through by the `workspace_measured` keyword
argument below. The shape, identical across `claude_code_bridge`,
`codex_bridge`, and `colleague_bridge`:

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

#: The one non-error subtype a claude `type: "result"` message reports.
SUBTYPE_SUCCESS = "success"
#: claude's turn budget was exhausted before a final answer — the "ran out
#: of budget" flavor of not-done, analogous to colleague contract v1's
#: `TaskResult.status == "incomplete"`.
SUBTYPE_ERROR_MAX_TURNS = "error_max_turns"

#: §13.5 error classes this module ever produces (a strict subset of
#: internal/actors.ErrorClass — the bridge only ever originates the classes
#: an actor is positioned to know about).
CLASS_EXECUTION = "execution"
CLASS_TIMEOUT = "timeout"
CLASS_ACTOR_REJECTED_INPUT = "actor_rejected_input"


@dataclass(frozen=True)
class InvocationContext:
    """The parts of a §13.1 InvocationRequest the mapping layer needs.

    A narrow view rather than the whole request dict, mirroring
    `colleague_bridge.mapping.InvocationContext` field for field.
    """

    run_id: str = ""
    node_run_id: str | None = None
    attempt_id: str | None = None
    #: The domain outcome to report on success, when the invocation's own
    #: input declared one (`input.success_outcome`). `None` defers to
    #: `Config.default_success_outcome`.
    success_outcome: str | None = None
    #: The domain outcome to report for `subtype == "error_max_turns"`, when
    #: the invocation's input declares the node accepts that outcome
    #: (`input.incomplete_outcome`, a non-empty string). `None` (the
    #: default) means the node's contract does not declare it, so a
    #: turn-budget-exhausted run is reported as an execution failure
    #: instead — never as a silent success.
    incomplete_outcome: str | None = None
    #: A prior session handle to resume (from the engine's
    #: `continuation_ref` on the InvocationRequest). `None` means cold-start.
    continuation_ref: str | None = None


@dataclass(frozen=True)
class Classification:
    """What one result (or its absence) means as a protocol answer."""

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
    result: dict[str, Any] | None, ctx: InvocationContext, *, default_success_outcome: str
) -> Classification:
    """Decide what *result* means, independent of sync vs async.

    *result* is `None` when the claude subprocess produced no parseable
    `type: "result"` object at all — a crash, a kill, or output that was
    never valid JSON. That is a failure exactly like any other unrecognised
    outcome: this bridge never guesses success from silence.
    """
    if result is None:
        return Classification(
            domain=False,
            message="claude produced no parseable result (the session crashed, was killed, or "
            'never wrote a type: "result" record)',
            error_class=CLASS_EXECUTION,
        )

    subtype = result.get("subtype")
    is_error = bool(result.get("is_error"))

    if subtype == SUBTYPE_SUCCESS and not is_error:
        outcome = ctx.success_outcome or default_success_outcome
        return Classification(domain=True, outcome=outcome)

    if subtype == SUBTYPE_ERROR_MAX_TURNS:
        if ctx.incomplete_outcome:
            return Classification(domain=True, outcome=ctx.incomplete_outcome)
        return Classification(
            domain=False,
            message=(
                "claude reported subtype=error_max_turns (turn-budget exhaustion before a "
                "final answer); the invocation declared no incomplete domain outcome, so this "
                "is reported as an execution failure rather than success — incomplete is never "
                "success"
            ),
            error_class=CLASS_EXECUTION,
        )

    # Every other subtype (error_during_execution, an unrecognised one this
    # bridge has never seen, or is_error=true with subtype=success — a
    # combination the CLI should never produce, but this module never
    # trusts is_error and subtype to agree without checking both) is a
    # domain-less execution failure.
    return Classification(
        domain=False,
        message=f"claude reported subtype={subtype!r} is_error={is_error!r}",
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


def usage_from_result(result: dict[str, Any] | None) -> dict[str, Any]:
    """Map claude's reported usage and identity onto §13.2 `Usage`.

    Unlike colleague, claude reports a real `total_cost_usd`, so it passes
    through with USD. `cache_read_input_tokens` is the cache-hit count and
    therefore maps to `cached_input_tokens`. We deliberately do NOT add
    `cache_creation_input_tokens`: those are uncached tokens spent writing
    a new cache entry, not reads served from cache, and adding them would
    overstate the cache ratio this field exists to measure.

    A direct model name is preferred. Some CLI result versions expose only
    a `modelUsage` map; its sole key is unambiguous, while a multi-model
    aggregate cannot honestly be labeled as one model and is left absent.
    """
    r = result or {}
    usage = r.get("usage") or {}
    cost = r.get("total_cost_usd")
    mapped: dict[str, Any] = {
        "input_tokens": int(usage.get("input_tokens") or 0),
        "output_tokens": int(usage.get("output_tokens") or 0),
        "cost": float(cost) if isinstance(cost, (int, float)) else None,
        "currency": "USD" if isinstance(cost, (int, float)) else None,
    }

    cached_input_tokens = usage.get("cache_read_input_tokens")
    if cached_input_tokens is not None:
        mapped["cached_input_tokens"] = int(cached_input_tokens)

    model = r.get("model")
    if not isinstance(model, str) or not model:
        model_usage = r.get("modelUsage") or r.get("model_usage")
        if isinstance(model_usage, dict) and len(model_usage) == 1:
            model = next(iter(model_usage))
    if isinstance(model, str) and model:
        mapped["model"] = model

    session_id = r.get("session_id")
    if isinstance(session_id, str) and session_id:
        mapped["thread_id"] = session_id

    return mapped


def _attach_termination_reason(
    payload: dict[str, Any], result: dict[str, Any] | None
) -> dict[str, Any]:
    """Carry claude's stop reason beside usage, never by inventing usage."""
    reason = (result or {}).get("stop_reason")
    if isinstance(reason, str) and reason:
        payload["termination_reason"] = reason
    return payload


def declared_result_override(result):
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

    tr = result or {}
    text = (tr.get("result") or tr.get("summary") or "").strip()
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


def output_from_result(result: dict[str, Any] | None) -> dict[str, Any]:
    """Map the fields the node contract sees: `{summary, changed_files,
    artifacts_path}`.

    claude's headless result carries no `changed_files`/`artifacts_path`
    equivalent of its own (unlike colleague, which writes an artifact file
    per work item) — both are always reported empty/null here. A node
    contract that needs to know what changed reads it from claude's own
    `result` text (the `summary` field below) or from git status on the
    dispatched repo itself, which is outside this bridge's mapping layer.
    """
    r = result or {}
    return {
        "summary": r.get("result") or "",
        "changed_files": [],
        "artifacts_path": None,
    }


def claim_record(
    result: dict[str, Any] | None,
    ctx: InvocationContext,
    *,
    actor_id: str,
    created_at: str,
) -> dict[str, Any]:
    """Build the ONE proposed ledger record the bridge ever emits.

    A `claim` record, `authority: "proposed"`, `origin.kind: "agent"` —
    claude's own final answer is a completion CLAIM, not verified evidence
    (PRD §10.5); the bridge never emits `confirmed`/`observed` (no actor
    promotes its own proposal — PRD ledger authority model). Mirrors
    `colleague_bridge.mapping.claim_record`'s envelope exactly; only the
    `data` payload's backend-specific fields differ (`claude_session_id`/
    `claude_subtype` in place of `colleague_task_id`/`colleague_status`).
    """
    r = result or {}
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
            "statement": r.get("result") or "",
            "kind": "completion-claim",
            "claude_session_id": r.get("session_id"),
            "claude_subtype": r.get("subtype"),
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
    result: dict[str, Any] | None,
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
                "error": "claude did not finish within the bridge's sync timeout",
                "class": CLASS_TIMEOUT,
                "workspace_measured": measured,
            },
        )

    classification = classify(result, ctx, default_success_outcome=default_success_outcome)
    if not classification.domain:
        body = {
            "error": classification.message,
            "class": classification.error_class,
            "workspace_measured": measured,
        }
        # Issue #32: a failed session still burned real tokens. When claude
        # produced a parseable terminal result, its API-reported usage rides
        # the failure body; a result-less crash stays usage-less — absent,
        # never fabricated zeros.
        if result is not None:
            body["usage"] = usage_from_result(result)
        return SyncResponse(status_code=500, body=_attach_termination_reason(body, result))

    _declared = declared_result_override(result)
    body = {
        "outcome": _declared[0] if _declared else classification.outcome,
        "output": _declared[1] if _declared else output_from_result(result),
        "ledger_delta": {
            "records": [claim_record(result, ctx, actor_id=actor_id, created_at=created_at)]
        },
        "artifact_refs": [],
        "continuation_ref": r.get("session_id") if isinstance(r.get("session_id"), str) else None,
        "usage": usage_from_result(result),
        "workspace_measured": measured,
    }
    return SyncResponse(status_code=200, body=_attach_termination_reason(body, result))


@dataclass(frozen=True)
class TerminalEvent:
    """What the bridge's async poller posts as the terminal §13.4 event."""

    kind: str  # "completed" | "failed"
    payload: dict[str, Any]


def terminal_event(
    result: dict[str, Any] | None,
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
                "message": "claude did not finish within the bridge's async wait bound",
                "detail": detail,
                "workspace_measured": measured,
            },
        )

    classification = classify(result, ctx, default_success_outcome=default_success_outcome)
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
        if result is not None:
            payload["usage"] = usage_from_result(result)
        return TerminalEvent(kind="failed", payload=_attach_termination_reason(payload, result))

    _declared = declared_result_override(result)
    payload = {
        "outcome": _declared[0] if _declared else classification.outcome,
        "output": _declared[1] if _declared else output_from_result(result),
        "ledger_delta": {
            "records": [claim_record(result, ctx, actor_id=actor_id, created_at=created_at)]
        },
        "artifact_refs": [],
        "continuation_ref": r.get("session_id") if isinstance(r.get("session_id"), str) else None,
        "usage": usage_from_result(result),
        "workspace_measured": measured,
    }
    return TerminalEvent(kind="completed", payload=_attach_termination_reason(payload, result))
