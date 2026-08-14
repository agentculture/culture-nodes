"""Human submission -> actor-protocol mapping (PRD §13.2/§13.4).

This module is the one place the bridge decides what a human's submission
`{outcome, output, note}` means as an actor answer. It never touches a
socket or a file — every function here is a pure translation to the wire
shapes `internal/actors/protocol.go` defines, mirroring the sibling
bridges' `mapping.py` role.

Two deliberate divergences from the agent bridges, both honesty rules:

* **No usage block, ever.** Humans report no token usage. `CompletedPayload.
  Usage` is a pointer that stays nil when the key is absent ("a nil Usage
  always becomes a nil engine.Usage, never a fabricated zero block") — so
  this bridge OMITS `usage` rather than sending zeros nobody measured.
* **The claim record is human-origin, proposed-only.** Via the ordinary
  append path a human-origin record may carry `proposed` and nothing
  stronger (`internal/ledger/authority.go` `checkHumanAuthority`):
  confirmation and rejection are review transactions (PRD §10.8), not
  callback payloads. Manual submissions use `human-submission`; the tracker
  (merge OR reply/terminal-state observations, issue #71) uses an explicit,
  validated `observed` marker to select the `observed-submission` sibling
  and attach its collection method plus that method's own evidence field
  (`merge_commit` for `github_pr_merged`, `reference` for
  `github_pr_reply`/`github_pr_closed`). Neither path changes origin or
  authority.

There is also no `classify` ladder here: the human names the domain outcome
explicitly in the submission, so the mapping never infers one. A submission
without an outcome is rejected at the surface (`submission_error`), never
defaulted — a person who did not say what happened has not answered.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

#: §13.5 error classes this bridge ever originates.
CLASS_EXECUTION = "execution"
CLASS_ACTOR_REJECTED_INPUT = "actor_rejected_input"

#: The tracker collection methods this bridge accepts inside a submission's
#: `observed` marker, and the ONE extra evidence field each requires beyond
#: `collection_method` itself. `github_pr_merged` is t16's original merge
#: observation; `github_pr_reply` and `github_pr_closed` are issue #71's
#: decision-node observations (a qualifying PR reply, and the PR closing
#: unmerged while a question was pending, respectively). Adding a kind here
#: is the ONLY change needed to accept a new tracker observation shape —
#: `submission_error` and `claim_record` are both generic over this map.
_OBSERVED_REQUIRED_FIELDS: dict[str, frozenset[str]] = {
    "github_pr_merged": frozenset({"merge_commit"}),
    "github_pr_reply": frozenset({"reference"}),
    "github_pr_closed": frozenset({"reference"}),
}


@dataclass(frozen=True)
class InvocationContext:
    """The parts of a §13.1 InvocationRequest the mapping layer needs,
    mirroring the sibling bridges' `InvocationContext`."""

    run_id: str = ""
    node_run_id: str | None = None
    attempt_id: str | None = None


def submission_error(body: dict[str, Any]) -> str | None:
    """Validate a submission `{outcome, output?, note?, observed?}`.

    Returns a human-readable refusal, or None when the submission is
    acceptable. `outcome` is required and never defaulted (see module
    docstring); `output` must be a JSON object when present because it is
    bound into the node's contract-shaped output; `note` is free text.
    """
    outcome = body.get("outcome")
    if not isinstance(outcome, str) or not outcome.strip():
        return "outcome is required and must be a non-empty string naming a domain outcome"
    output = body.get("output")
    if output is not None and not isinstance(output, dict):
        return "output must be a JSON object when present"
    note = body.get("note")
    if note is not None and not isinstance(note, str):
        return "note must be a string when present"
    if "observed" in body:
        observed = body["observed"]
        if not isinstance(observed, dict):
            return "observed must be a JSON object when present"
        collection_method = observed.get("collection_method")
        required = _OBSERVED_REQUIRED_FIELDS.get(collection_method)
        if required is None:
            return "observed.collection_method must be one of: " + ", ".join(
                sorted(_OBSERVED_REQUIRED_FIELDS)
            )
        if set(observed) != {"collection_method"} | required:
            return (
                f"observed for collection_method {collection_method!r} must contain "
                f"exactly collection_method and {sorted(required)}"
            )
        for field_name in required:
            value = observed.get(field_name)
            if not isinstance(value, str) or not value.strip():
                return f"observed.{field_name} must be a non-empty string"
    return None


def claim_record(
    submission: dict[str, Any],
    ctx: InvocationContext,
    *,
    actor_id: str,
    created_at: str,
) -> dict[str, Any]:
    """Build the ONE proposed ledger record the bridge ever emits.

    A `claim` record, `authority: "proposed"`, `origin.kind: "human"` — the
    same envelope the sibling bridges' `claim_record` builds, with the
    origin kind and the data payload's backend-specific fields swapped for
    the human case. The engine re-checks authority on append regardless of
    what is claimed here.
    """
    observed = submission.get("observed")
    is_observed = isinstance(observed, dict)
    data = {
        # The ledger schema requires a non-empty statement. A submitter
        # who put their prose in output.note (or sent no note at all)
        # must not produce an engine-side contract_rejected on an
        # otherwise-valid human decision (found live: run
        # 01KZXD609QRFHWS8YQ6MRZ1Y0F failed on an empty statement), so
        # the statement falls back through output.note to a generated
        # sentence naming the outcome — never empty.
        "statement": (
            submission.get("note")
            or (submission.get("output") or {}).get("note")
            or f"human submitted outcome {submission.get('outcome')}"
        ),
        "kind": "observed-submission" if is_observed else "human-submission",
        "outcome": submission.get("outcome"),
    }
    if is_observed:
        # submission_error has already pinned this marker to one of the
        # collection methods _OBSERVED_REQUIRED_FIELDS understands, and
        # validated its one extra evidence field. Keep authority and origin
        # unchanged: this is honest attribution inside a proposed bridge
        # claim, not runner-origin observed authority.
        data["collection_method"] = observed["collection_method"]
        for field_name, value in observed.items():
            if field_name == "collection_method":
                continue
            data[field_name] = value.strip() if isinstance(value, str) else value

    return {
        "id": "",
        "schema_version": "nodes.culture.dev/ledger/v1alpha1",
        "record_type": "claim",
        "run_id": ctx.run_id,
        "node_run_id": ctx.node_run_id,
        "attempt_id": ctx.attempt_id,
        "origin": {"kind": "human", "actor_id": actor_id},
        "authority": "proposed",
        "subject_ref": None,
        "data": data,
        "provenance_refs": [],
        "supersedes": None,
        "created_at": created_at,
        "content_digest": "",
    }


@dataclass(frozen=True)
class TerminalEvent:
    """What the bridge posts as the terminal §13.4 event on submission."""

    kind: str  # always "completed" — a submitted outcome is a domain outcome
    payload: dict[str, Any]


def completed_event(
    submission: dict[str, Any],
    ctx: InvocationContext,
    *,
    actor_id: str,
    created_at: str,
) -> TerminalEvent:
    """Build the terminal `completed` payload from a validated submission.

    The human's `output` object passes through verbatim as the node's
    output (defaulting to `{}` when omitted); the note travels in the claim
    record's statement. NO `usage` key — see module docstring.
    """
    output = submission.get("output")
    return TerminalEvent(
        kind="completed",
        payload={
            "outcome": str(submission["outcome"]).strip(),
            "output": output if isinstance(output, dict) else {},
            "ledger_delta": {
                "records": [claim_record(submission, ctx, actor_id=actor_id, created_at=created_at)]
            },
            "artifact_refs": [],
        },
    )
