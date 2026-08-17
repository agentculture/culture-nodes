#!/usr/bin/env python3
"""Check that the recorded stage-1 lanes partition bucket A exactly."""

from __future__ import annotations

import csv
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
LANES = {
    "spark-go-shell": {8, 9, 13, 28, 48},
    "spark-claude-python": {10, 62, 98},
    "spark-network-api": {54, 61},
}


def main() -> None:
    rows = csv.DictReader((ROOT / "docs/triage/dispositions.csv").open())
    bucket_a = {int(row["issue"]) for row in rows if row["bucket"] == "verify-then-close"}
    assigned: set[int] = set()
    for name, issues in LANES.items():
        overlap = assigned & issues
        if overlap:
            raise SystemExit(f"duplicate lane assignment in {name}: {sorted(overlap)}")
        assigned |= issues
    if assigned != bucket_a:
        raise SystemExit(
            f"lane mismatch: missing={sorted(bucket_a - assigned)} extra={sorted(assigned - bucket_a)}"
        )
    print(f"stage-1 lanes partition all {len(bucket_a)} bucket-A issues exactly once")


if __name__ == "__main__":
    main()
