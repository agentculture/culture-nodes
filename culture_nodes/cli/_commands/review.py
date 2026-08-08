"""``nodes review`` — thin REST client over the review transactions API.

Every verb here is one HTTP call to the Culture Nodes control-plane API
(``api/openapi/openapi.yaml``, ``reviews`` tag): create, commit. No ledger
authority logic lives in this module (spec decision c28) — confirm/reject
decisions are applied server-side, all-or-nothing, under the PRD §10.8
review protocol.
"""

from __future__ import annotations

import argparse

from culture_nodes.api_client import API_PREFIX, add_api_url_argument, client_from_args
from culture_nodes.cli._errors import EXIT_USER_ERROR, CliError
from culture_nodes.cli._output import emit_json_passthrough, emit_result


def _split_ids(raw: str | None) -> list[str]:
    if not raw:
        return []
    return [item.strip() for item in raw.split(",") if item.strip()]


def cmd_review_create(args: argparse.Namespace) -> int:
    record_ids = _split_ids(args.records)
    if not record_ids:
        raise CliError(
            code=EXIT_USER_ERROR,
            message="--records must name at least one ledger record id",
            remediation="pass a comma-separated list: --records id1,id2",
        )
    body: dict[str, object] = {"record_ids": record_ids, "ledger_version": args.ledger_version}
    if args.reviewer_actor_id:
        body["reviewer_actor_id"] = args.reviewer_actor_id
    client = client_from_args(args)
    resp = client.request("POST", f"{API_PREFIX}/runs/{args.run_id}/reviews", json_body=body)
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
        return 0
    payload = resp.payload or {}
    text = (
        f"id: {payload.get('id', '')}\n"
        f"status: {payload.get('status', '')}\n"
        f"ledger_version: {payload.get('ledger_version', '')}\n"
        f"records: {len(payload.get('record_ids') or [])}"
    )
    emit_result(text, json_mode=False)
    return 0


def cmd_review_commit(args: argparse.Namespace) -> int:
    confirm_ids = _split_ids(args.confirm)
    reject_ids = _split_ids(args.reject)
    if not confirm_ids and not reject_ids:
        raise CliError(
            code=EXIT_USER_ERROR,
            message="review commit needs at least one of --confirm or --reject",
            remediation="pass --confirm id1,id2 and/or --reject id3,id4",
        )
    decisions: dict[str, str] = {rid: "confirm" for rid in confirm_ids}
    overlap = [rid for rid in reject_ids if rid in decisions]
    if overlap:
        raise CliError(
            code=EXIT_USER_ERROR,
            message=f"record id(s) named in both --confirm and --reject: {', '.join(overlap)}",
            remediation="each record id must get exactly one verdict",
        )
    decisions.update({rid: "reject" for rid in reject_ids})

    body = {"decisions": decisions, "expected_ledger_version": args.ledger_version}
    client = client_from_args(args)
    # A 409 here (stale ledger version, superseded target, already-committed
    # review) is relayed by ApiClient as a CliError carrying the API's own
    # code/message/remediation verbatim — no special-casing needed.
    resp = client.request("POST", f"{API_PREFIX}/reviews/{args.review_id}/commit", json_body=body)
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
        return 0
    payload = resp.payload or {}
    text = (
        f"review_id: {payload.get('review_id', '')}\n"
        f"ledger_version: {payload.get('ledger_version', '')}\n"
        f"records: {len(payload.get('records') or [])}"
    )
    emit_result(text, json_mode=False)
    return 0


def _bare_noun(args: argparse.Namespace) -> int:
    emit_result(
        "usage: nodes review {create,commit} ...\nrun 'nodes explain review' for details",
        json_mode=False,
    )
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser(
        "review", help="Thin client for the human review transactions API (create/commit)."
    )
    p.add_argument("--json", action="store_true", help="Emit structured JSON.")
    p.set_defaults(func=_bare_noun, json=False)
    noun_sub = p.add_subparsers(dest="review_command", parser_class=type(p))

    create = noun_sub.add_parser(
        "create", help="Create a review request over a batch of ledger records."
    )
    create.add_argument("run_id", help="The run id.")
    create.add_argument(
        "--records", dest="records", required=True, help="Comma-separated ledger record ids."
    )
    create.add_argument(
        "--ledger-version",
        dest="ledger_version",
        type=int,
        required=True,
        help="The ledger version this request was read at.",
    )
    create.add_argument(
        "--reviewer-actor-id",
        dest="reviewer_actor_id",
        default=None,
        help="The human actor this review is bound to.",
    )
    create.add_argument("--json", action="store_true", help="Emit structured JSON.")
    add_api_url_argument(create)
    create.set_defaults(func=cmd_review_create)

    commit = noun_sub.add_parser("commit", help="Commit a review's confirm/reject decisions.")
    commit.add_argument("review_id", help="The review request id.")
    commit.add_argument(
        "--confirm", dest="confirm", default=None, help="Comma-separated record ids to confirm."
    )
    commit.add_argument(
        "--reject", dest="reject", default=None, help="Comma-separated record ids to reject."
    )
    commit.add_argument(
        "--ledger-version",
        dest="ledger_version",
        type=int,
        required=True,
        help="The ledger version the caller expects to still hold.",
    )
    commit.add_argument("--json", action="store_true", help="Emit structured JSON.")
    add_api_url_argument(commit)
    commit.set_defaults(func=cmd_review_commit)
