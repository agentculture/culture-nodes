from __future__ import annotations

import argparse
import json

from . import capabilities, preflight
from .config import Config
from .server import serve_forever


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="jira-bridge")
    parser.add_argument("command", nargs="?", default="serve", choices=["serve"])
    parser.add_argument("--config")
    parser.add_argument("--host")
    parser.add_argument("--port", type=int)
    parser.add_argument("--print-capabilities", action="store_true")
    args = parser.parse_args(argv)
    cfg = Config.load(args.config)
    if args.host:
        cfg.host = args.host
    if args.port is not None:
        cfg.port = args.port
    if args.print_capabilities:
        print(json.dumps(preflight.capability_block(capabilities.host_facts(cfg)), indent=2))
        return 0
    serve_forever(cfg)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
