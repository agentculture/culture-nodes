"""CLI entry point: `human-inbox-bridge` / `python -m human_inbox_bridge`.

    human-inbox-bridge [serve] [--config PATH] [--host HOST] [--port PORT]
    human-inbox-bridge list   [--url URL] [--token TOKEN] [--status STATUS]
    human-inbox-bridge submit <invocation_id> --outcome NAME
                              [--output JSON] [--note TEXT]
                              [--url URL] [--token TOKEN]

`serve` runs the bridge (the default when no subcommand is given),
mirroring the sibling bridges' `__main__`. `list` and `submit` are the
human-facing surface promised by t12: a thin urllib client over the same
HTTP inbox endpoints, so a person at a shell can see what runs are waiting
on them and answer. The token defaults to `HUMAN_INBOX_BRIDGE_AUTH_TOKEN`
so `list`/`submit` on the bridge host need no flags.
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import sys
import urllib.error
import urllib.request

from human_inbox_bridge.config import Config, ConfigError
from human_inbox_bridge.server import serve_forever

_DEFAULT_URL = "http://127.0.0.1:8087"


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="human-inbox-bridge", description=__doc__)
    sub = parser.add_subparsers(dest="command")

    serve = sub.add_parser("serve", help="Run the bridge server (the default).")
    serve.add_argument(
        "--config",
        default=None,
        help="Path to a JSON config file (or set HUMAN_INBOX_BRIDGE_CONFIG).",
    )
    serve.add_argument("--host", default=None, help="Override the bind host.")
    serve.add_argument("--port", type=int, default=None, help="Override the bind port.")
    serve.add_argument("-v", "--verbose", action="store_true", help="Enable debug logging.")

    def client_args(p: argparse.ArgumentParser) -> None:
        p.add_argument("--url", default=_DEFAULT_URL, help=f"Bridge URL (default {_DEFAULT_URL}).")
        p.add_argument(
            "--token",
            default=os.environ.get("HUMAN_INBOX_BRIDGE_AUTH_TOKEN", ""),
            help="Bearer token (default: HUMAN_INBOX_BRIDGE_AUTH_TOKEN).",
        )

    lst = sub.add_parser("list", help="List inbox tasks waiting on a human.")
    client_args(lst)
    lst.add_argument("--status", default="pending", help="Filter by status (default pending).")

    submit = sub.add_parser("submit", help="Submit a result for a pending task.")
    client_args(submit)
    submit.add_argument("invocation_id", help="The task's invocation id (see `list`).")
    submit.add_argument(
        "--outcome", required=True, help="The domain outcome to report (e.g. approved)."
    )
    submit.add_argument(
        "--output", default=None, help="Contract-shaped output as a JSON object string."
    )
    submit.add_argument("--note", default=None, help="Free-text note (lands in the claim record).")

    return parser


def _http_json(url: str, token: str, *, method: str = "GET", body: dict | None = None):
    data = json.dumps(body).encode("utf-8") if body is not None else None
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, data=data, method=method, headers=headers)  # noqa: S310
    with urllib.request.urlopen(req, timeout=30) as resp:  # noqa: S310  # nosec B310 - operator URL
        return resp.status, json.loads(resp.read().decode("utf-8"))


def _cmd_serve(args: argparse.Namespace) -> int:
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    try:
        cfg = Config.load(args.config)
    except ConfigError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
    if args.host:
        cfg.host = args.host
    if args.port is not None:
        cfg.port = args.port
    serve_forever(cfg)
    return 0


def _cmd_list(args: argparse.Namespace) -> int:
    url = f"{args.url.rstrip('/')}/inbox/tasks?status={args.status}"
    try:
        _status, body = _http_json(url, args.token)
    except (urllib.error.URLError, OSError) as exc:
        print(f"error: cannot reach the bridge at {args.url}: {exc}", file=sys.stderr)
        return 1
    tasks = body.get("tasks", [])
    if not tasks:
        print(f"no {args.status} tasks")
        return 0
    for task in tasks:
        print(
            f"{task['invocation_id']}  run={task.get('run_id', '')}  {task.get('created_at', '')}"
        )
        print(f"  {task.get('instruction', '')}")
    return 0


def _cmd_submit(args: argparse.Namespace) -> int:
    submission: dict = {"outcome": args.outcome}
    if args.output is not None:
        try:
            output = json.loads(args.output)
        except ValueError as exc:
            print(f"error: --output is not valid JSON: {exc}", file=sys.stderr)
            return 2
        if not isinstance(output, dict):
            print("error: --output must be a JSON object", file=sys.stderr)
            return 2
        submission["output"] = output
    if args.note is not None:
        submission["note"] = args.note

    url = f"{args.url.rstrip('/')}/inbox/tasks/{args.invocation_id}/submit"
    try:
        _status, body = _http_json(url, args.token, method="POST", body=submission)
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", "replace")
        print(f"error: the bridge refused the submission ({exc.code}): {detail}", file=sys.stderr)
        return 1
    except (urllib.error.URLError, OSError) as exc:
        print(f"error: cannot reach the bridge at {args.url}: {exc}", file=sys.stderr)
        return 1
    print(
        f"{body['invocation_id']}: {body['status']} "
        f"(event {body['event_id']}, sequence {body['sequence']})"
    )
    return 0


def main(argv: list[str] | None = None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    parser = _build_parser()
    args = parser.parse_args(argv or ["serve"])
    if args.command == "list":
        return _cmd_list(args)
    if args.command == "submit":
        return _cmd_submit(args)
    return _cmd_serve(args)


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
