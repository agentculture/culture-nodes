from __future__ import annotations

import argparse

from .config import Config
from .server import serve_forever


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="jira-bridge")
    parser.add_argument("command", nargs="?", default="serve", choices=["serve"])
    parser.add_argument("--config")
    parser.add_argument("--host")
    parser.add_argument("--port", type=int)
    args = parser.parse_args(argv)
    cfg = Config.load(args.config)
    if args.host:
        cfg.host = args.host
    if args.port is not None:
        cfg.port = args.port
    serve_forever(cfg)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
