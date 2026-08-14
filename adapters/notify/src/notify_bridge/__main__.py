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
import logging
import sys

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
    serve_forever(cfg)
    return 0


def main(argv: list[str] | None = None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    parser = _build_parser()
    args = parser.parse_args(argv or ["serve"])
    return _cmd_serve(args)


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
