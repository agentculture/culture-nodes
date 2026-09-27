"""``nodes chain`` — thin REST client over the declaration-alias (chain) API.

Every verb here is one HTTP call to the Culture Nodes control-plane API
(``api/openapi/openapi.yaml``, ``declarations`` tag): a chain is named via
``POST /v1alpha1/declarations/aliases`` (``alias``) and shown via
``GET /v1alpha1/declarations/{name}`` (``show``) -- the same show route
``nodes decl show`` uses (spec c24: the API has one name -> declaration
resolution surface, so both an ordinary declaration and an alias name are
just the ``{name}`` path segment). No declaration or alias logic lives in
this module (spec decision c28, honesty h24): naming, nesting, and name
resolution all happen server-side (``internal/api/declarations.go``).

Authentication follows :mod:`culture_nodes.cli._commands.decl`: ``alias``
is a write and requires a credential (``$NODES_ACTOR_TOKEN`` or
``$NODES_OP_COOKIE`` -- see that module's docstring); ``show`` is a
read-only GET and never requires one.

``show`` accepts an alias name exactly as it accepts a declaration name
(spec c31/h23) -- this module forwards whatever string is given as the
``{name}`` path segment unchanged, but a name is not always a declaration:
when ``GET /v1alpha1/declarations/{name}`` 404s, ``show`` falls back to
``GET /v1alpha1/declaration-aliases/{name}`` (task t21b, #328) before
giving up, so a pure alias name (one with no declaration of the same name)
resolves too, not just a declaration name. A name that is both a
declaration and an alias always resolves as the declaration -- the
declaration route is tried first and, on success, the alias route is never
reached (the same precedence ``GET .../focus`` documents server-side).
"""

from __future__ import annotations

import argparse

from culture_nodes.api_client import API_PREFIX, add_api_url_argument, client_from_args
from culture_nodes.cli._commands.decl import auth_headers
from culture_nodes.cli._errors import CliError
from culture_nodes.cli._output import JSON_FLAG_HELP, emit_json_passthrough, emit_result

_NAME_HELP = "The chain (alias) name -- or an ordinary declaration name."


def _split_names(raw: str | None) -> list[str]:
    if not raw:
        return []
    return [item.strip() for item in raw.split(",") if item.strip()]


def cmd_chain_alias(args: argparse.Namespace) -> int:
    body: dict[str, object] = {"name": args.name}
    declarations = _split_names(args.declarations)
    if declarations:
        body["declarations"] = declarations
    client = client_from_args(args)
    resp = client.request(
        "POST",
        f"{API_PREFIX}/declarations/aliases",
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
            f"id: {payload.get('id', '')}",
        ]
        members = payload.get("declarations") or []
        lines.append(f"declarations: {', '.join(members) if members else 'none'}")
        emit_result("\n".join(lines), json_mode=False)
    return 0


def _render_chain_alias_text(payload: dict) -> str:
    lines = [
        f"name: {payload.get('name', '')}",
        f"id: {payload.get('id', '')}",
        f"parent: {payload.get('parent', '') or 'none'}",
    ]
    declarations = payload.get("declarations") or []
    lines.append(f"declarations: {', '.join(declarations) if declarations else 'none'}")
    aliases = payload.get("aliases") or []
    lines.append(f"aliases: {', '.join(aliases) if aliases else 'none'}")
    return "\n".join(lines)


def cmd_chain_show(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    headers = auth_headers(args)
    json_mode = bool(getattr(args, "json", False))
    try:
        resp = client.request("GET", f"{API_PREFIX}/declarations/{args.name}", headers=headers)
    except CliError as exc:
        # Only "no such declaration" (404) falls through to the alias route
        # (spec c31/h23: a chain alias resolves by name in every verb that
        # accepts a chain). Any other failure -- a refused credential, a
        # server error, an unreachable API -- is the answer and is relayed
        # as is, never masked by a second request. When neither resolves,
        # the alias route's 404 is the error shown.
        if exc.http_status != 404:
            raise
        resp = client.request(
            "GET", f"{API_PREFIX}/declaration-aliases/{args.name}", headers=headers
        )
        if json_mode:
            emit_json_passthrough(resp.raw)
        else:
            emit_result(_render_chain_alias_text(resp.payload or {}), json_mode=False)
        return 0
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        lines = [
            f"name: {payload.get('name', '')}",
            f"declaration_id: {payload.get('declaration_id', '')}",
            f"version: {payload.get('version', '')}",
            f"digest: {payload.get('digest', '')}",
            f"active: {payload.get('active', False)}",
        ]
        links = payload.get("links") or []
        if not links:
            lines.append("links: none")
        else:
            for link in links:
                lines.append(f"link: {link.get('kind', '')} -> {link.get('to', '')}")
        emit_result("\n".join(lines), json_mode=False)
    return 0


def _bare_noun(args: argparse.Namespace) -> int:
    emit_result(
        "usage: nodes chain {alias,show} ...\nrun 'nodes explain chain' for details",
        json_mode=False,
    )
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser("chain", help="Name and inspect chains (declaration aliases).")
    p.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    p.set_defaults(func=_bare_noun, json=False)
    noun_sub = p.add_subparsers(dest="chain_command", parser_class=type(p))

    alias = noun_sub.add_parser(
        "alias", help="Create a named declaration alias (chain), optionally with initial members."
    )
    alias.add_argument("name", help="The alias (chain) name.")
    alias.add_argument(
        "--declarations",
        default=None,
        help="Comma-separated already-published declaration names to attach as initial members.",
    )
    alias.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(alias)
    alias.set_defaults(func=cmd_chain_alias)

    show = noun_sub.add_parser("show", help="Show one chain (alias) or declaration by name.")
    show.add_argument("name", help=_NAME_HELP)
    show.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(show)
    show.set_defaults(func=cmd_chain_show)
