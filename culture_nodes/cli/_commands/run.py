"""``nodes run`` — thin REST client over the runs API.

Every verb here is one HTTP call to the Culture Nodes control-plane API
(``api/openapi/openapi.yaml``, ``runs``/``events``/``grades`` tags): create,
list, get, cancel, events, retag, grade. No engine logic lives in this module
(spec decision c28).
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from culture_nodes.api_client import API_PREFIX, add_api_url_argument, client_from_args
from culture_nodes.cli._errors import EXIT_ENV_ERROR, EXIT_USER_ERROR, CliError
from culture_nodes.cli._output import (
    JSON_FLAG_HELP,
    emit_json_passthrough,
    emit_result,
    format_usage_lines,
)

#: Run-level SSE event types after which the API closes the stream
#: (internal/api/events.go's terminalRunEventTypes) — the client mirrors
#: that as a belt-and-braces stop condition of its own.
_TERMINAL_RUN_EVENTS = {"run.completed", "run.failed", "run.cancelled", "run.bounded"}


def _read_input(spec: str | None) -> object:
    """Read ``--input``'s ``file|-`` argument and parse it as JSON.

    Returns ``None`` when no ``--input`` was given, or the file/stdin content
    is blank — the caller then omits ``input`` from the request body
    entirely rather than sending an explicit JSON ``null``.
    """
    if spec is None:
        return None
    if spec == "-":
        text = sys.stdin.read()
        source = "<stdin>"
    else:
        try:
            text = Path(spec).read_text(encoding="utf-8")
        except OSError as err:
            raise CliError(
                code=EXIT_ENV_ERROR,
                message=f"cannot read input file {spec!r}: {err}",
                remediation="check the path and that the file is readable",
            ) from None
        source = spec
    text = text.strip()
    if not text:
        return None
    try:
        return json.loads(text)
    except json.JSONDecodeError as err:
        raise CliError(
            code=EXIT_USER_ERROR,
            message=f"input from {source} is not valid JSON: {err}",
            remediation="pass a JSON document via --input <file> or --input -",
        ) from None


_RUN_ID_HELP = "The run id."


def _format_run_metadata_lines(run: dict[str, object]) -> list[str]:
    """Render a run's ``name``/``display_hint``/``category`` (task t3), honestly.

    ``name`` is an operator-given display name; when absent, the API may
    supply ``display_hint`` — a truncated, best-effort GUESS derived at read
    time from the run's own input, never something an operator actually
    said. This renders it as ``name: <hint> (derived)`` so it is never
    mistaken for a real name. ``category`` renders only when present.
    """
    lines: list[str] = []
    name = run.get("name")
    hint = run.get("display_hint")
    if name:
        lines.append(f"name: {name}")
    elif hint:
        lines.append(f"name: {hint} (derived)")
    category = run.get("category")
    if category:
        lines.append(f"category: {category}")
    return lines


def cmd_run_create(args: argparse.Namespace) -> int:
    body: dict[str, object] = {"workflow_digest": args.workflow}
    input_value = _read_input(args.input)
    if input_value is not None:
        body["input"] = input_value
    if args.name is not None:
        body["name"] = args.name
    if args.description is not None:
        body["description"] = args.description
    if args.category is not None:
        body["category"] = args.category
    client = client_from_args(args)
    resp = client.request("POST", f"{API_PREFIX}/runs", json_body=body)
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        lines = [
            f"id: {payload.get('id', '')}",
            f"workflow_digest: {payload.get('workflow_digest', '')}",
            f"state: {payload.get('state', '')}",
        ]
        lines.extend(_format_run_metadata_lines(payload))
        lines.append(f"created_at: {payload.get('created_at', '')}")
        usage = payload.get("usage")
        if isinstance(usage, dict):
            lines.extend(format_usage_lines(usage))
        emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_run_list(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request(
        "GET",
        f"{API_PREFIX}/runs",
        query={
            "state": args.state,
            "updated_since": args.updated_since,
            "updated_until": args.updated_until,
            "sort": args.sort,
            "limit": args.limit,
        },
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        items = (resp.payload or {}).get("items") or []
        if not items:
            emit_result("no runs", json_mode=False)
        else:
            lines = []
            for item in items:
                line = (
                    f"{item.get('id', '')}  {item.get('state', '')}  "
                    f"{item.get('workflow_digest', '')}  {item.get('created_at', '')}"
                )
                name = item.get("name")
                hint = item.get("display_hint")
                if name:
                    line += f"  {name}"
                elif hint:
                    line += f"  {hint} (derived)"
                lines.append(line)
            emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_run_get(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request("GET", f"{API_PREFIX}/runs/{args.id}")
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        run = payload.get("run") or {}
        tokens = payload.get("tokens") or []
        node_runs = payload.get("node_runs") or []
        lines = [
            f"id: {run.get('id', '')}",
            f"state: {run.get('state', '')}",
            f"workflow_digest: {run.get('workflow_digest', '')}",
        ]
        lines.extend(_format_run_metadata_lines(run))
        usage = run.get("usage")
        if isinstance(usage, dict):
            lines.extend(format_usage_lines(usage))
        lines.append(f"tokens: {len(tokens)}")
        lines.append(f"node_runs: {len(node_runs)}")
        for nr in node_runs:
            lines.append(
                f"  - {nr.get('node_id', '')}: {nr.get('state', '')} "
                f"(visit {nr.get('visit_count', '')})"
            )
        emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_run_retag(args: argparse.Namespace) -> int:
    """Retag a run's ``category`` — the only field PATCH /v1alpha1/runs/{id} accepts.

    ``name``/``description`` are immutable once a run is created (frame
    decision q4); this verb never sends either.
    """
    client = client_from_args(args)
    resp = client.request(
        "PATCH", f"{API_PREFIX}/runs/{args.id}", json_body={"category": args.category}
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        lines = [
            f"id: {payload.get('id', '')}",
            f"state: {payload.get('state', '')}",
        ]
        lines.extend(_format_run_metadata_lines(payload))
        emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_run_grade(args: argparse.Namespace) -> int:
    """Grade a run against an actor: POST /v1alpha1/runs/{id}/grades.

    The grading actor's registered kind (looked up server-side) decides the
    record's origin and authority: a human ``--as`` actor lands confirmed
    (the human's own opinion, not a claim someone else must ratify); an
    agent ``--as`` actor lands proposed, exactly like any other agent-origin
    record, and reaches confirmed only by later going through ``nodes
    review create``/``commit`` — no special-casing lives in this module
    (spec decision c28).
    """
    body: dict[str, object] = {
        "rating": args.rating,
        "rationale": args.notes,
        "evaluated_actor_id": args.actor,
        "grading_actor_id": args.as_actor,
    }
    if args.node_run_ref is not None:
        body["node_run_ref"] = args.node_run_ref
    if args.attempt_ref is not None:
        body["attempt_ref"] = args.attempt_ref
    if args.category is not None:
        body["category"] = args.category
    client = client_from_args(args)
    resp = client.request("POST", f"{API_PREFIX}/runs/{args.id}/grades", json_body=body)
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        data = payload.get("data") or {}
        origin = payload.get("origin") or {}
        lines = [
            f"id: {payload.get('id', '')}",
            f"authority: {payload.get('authority', '')}",
            f"origin: {origin.get('kind', '')} ({origin.get('actor_id', '')})",
            f"rating: {data.get('rating', '')}",
            f"evaluated_actor_id: {data.get('evaluated_actor_id', '')}",
        ]
        emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_run_cancel(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request("POST", f"{API_PREFIX}/runs/{args.id}/cancel")
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        emit_result(
            f"run {payload.get('id', args.id)} is now {payload.get('state', '')}", json_mode=False
        )
    return 0


def _iter_sse_frames(stream):
    """Parse ``id:``/``event:``/``data:`` lines into frame dicts.

    A blank line ends one frame (the SSE wire format,
    ``internal/api/events.go``'s ``writeSSEEvent``: ``id: N\\nevent:
    T\\ndata: JSON\\n\\n``). Comment lines (leading ``:``) and unknown
    fields are ignored, per the SSE spec.
    """
    frame_id: str | None = None
    frame_event = "message"
    data_lines: list[str] = []
    saw_field = False
    while True:
        raw_line = stream.readline()
        if not raw_line:
            return
        line = raw_line.decode("utf-8", errors="replace").rstrip("\r\n")
        if line == "":
            if saw_field:
                yield {"id": frame_id, "event": frame_event, "data": "\n".join(data_lines)}
            frame_id, frame_event, data_lines, saw_field = None, "message", [], False
            continue
        if line.startswith(":"):
            continue
        if line.startswith("id:"):
            frame_id = line[3:].strip()
            saw_field = True
        elif line.startswith("event:"):
            frame_event = line[6:].strip()
            saw_field = True
        elif line.startswith("data:"):
            data_lines.append(line[5:].strip())
            saw_field = True


def _emit_event_frame(frame: dict, *, json_mode: bool) -> None:
    data_text = frame.get("data") or ""
    try:
        data_obj = json.loads(data_text) if data_text else None
    except json.JSONDecodeError:
        data_obj = data_text
    if json_mode:
        emit_result(
            {"id": frame.get("id"), "event": frame.get("event"), "data": data_obj}, json_mode=True
        )
    else:
        compact = json.dumps(data_obj, separators=(",", ":"), ensure_ascii=False)
        emit_result(f"[{frame.get('id')}] {frame.get('event')}: {compact}", json_mode=False)


def cmd_run_events(args: argparse.Namespace) -> int:
    """Stream a run's committed events until a terminal event or disconnect.

    The API (``internal/api/events.go``) closes the connection once a
    terminal run event has been sent, so this always follows to completion
    — ``--follow`` is accepted (documented in the explain catalog) but is a
    no-op today; there is no meaningful "dump the buffer and exit"
    distinction to make against a stream the server itself terminates.
    """
    client = client_from_args(args)
    json_mode = bool(getattr(args, "json", False))
    stream = client.open_stream("GET", f"{API_PREFIX}/runs/{args.id}/events")
    try:
        for frame in _iter_sse_frames(stream):
            _emit_event_frame(frame, json_mode=json_mode)
            if frame["event"] in _TERMINAL_RUN_EVENTS:
                break
    except OSError as err:
        raise CliError(
            code=EXIT_ENV_ERROR,
            message=f"the nodes API event stream for run {args.id} was interrupted: {err}",
            remediation=f"retry: nodes run events {args.id}",
        ) from None
    finally:
        stream.close()
    return 0


def _bare_noun(args: argparse.Namespace) -> int:
    emit_result(
        "usage: nodes run {create,list,get,cancel,events,retag,grade} ...\n"
        "run 'nodes explain run' for details",
        json_mode=False,
    )
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser("run", help="Thin client for the runs API (create/list/get/cancel/events).")
    p.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    p.set_defaults(func=_bare_noun, json=False)
    noun_sub = p.add_subparsers(dest="run_command", parser_class=type(p))

    create = noun_sub.add_parser("create", help="Create a run of a published workflow.")
    create.add_argument(
        "--workflow", dest="workflow", required=True, help="Workflow version content digest."
    )
    create.add_argument(
        "--input", dest="input", default=None, help="Path to a JSON input file, or '-' for stdin."
    )
    create.add_argument(
        "--name",
        dest="name",
        default=None,
        help="Optional operator-given display name. Set at creation only — immutable afterward.",
    )
    create.add_argument(
        "--description",
        dest="description",
        default=None,
        help="Optional operator-given free-text description. Immutable afterward, like --name.",
    )
    create.add_argument(
        "--category",
        dest="category",
        default=None,
        help="Optional flat category tag (e.g. review, audit). Retaggable via 'run retag'.",
    )
    create.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(create)
    create.set_defaults(func=cmd_run_create)

    listp = noun_sub.add_parser(
        "list", help="List runs, optionally filtered by state and/or an updated_at window."
    )
    listp.add_argument("--state", dest="state", default=None, help="Filter to runs in this state.")
    listp.add_argument(
        "--updated-since",
        dest="updated_since",
        default=None,
        help="Only runs updated at or after this instant (RFC3339).",
    )
    listp.add_argument(
        "--updated-until",
        dest="updated_until",
        default=None,
        help="Only runs updated at or before this instant (RFC3339).",
    )
    listp.add_argument(
        "--sort",
        dest="sort",
        default=None,
        choices=["created_at", "updated_at"],
        help=(
            "Sort column (default: created_at, or updated_at when "
            "--updated-since/--updated-until is set and --sort is omitted)."
        ),
    )
    listp.add_argument("--limit", type=int, default=None, help="Max items to return.")
    listp.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(listp)
    listp.set_defaults(func=cmd_run_list)

    getp = noun_sub.add_parser(
        "get", help="Fetch the Run-view payload: run, tokens, node runs, attempts."
    )
    getp.add_argument("id", help=_RUN_ID_HELP)
    getp.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(getp)
    getp.set_defaults(func=cmd_run_get)

    retag = noun_sub.add_parser(
        "retag", help="Retag a run's category (name/description are immutable)."
    )
    retag.add_argument("id", help=_RUN_ID_HELP)
    retag.add_argument(
        "--category",
        dest="category",
        required=True,
        help="The run's new category tag. Pass an empty string to clear it.",
    )
    retag.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(retag)
    retag.set_defaults(func=cmd_run_retag)

    grade = noun_sub.add_parser(
        "grade", help="Grade a run against an actor: a 1-5 rating plus rationale."
    )
    grade.add_argument("id", help=_RUN_ID_HELP)
    grade.add_argument(
        "--rating",
        dest="rating",
        type=int,
        choices=[1, 2, 3, 4, 5],
        required=True,
        help="Rating 1-5.",
    )
    grade.add_argument(
        "--notes", dest="notes", required=True, help="Free-text rationale for the rating."
    )
    grade.add_argument(
        "--actor",
        dest="actor",
        required=True,
        help="The actor being evaluated (evaluated_actor_id).",
    )
    grade.add_argument(
        "--as",
        dest="as_actor",
        required=True,
        help=(
            "The grading actor (grading_actor_id). Its registered kind decides the "
            "record's origin: a human actor lands confirmed, an agent actor lands proposed."
        ),
    )
    grade.add_argument(
        "--node-run-ref",
        dest="node_run_ref",
        default=None,
        help="Optional: narrow the grade to one node run.",
    )
    grade.add_argument(
        "--attempt-ref",
        dest="attempt_ref",
        default=None,
        help="Optional: narrow the grade to one attempt.",
    )
    grade.add_argument(
        "--category", dest="category", default=None, help="Optional flat category tag on the grade."
    )
    grade.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(grade)
    grade.set_defaults(func=cmd_run_grade)

    cancel = noun_sub.add_parser("cancel", help="Cancel a run.")
    cancel.add_argument("id", help=_RUN_ID_HELP)
    cancel.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(cancel)
    cancel.set_defaults(func=cmd_run_cancel)

    events = noun_sub.add_parser("events", help="Stream a run's committed events (SSE).")
    events.add_argument("id", help=_RUN_ID_HELP)
    events.add_argument(
        "--follow",
        action="store_true",
        help=(
            "Accepted for symmetry with 'tail -f'; the API always streams until a "
            "terminal run event closes the connection, so this is a no-op today."
        ),
    )
    events.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(events)
    events.set_defaults(func=cmd_run_events)
