"""CLI entry point: `claude-code-bridge` / `python -m claude_code_bridge`.

    claude-code-bridge [--config PATH] [--host HOST] [--port PORT]

Configuration is otherwise entirely environment-driven (see
`config.py`/README) — mirrors `colleague_bridge.__main__` field for field.
"""

from __future__ import annotations

import argparse
import json
import logging
import sys

from claude_code_bridge import capabilities, claude_cli, preflight
from claude_code_bridge.config import Config, ConfigError
from claude_code_bridge.server import serve_forever


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="claude-code-bridge", description=__doc__)
    parser.add_argument(
        "--config",
        default=None,
        help="Path to a JSON config file (or set CLAUDE_CODE_BRIDGE_CONFIG).",
    )
    parser.add_argument("--host", default=None, help="Override the bind host.")
    parser.add_argument("--port", type=int, default=None, help="Override the bind port.")
    parser.add_argument("-v", "--verbose", action="store_true", help="Enable debug logging.")
    parser.add_argument(
        "--print-capabilities",
        action="store_true",
        help=(
            "Print this host's preflight capability surface (issue #67) as the JSON an actor "
            "registration carries in `capabilities`, then exit without serving. The running "
            "bridge serves the same document at GET /v1/capabilities; this flag is for "
            "registering an actor before its bridge has ever started."
        ),
    )
    args = parser.parse_args(argv)

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

    # Before the version probe below on purpose: the surface describes the
    # host, and an operator registering this actor must be able to read it
    # off a machine whose claude install is not yet in shape.
    if args.print_capabilities:
        print(json.dumps(preflight.capability_block(capabilities.host_facts(cfg)), indent=2))
        return 0

    if not cfg.repo_allowlist:
        print(
            "warning: no repo allowlist configured (CLAUDE_CODE_BRIDGE_REPO_ALLOWLIST or "
            "config file 'repo_allowlist') — every invocation will be refused with 403",
            file=sys.stderr,
        )

    # Fail fast and loud at startup: an operator who mis-pinned the version
    # gate, or whose claude install is older than this bridge requires,
    # should learn that from a refusal to start, not from every invocation
    # answering 503 one at a time.
    try:
        claude_cli.ensure_supported_version(cfg)
    except claude_cli.ClaudeVersionProbeError as exc:
        print(f"error: could not verify the claude CLI version: {exc}", file=sys.stderr)
        return 2
    except claude_cli.UnsupportedClaudeVersionError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    serve_forever(cfg)
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
