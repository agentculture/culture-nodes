"""``nodes workflow`` — thin REST client over the workflows API.

Every verb here is one HTTP call to the Culture Nodes control-plane API
(``api/openapi/openapi.yaml``, ``workflows`` tag): validate, publish, list,
get. No engine logic lives in this module (spec decision c28) — compiling,
digesting, and storing a workflow all happen server-side; this module only
shapes the request and renders the response.
"""

from __future__ import annotations

import argparse
from pathlib import Path

from culture_nodes.api_client import API_PREFIX, add_api_url_argument, client_from_args
from culture_nodes.cli._errors import EXIT_ENV_ERROR, CliError
from culture_nodes.cli._output import JSON_FLAG_HELP, emit_json_passthrough, emit_result


def _read_workflow_source(path: str) -> tuple[str, str]:
    """Read a workflow file and return ``(source_text, format)``.

    Format detection mirrors the Go CLI's own rule (``internal/compiler/
    source.go``'s ``FormatForPath``): an explicit ``.json`` extension reads
    as JSON, anything else reads as YAML (a superset of JSON, so this is
    always safe).
    """
    try:
        text = Path(path).read_text(encoding="utf-8")
    except OSError as err:
        raise CliError(
            code=EXIT_ENV_ERROR,
            message=f"cannot read workflow file {path!r}: {err}",
            remediation="check the path and that the file is readable",
        ) from None
    fmt = "json" if path.lower().endswith(".json") else "yaml"
    return text, fmt


def _render_validation_text(payload: dict) -> str:
    lines: list[str] = []
    diagnostics = payload.get("diagnostics") or []
    for d in diagnostics:
        path = d.get("path") or "<document>"
        line = f"{d.get('level')} {path} {d.get('code')}: {d.get('message')}"
        hint = d.get("hint")
        if hint:
            line += f" | hint: {hint}"
        lines.append(line)

    error_count = sum(1 for d in diagnostics if d.get("level") == "error")
    warning_count = sum(1 for d in diagnostics if d.get("level") == "warning")
    counts = (
        f"{error_count} error{'s' if error_count != 1 else ''}, "
        f"{warning_count} warning{'s' if warning_count != 1 else ''}"
    )
    if payload.get("valid"):
        lines.append(f"valid: {counts}")
        lines.append(f"digest: {payload.get('digest', '')}")
    else:
        lines.append(f"invalid: {counts}")
    return "\n".join(lines)


def cmd_workflow_validate(args: argparse.Namespace) -> int:
    """Domain outcome, not a technical failure (PRD §3.4): an invalid document
    is reported on stdout with exit 1, never routed through CliError/stderr.
    """
    source, fmt = _read_workflow_source(args.file)
    client = client_from_args(args)
    resp = client.request(
        "POST", f"{API_PREFIX}/workflows/validate", json_body={"format": fmt, "source": source}
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        emit_result(_render_validation_text(resp.payload or {}), json_mode=False)
    return 0 if (resp.payload or {}).get("valid") else 1


def cmd_workflow_publish(args: argparse.Namespace) -> int:
    source, fmt = _read_workflow_source(args.file)
    client = client_from_args(args)
    resp = client.request(
        "POST", f"{API_PREFIX}/workflows", json_body={"format": fmt, "source": source}
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        status_word = "new" if resp.status == 201 else "already published"
        text = (
            f"published: {status_word}\n"
            f"workflow_key: {payload.get('workflow_key', '')}\n"
            f"version: {payload.get('version', '')}\n"
            f"digest: {payload.get('digest', '')}\n"
            f"created_at: {payload.get('created_at', '')}"
        )
        emit_result(text, json_mode=False)
    return 0


def cmd_workflow_list(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request(
        "GET",
        f"{API_PREFIX}/workflows",
        query={"workflow_key": args.workflow_key, "limit": args.limit},
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        items = (resp.payload or {}).get("items") or []
        if not items:
            emit_result("no published workflows", json_mode=False)
            return 0
        lines = [
            f"{item.get('digest', '')}  {item.get('workflow_key', '')}  "
            f"v{item.get('version', '')}  {item.get('created_at', '')}"
            for item in items
        ]
        emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_workflow_get(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request("GET", f"{API_PREFIX}/workflows/{args.digest}")
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        text = (
            f"digest: {payload.get('digest', '')}\n"
            f"workflow_key: {payload.get('workflow_key', '')}\n"
            f"version: {payload.get('version', '')}\n"
            f"source_format: {payload.get('source_format', '')}\n"
            f"created_at: {payload.get('created_at', '')}"
        )
        emit_result(text, json_mode=False)
    return 0


def _bare_noun(args: argparse.Namespace) -> int:
    emit_result(
        "usage: nodes workflow {validate,publish,list,get} ...\n"
        "run 'nodes explain workflow' for details",
        json_mode=False,
    )
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser(
        "workflow", help="Thin client for the workflows API (validate/publish/list/get)."
    )
    p.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    p.set_defaults(func=_bare_noun, json=False)
    noun_sub = p.add_subparsers(dest="workflow_command", parser_class=type(p))

    validate = noun_sub.add_parser(
        "validate", help="Compile a workflow definition and report diagnostics."
    )
    validate.add_argument("file", help="Path to a workflow definition (.yaml or .json).")
    validate.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(validate)
    validate.set_defaults(func=cmd_workflow_validate)

    publish = noun_sub.add_parser(
        "publish", help="Publish a workflow definition as an immutable version."
    )
    publish.add_argument("file", help="Path to a workflow definition (.yaml or .json).")
    publish.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(publish)
    publish.set_defaults(func=cmd_workflow_publish)

    listp = noun_sub.add_parser("list", help="List published workflow versions.")
    listp.add_argument(
        "--workflow-key",
        dest="workflow_key",
        default=None,
        help="Filter to versions of one workflow key.",
    )
    listp.add_argument(
        "--limit", type=int, default=None, help="Max items to return (server default 50, max 500)."
    )
    listp.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(listp)
    listp.set_defaults(func=cmd_workflow_list)

    getp = noun_sub.add_parser("get", help="Fetch one workflow version by content digest.")
    getp.add_argument("digest", help="The workflow version's content digest.")
    getp.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(getp)
    getp.set_defaults(func=cmd_workflow_get)
