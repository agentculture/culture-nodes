"""``nodes run`` — thin REST client over the runs API.

Every verb here is one HTTP call to the Culture Nodes control-plane API
(``api/openapi/openapi.yaml``, ``runs``/``events`` tags): create, list, get,
cancel, events. No engine logic lives in this module (spec decision c28).
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from culture_nodes.api_client import API_PREFIX, add_api_url_argument, client_from_args
from culture_nodes.cli._errors import EXIT_ENV_ERROR, EXIT_USER_ERROR, CliError
from culture_nodes.cli._output import emit_json_passthrough, emit_result

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


def cmd_run_create(args: argparse.Namespace) -> int:
    body: dict[str, object] = {"workflow_digest": args.workflow}
    input_value = _read_input(args.input)
    if input_value is not None:
        body["input"] = input_value
    client = client_from_args(args)
    resp = client.request("POST", f"{API_PREFIX}/runs", json_body=body)
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
        return 0
    payload = resp.payload or {}
    text = (
        f"id: {payload.get('id', '')}\n"
        f"workflow_digest: {payload.get('workflow_digest', '')}\n"
        f"state: {payload.get('state', '')}\n"
        f"created_at: {payload.get('created_at', '')}"
    )
    emit_result(text, json_mode=False)
    return 0


def cmd_run_list(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request(
        "GET", f"{API_PREFIX}/runs", query={"state": args.state, "limit": args.limit}
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
        return 0
    items = (resp.payload or {}).get("items") or []
    if not items:
        emit_result("no runs", json_mode=False)
        return 0
    lines = [
        f"{item.get('id', '')}  {item.get('state', '')}  "
        f"{item.get('workflow_digest', '')}  {item.get('created_at', '')}"
        for item in items
    ]
    emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_run_get(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request("GET", f"{API_PREFIX}/runs/{args.id}")
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
        return 0
    payload = resp.payload or {}
    run = payload.get("run") or {}
    tokens = payload.get("tokens") or []
    node_runs = payload.get("node_runs") or []
    lines = [
        f"id: {run.get('id', '')}",
        f"state: {run.get('state', '')}",
        f"workflow_digest: {run.get('workflow_digest', '')}",
        f"tokens: {len(tokens)}",
        f"node_runs: {len(node_runs)}",
    ]
    for nr in node_runs:
        lines.append(
            f"  - {nr.get('node_id', '')}: {nr.get('state', '')} "
            f"(visit {nr.get('visit_count', '')})"
        )
    emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_run_cancel(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request("POST", f"{API_PREFIX}/runs/{args.id}/cancel")
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
        return 0
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
    except (TimeoutError, OSError) as err:
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
        "usage: nodes run {create,list,get,cancel,events} ...\n"
        "run 'nodes explain run' for details",
        json_mode=False,
    )
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser("run", help="Thin client for the runs API (create/list/get/cancel/events).")
    p.add_argument("--json", action="store_true", help="Emit structured JSON.")
    p.set_defaults(func=_bare_noun, json=False)
    noun_sub = p.add_subparsers(dest="run_command", parser_class=type(p))

    create = noun_sub.add_parser("create", help="Create a run of a published workflow.")
    create.add_argument(
        "--workflow", dest="workflow", required=True, help="Workflow version content digest."
    )
    create.add_argument(
        "--input", dest="input", default=None, help="Path to a JSON input file, or '-' for stdin."
    )
    create.add_argument("--json", action="store_true", help="Emit structured JSON.")
    add_api_url_argument(create)
    create.set_defaults(func=cmd_run_create)

    listp = noun_sub.add_parser("list", help="List runs, optionally filtered by state.")
    listp.add_argument("--state", dest="state", default=None, help="Filter to runs in this state.")
    listp.add_argument("--limit", type=int, default=None, help="Max items to return.")
    listp.add_argument("--json", action="store_true", help="Emit structured JSON.")
    add_api_url_argument(listp)
    listp.set_defaults(func=cmd_run_list)

    getp = noun_sub.add_parser(
        "get", help="Fetch the Run-view payload: run, tokens, node runs, attempts."
    )
    getp.add_argument("id", help="The run id.")
    getp.add_argument("--json", action="store_true", help="Emit structured JSON.")
    add_api_url_argument(getp)
    getp.set_defaults(func=cmd_run_get)

    cancel = noun_sub.add_parser("cancel", help="Cancel a run.")
    cancel.add_argument("id", help="The run id.")
    cancel.add_argument("--json", action="store_true", help="Emit structured JSON.")
    add_api_url_argument(cancel)
    cancel.set_defaults(func=cmd_run_cancel)

    events = noun_sub.add_parser("events", help="Stream a run's committed events (SSE).")
    events.add_argument("id", help="The run id.")
    events.add_argument(
        "--follow",
        action="store_true",
        help=(
            "Accepted for symmetry with 'tail -f'; the API always streams until a "
            "terminal run event closes the connection, so this is a no-op today."
        ),
    )
    events.add_argument("--json", action="store_true", help="Emit structured JSON.")
    add_api_url_argument(events)
    events.set_defaults(func=cmd_run_events)
