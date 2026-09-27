"""``nodes decl`` — thin REST client over the declarations API.

Every verb here is one HTTP call to the Culture Nodes control-plane API
(``api/openapi/openapi.yaml``, ``declarations`` tag): declare, validate,
link, show, list, focus, activate. No declaration logic lives in this
module (spec decision c28, honesty h24) — parsing, schema-checking, linking,
the one-level-deep activation root of trust (ADR 0014), and the focus BFS
all happen server-side (``internal/api/declarations.go``,
``internal/api/declgraph.go``); this module only shapes the request and
renders the response.

Authentication (task t21, spec c66/h43): a write route (``declare``,
``link``, ``activate``) requires an authenticated principal — either a
human session (Cloudflare Access) or a registered agent actor's own
bearer (ADR 0014's one-level-deep root of trust; ``declarationPrincipal``
in ``internal/api/declarations.go``). This module sends whichever
credential the environment provides: ``$NODES_ACTOR_TOKEN`` (an agent's own
bearer, the same variable ``examples/spec-chain-lane/post_frame.py`` reads)
as ``Authorization: Bearer <token>``, or ``$NODES_OP_COOKIE`` (a human's
Cloudflare Access session, the same variable
``.claude/skills/nodes-operator/scripts/nodes-op.sh`` reads) as
``Cookie: CF_Authorization=<value>``. Both credential kinds hit the exact
same CLI verb and API route (h43) -- only activation authority differs
server-side (c33), never the surface. Read-only verbs (``validate``,
``show``, ``list``, ``focus``) never require a credential, but send one if
present.

``show`` and ``focus`` both take a ``name`` argument that may name either a
declaration or a declaration alias (spec c31/h23): this module never
resolves or distinguishes the two -- it forwards whatever string is given
as the ``{name}`` path segment, exactly as it does for an ordinary
declaration name, so any name the API resolves is honoured.
"""

from __future__ import annotations

import argparse
import os
from pathlib import Path

from culture_nodes.api_client import API_PREFIX, add_api_url_argument, client_from_args
from culture_nodes.cli._errors import EXIT_ENV_ERROR, CliError
from culture_nodes.cli._output import JSON_FLAG_HELP, emit_json_passthrough, emit_result

#: Env var carrying an agent actor's own bearer token (checked first).
#: This is an env var *name*, not a credential value -- bandit's B105
#: heuristic flags it anyway because the string contains "TOKEN".
ENV_ACTOR_TOKEN = "NODES_ACTOR_TOKEN"  # nosec B105
#: Env var carrying a human's Cloudflare Access session cookie value
#: (checked second, only when no actor token is present).
ENV_ACCESS_COOKIE = "NODES_OP_COOKIE"

_NAME_HELP = "The declaration's name -- or a declaration alias's name (spec c31/h23)."


def auth_headers(args: argparse.Namespace) -> dict[str, str]:
    """Resolve whichever credential the environment provides.

    ``$NODES_ACTOR_TOKEN`` (an agent's own bearer) is checked first;
    ``$NODES_OP_COOKIE`` (a human's Cloudflare Access session) second.
    Environment only, never a flag: a credential on argv is visible in
    ``ps`` and shell history. Neither is required: an
    unauthenticated write is refused by the API itself (401), which this
    client relays verbatim as a CliError -- this function never raises.
    Shared with :mod:`culture_nodes.cli._commands.chain`.
    """
    token = os.environ.get(ENV_ACTOR_TOKEN)
    if token:
        return {"Authorization": f"Bearer {token}"}
    cookie = os.environ.get(ENV_ACCESS_COOKIE)
    if cookie:
        return {"Cookie": f"CF_Authorization={cookie}"}
    return {}


def _read_declaration_source(path: str) -> tuple[str, str]:
    """Read a declaration file and return ``(source_text, format)``.

    Format detection mirrors ``workflow.py``'s own rule: an explicit
    ``.json`` extension reads as JSON, anything else reads as YAML (a
    superset of JSON, so this is always safe).
    """
    try:
        text = Path(path).read_text(encoding="utf-8")
    except OSError as err:
        raise CliError(
            code=EXIT_ENV_ERROR,
            message=f"cannot read declaration file {path!r}: {err}",
            remediation="check the path and that the file is readable",
        ) from None
    fmt = "json" if path.lower().endswith(".json") else "yaml"
    return text, fmt


def _render_validation_text(payload: dict) -> str:
    lines: list[str] = []
    for diag in payload.get("diagnostics") or []:
        lines.append(str(diag))
    for warning in payload.get("warnings") or []:
        lines.append(f"warning: {warning}")
    if payload.get("valid"):
        lines.append("valid: true")
        if payload.get("digest"):
            lines.append(f"digest: {payload.get('digest', '')}")
    else:
        lines.append("valid: false")
    return "\n".join(lines) if lines else "valid: false"


def cmd_decl_validate(args: argparse.Namespace) -> int:
    """Domain outcome, not a technical failure (matches ``workflow validate``):
    an invalid document is reported on stdout with exit 1, never routed
    through CliError/stderr.
    """
    source, fmt = _read_declaration_source(args.file)
    client = client_from_args(args)
    resp = client.request(
        "POST",
        f"{API_PREFIX}/declarations/validate",
        json_body={"format": fmt, "source": source},
        headers=auth_headers(args),
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        emit_result(_render_validation_text(resp.payload or {}), json_mode=False)
    return 0 if (resp.payload or {}).get("valid") else 1


def _render_version(payload: dict) -> str:
    lines = [
        f"name: {payload.get('name', '')}",
        f"declaration_id: {payload.get('declaration_id', '')}",
        f"version: {payload.get('version', '')}",
        f"digest: {payload.get('digest', '')}",
        f"author: {payload.get('author', '')}",
        f"created_at: {payload.get('created_at', '')}",
    ]
    for warning in payload.get("warnings") or []:
        lines.append(f"warning: {warning}")
    return "\n".join(lines)


def cmd_decl_declare(args: argparse.Namespace) -> int:
    source, fmt = _read_declaration_source(args.file)
    client = client_from_args(args)
    resp = client.request(
        "POST",
        f"{API_PREFIX}/declarations",
        json_body={"format": fmt, "source": source},
        headers=auth_headers(args),
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        emit_result(_render_version(resp.payload or {}), json_mode=False)
    return 0


def cmd_decl_list(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request("GET", f"{API_PREFIX}/declarations", headers=auth_headers(args))
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        items = (resp.payload or {}).get("items") or []
        if not items:
            emit_result("no published declarations", json_mode=False)
        else:
            lines = [
                f"{item.get('name', '')}  v{item.get('version', '')}  "
                f"{item.get('digest', '')}  {item.get('created_at', '')}"
                for item in items
            ]
            emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_decl_show(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request(
        "GET", f"{API_PREFIX}/declarations/{args.name}", headers=auth_headers(args)
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        lines = [
            f"name: {payload.get('name', '')}",
            f"declaration_id: {payload.get('declaration_id', '')}",
            f"version: {payload.get('version', '')}",
            f"digest: {payload.get('digest', '')}",
            f"author: {payload.get('author', '')}",
            f"created_at: {payload.get('created_at', '')}",
            f"active: {payload.get('active', False)}",
        ]
        if payload.get("active"):
            lines.append(f"active_version_id: {payload.get('active_version_id', '')}")
        links = payload.get("links") or []
        if not links:
            lines.append("links: none")
        else:
            for link in links:
                lines.append(f"link: {link.get('kind', '')} -> {link.get('to', '')}")
        emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_decl_link(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request(
        "POST",
        f"{API_PREFIX}/declarations/{args.name}/links",
        json_body={"to": args.to, "kind": args.kind},
        headers=auth_headers(args),
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        kind = payload.get("kind", args.kind)
        to = payload.get("to", args.to)
        emit_result(f"linked: {args.name} --{kind}--> {to}", json_mode=False)
    return 0


def cmd_decl_focus(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request(
        "GET",
        f"{API_PREFIX}/declarations/{args.name}/focus",
        query={"distance": args.distance, "direction": args.direction, "link": args.link},
        headers=auth_headers(args),
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        lines = [
            f"center: {payload.get('center', '')}",
            f"distance: {payload.get('distance', 0)}",
            f"direction: {payload.get('direction', '')}",
            f"link: {payload.get('link', '')}",
        ]
        declarations = payload.get("declarations") or []
        if not declarations:
            lines.append("declarations: none")
        else:
            for d in declarations:
                lines.append(
                    f"[{d.get('distance', 0)}] {d.get('name', '')}  v{d.get('version', '')}"
                )
        for link in payload.get("links") or []:
            lines.append(
                f"link: {link.get('from', '')} --{link.get('kind', '')}--> {link.get('to', '')}"
            )
        emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_decl_activate(args: argparse.Namespace) -> int:
    body: dict[str, object] = {}
    if args.version_id:
        body["version_id"] = args.version_id
    client = client_from_args(args)
    resp = client.request(
        "POST",
        f"{API_PREFIX}/declarations/{args.name}/activate",
        json_body=body,
        headers=auth_headers(args),
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        lines = [
            f"name: {payload.get('name', '')}",
            f"version_id: {payload.get('version_id', '')}",
            f"active: {payload.get('active', False)}",
        ]
        emit_result("\n".join(lines), json_mode=False)
    return 0


def _bare_noun(args: argparse.Namespace) -> int:
    emit_result(
        "usage: nodes decl {declare,validate,link,show,list,focus,activate} ...\n"
        "run 'nodes explain decl' for details",
        json_mode=False,
    )
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser(
        "decl", help="Declare, link, activate and inspect trigger-condition-action declarations."
    )
    p.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    p.set_defaults(func=_bare_noun, json=False)
    noun_sub = p.add_subparsers(dest="decl_command", parser_class=type(p))

    declare = noun_sub.add_parser(
        "declare", help="Publish a declaration as a new (or repeat) version."
    )
    declare.add_argument("file", help="Path to a declaration definition (.yaml or .json).")
    declare.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(declare)
    declare.set_defaults(func=cmd_decl_declare)

    validate = noun_sub.add_parser(
        "validate", help="Parse and schema-check a declaration without publishing it."
    )
    validate.add_argument("file", help="Path to a declaration definition (.yaml or .json).")
    validate.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(validate)
    validate.set_defaults(func=cmd_decl_validate)

    link = noun_sub.add_parser(
        "link", help="Record a must/can ordering relation to another declaration."
    )
    link.add_argument("name", help="The declaration this link is FROM.")
    link.add_argument("--to", required=True, help="The declaration this one depends on.")
    link.add_argument(
        "--kind", required=True, choices=["must", "can"], help="Ordering relation kind."
    )
    link.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(link)
    link.set_defaults(func=cmd_decl_link)

    show = noun_sub.add_parser(
        "show", help="Show one declaration's newest version, links and activation status."
    )
    show.add_argument("name", help=_NAME_HELP)
    show.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(show)
    show.set_defaults(func=cmd_decl_show)

    listp = noun_sub.add_parser("list", help="List every declaration's newest published version.")
    listp.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(listp)
    listp.set_defaults(func=cmd_decl_list)

    focus = noun_sub.add_parser(
        "focus", help="A distance-N neighborhood of the one global declaration graph."
    )
    focus.add_argument("name", help=_NAME_HELP)
    focus.add_argument(
        "--distance", type=int, default=0, help="Non-negative hop count (default 0)."
    )
    focus.add_argument(
        "--direction",
        choices=["both", "up", "down"],
        default="both",
        help="Which side of the graph to traverse.",
    )
    focus.add_argument(
        "--link",
        choices=["both", "must", "can"],
        default="both",
        help="Which link kind to traverse.",
    )
    focus.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(focus)
    focus.set_defaults(func=cmd_decl_focus)

    activate = noun_sub.add_parser("activate", help="Activate a declaration version.")
    activate.add_argument("name", help="The declaration's name.")
    activate.add_argument(
        "--version-id",
        dest="version_id",
        default=None,
        help="Defaults to the newest published version.",
    )
    activate.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(activate)
    activate.set_defaults(func=cmd_decl_activate)
