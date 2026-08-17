#!/usr/bin/env python3
"""Render the cycle's sole stage-1 verification brief shape."""

from __future__ import annotations

import argparse
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
TEMPLATE = ROOT / "docs" / "triage" / "verification-brief.template"


def one_line(parser: argparse.ArgumentParser, name: str, value: str) -> str:
    if not value.strip() or "\n" in value or "\r" in value:
        parser.error(f"{name} must be one non-empty line")
    return value


def main() -> None:
    parser = argparse.ArgumentParser(description="render a read-only stage-1 verification brief")
    parser.add_argument("issue_number", type=int)
    parser.add_argument("claim")
    parser.add_argument("evidence_form")
    args = parser.parse_args()
    if args.issue_number <= 0:
        parser.error("issue_number must be positive")

    values = {
        "issue_number": str(args.issue_number),
        "claim": one_line(parser, "claim", args.claim),
        "evidence_form": one_line(parser, "evidence_form", args.evidence_form),
    }
    print(TEMPLATE.read_text(encoding="utf-8").format_map(values), end="")


if __name__ == "__main__":
    main()
