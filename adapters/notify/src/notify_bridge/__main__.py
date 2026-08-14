"""CLI entry point: `notify-bridge` / `python -m notify_bridge`.

    notify-bridge [serve] [--config PATH] [--host HOST] [--port PORT]

`serve` runs the bridge (the default when no subcommand is given),
mirroring the sibling bridges' `__main__`. There is no client subcommand
here (unlike `human-inbox-bridge list`/`submit`): a `notify` node's result
is always synchronous, so there is nothing to poll for -- the workflow run
itself is the record of what happened.
"""

from __future__ import annotations

import argparse
import json
import logging
import sys

from notify_bridge import capabilities, preflight
from notify_bridge.config import Config, ConfigError
from notify_bridge.server import serve_forever


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="notify-bridge", description=__doc__)
    sub = parser.add_subparsers(dest="command")

    serve = sub.add_parser("serve", help="Run the bridge server (the default).")
    serve.add_argument(
        "--config",
        default=None,
        help="Path to a JSON config file (or set NOTIFY_BRIDGE_CONFIG).",
    )
    serve.add_argument("--host", default=None, help="Override the bind host.")
    serve.add_argument("--port", type=int, default=None, help="Override the bind port.")
    serve.add_argument("-v", "--verbose", action="store_true", help="Enable debug logging.")
    # On the `serve` subparser rather than at top level because this
    # bridge's CLI is subcommand-shaped (the sibling bridges take bare
    # flags, and their --print-capabilities is bare to match). The document
    # it prints is identical on all four.
    serve.add_argument(
        "--print-capabilities",
        action="store_true",
        help=(
            "Print this host's preflight capability surface (issue #67) as the JSON an actor "
            "registration carries in `capabilities`, then exit without serving. The running "
            "bridge serves the same document at GET /v1/capabilities; this flag is for "
            "registering an actor before its bridge has ever started."
        ),
    )

    return parser


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
    if args.print_capabilities:
        print(json.dumps(preflight.capability_block(capabilities.host_facts(cfg)), indent=2))
        return 0
    serve_forever(cfg)
    return 0


def main(argv: list[str] | None = None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    parser = _build_parser()
    args = parser.parse_args(argv or ["serve"])
    return _cmd_serve(args)


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
