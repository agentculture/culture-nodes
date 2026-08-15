"""`notify` node input -> outbound message, and delivery result -> §13.2
InvocationResult. This is the one place the bridge decides what a `notify`
node's input means as a Discord post and what the post's outcome means as
an actor answer -- mirroring the sibling bridges' `mapping.py` role.

Two rules this module exists to enforce structurally, both from issue #68:

* **Fail-open by default.** `input.require_delivery` (absent or false)
  means the node always reports the `sent` domain outcome, whatever
  Discord's webhook said back -- a webhook outage must never fail the
  node or stall the run. `require_delivery: true` is the declared
  exception: a non-2xx (or a disabled webhook) becomes the
  `delivery_failed` domain outcome instead, so the workflow author can
  route a fallback edge on it.
* **Only status codes.** The one ledger record this module builds
  (`claim_record`) carries a domain outcome and an HTTP status -- never
  the webhook URL (this module never even sees it; `server.py` calls
  `webhook.post` directly) and never the message body that was sent. A
  `proposed` claim (PRD §10.4), never `observed`: a 2xx from Discord is
  evidence the request was accepted, not that a human read it.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from . import payload as payload_mod
from .webhook import PostResult

#: §13.5 error class this bridge originates for a malformed node input.
CLASS_ACTOR_REJECTED_INPUT = "actor_rejected_input"

#: The two domain outcomes this bridge ever reports. Both are `completed`
#: (§13.2/§13.4) events -- `delivery_failed` is a domain outcome the
#: workflow can route, never an engine-level failure (the repo-wide rule:
#: "domain outcome != technical status").
OUTCOME_SENT = "sent"
OUTCOME_DELIVERY_FAILED = "delivery_failed"


@dataclass(frozen=True)
class InvocationContext:
    """The parts of a §13.1 InvocationRequest the mapping layer needs,
    mirroring the sibling bridges' `InvocationContext`."""

    run_id: str = ""
    node_run_id: str | None = None
    attempt_id: str | None = None


@dataclass(frozen=True)
class ParsedMessage:
    message: payload_mod.NotifyMessage
    require_delivery: bool


def parse_message(raw_input: dict[str, Any]) -> tuple[ParsedMessage | None, str | None]:
    """Validate a `notify` node's `input` and build `(parsed, error)`.

    *error* is None on success, in which case *parsed* is not None (and
    vice versa). At least one of `content`, `title`, `description` must be
    present and non-blank -- an empty notification is not a request this
    bridge can honor.
    """
    if not isinstance(raw_input, dict):
        return None, "input must be a JSON object"

    content = raw_input.get("content", "")
    title = raw_input.get("title", "")
    description = raw_input.get("description", "")
    fields_raw = raw_input.get("fields", [])
    require_delivery = raw_input.get("require_delivery", False)

    if not isinstance(content, str):
        return None, "input.content must be a string when present"
    if not isinstance(title, str):
        return None, "input.title must be a string when present"
    if not isinstance(description, str):
        return None, "input.description must be a string when present"
    if not isinstance(require_delivery, bool):
        return None, "input.require_delivery must be a boolean when present"
    if not isinstance(fields_raw, list):
        return None, "input.fields must be an array when present"

    fields: list[payload_mod.NotifyField] = []
    for i, raw_field in enumerate(fields_raw):
        if not isinstance(raw_field, dict):
            return None, f"input.fields[{i}] must be an object"
        name = raw_field.get("name")
        value = raw_field.get("value")
        if not isinstance(name, str) or not name.strip():
            return None, f"input.fields[{i}].name is required and must be a non-empty string"
        if not isinstance(value, str) or not value.strip():
            return None, f"input.fields[{i}].value is required and must be a non-empty string"
        inline = raw_field.get("inline", False)
        if not isinstance(inline, bool):
            return None, f"input.fields[{i}].inline must be a boolean when present"
        fields.append(payload_mod.NotifyField(name=name, value=value, inline=inline))

    if not content.strip() and not title.strip() and not description.strip():
        return (
            None,
            "at least one of input.content, input.title, input.description is required",
        )

    message = payload_mod.NotifyMessage(
        content=content, title=title, description=description, fields=tuple(fields)
    )
    return ParsedMessage(message=message, require_delivery=require_delivery), None


def claim_record(
    ctx: InvocationContext,
    *,
    actor_id: str,
    created_at: str,
    outcome: str,
    delivered: bool,
    status_code: int | None,
) -> dict[str, Any]:
    """Build the ONE proposed ledger record this bridge ever emits.

    `data` carries only the outcome, the delivered flag, and the HTTP
    status code -- never the webhook URL, never the message that was sent
    (issue #68's hard constraint: "only status codes"). The engine
    re-checks authority on append regardless of what is claimed here.
    """
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
            "statement": (
                f"posted a Discord notification (status {status_code})"
                if delivered
                else "the Discord webhook request did not complete with a 2xx status"
            ),
            "kind": "notify-send",
            "outcome": outcome,
            "delivered": delivered,
            "status_code": status_code,
        },
        "provenance_refs": [],
        "supersedes": None,
        "created_at": created_at,
        "content_digest": "",
    }


def result_for(
    post_result: PostResult,
    status_code: int | None,
    *,
    require_delivery: bool,
    ctx: InvocationContext,
    actor_id: str,
    created_at: str,
) -> dict[str, Any]:
    """Build the §13.2 InvocationResult body from a completed `webhook.post`
    attempt.

    Default (`require_delivery` False): the outcome is ALWAYS `sent` -- a
    webhook outage must never fail the node (issue #68: "a webhook outage
    with the default settings leaves the run green"). `require_delivery`
    True turns anything other than `PostResult.POSTED` into the
    `delivery_failed` domain outcome instead, so the workflow author can
    declare a fallback edge on it.
    """
    delivered = post_result == PostResult.POSTED
    if require_delivery and not delivered:
        outcome = OUTCOME_DELIVERY_FAILED
    else:
        outcome = OUTCOME_SENT

    record = claim_record(
        ctx,
        actor_id=actor_id,
        created_at=created_at,
        outcome=outcome,
        delivered=delivered,
        status_code=status_code,
    )
    return {
        "outcome": outcome,
        "output": {"delivered": delivered, "status_code": status_code},
        "ledger_delta": {"records": [record]},
        "artifact_refs": [],
        "continuation_ref": None,
        "usage": usage_for_backend(),
    }


def usage_for_backend() -> dict[str, Any]:
    """Report an explicit model unknown for this non-model backend."""
    return {
        "input_tokens": 0,
        "output_tokens": 0,
        "cost": None,
        "currency": None,
        "model": "unknown:notify-backend-cannot-report",
    }
