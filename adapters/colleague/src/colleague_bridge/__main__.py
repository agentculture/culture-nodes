"""CLI entry point: `colleague-bridge` / `python -m colleague_bridge`.

    colleague-bridge [--config PATH] [--host HOST] [--port PORT]

Configuration is otherwise entirely environment-driven (see
`config.py`/README) — the two flags here exist only because "which port did
you start it on" is the one thing an operator most often wants to override
ad hoc without editing a file or exporting an env var.
"""

from __future__ import annotations

import argparse
import json
import logging
import sys

from colleague_bridge import capabilities, preflight, reap, reclaim
from colleague_bridge.config import Config, ConfigError
from colleague_bridge.server import serve_forever


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="colleague-bridge", description=__doc__)
    parser.add_argument(
        "--config",
        default=None,
        help="Path to a JSON config file (or set COLLEAGUE_BRIDGE_CONFIG).",
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
    parser.add_argument(
        "--reap-plan",
        metavar="REPO",
        default=None,
        help=(
            "Print the worktree reaper's plan (task t17) for every worktree of REPO as JSON, "
            "then exit. READ-ONLY by default: it reports reap/preserve_then_reap/refuse/defer "
            "with the reasons and the exact `git worktree remove` an operator would run, and "
            "removes nothing itself."
        ),
    )
    parser.add_argument(
        "--reap-perform",
        action="store_true",
        help=(
            "With --reap-plan, actually reclaim the worktrees the plan cleared. `--force` is "
            "never passed, so a dirty worktree is retained rather than destroyed."
        ),
    )
    parser.add_argument(
        "--reap-assume-idle",
        action="store_true",
        help=(
            "With --reap-plan, state positively that this bridge holds no live session in any "
            "worktree. Without it the standalone CLI has no session registry to consult, so "
            "every candidate DEFERS on session_liveness_unknown."
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

    if args.print_capabilities:
        print(json.dumps(preflight.capability_block(capabilities.host_facts(cfg)), indent=2))
        return 0

    # Read-only unless --reap-perform, and (like --print-capabilities) ahead
    # of the version probe on purpose: reclaiming disk on a host whose CLI
    # install is broken is exactly when an operator needs this.
    if args.reap_plan:
        policy = reap.ReapPolicy.from_config(
            cfg, active_workspaces=() if args.reap_assume_idle else None
        )
        print(
            json.dumps(
                reclaim.sweep(args.reap_plan, policy, perform=args.reap_perform),
                indent=2,
            )
        )
        return 0

    if not (cfg.repo_allowlist or cfg.repo_allowlist_prefixes):
        print(
            "warning: no repo allowlist configured (COLLEAGUE_BRIDGE_REPO_ALLOWLIST or "
            "config file 'repo_allowlist') — every invocation will be refused with 403",
            file=sys.stderr,
        )

    serve_forever(cfg)
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
