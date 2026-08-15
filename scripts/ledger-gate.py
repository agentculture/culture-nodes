#!/usr/bin/env python3
"""Stage exit gate: no claim from this cycle's runs is still undecided.

The gate is the query's output, not anyone's recollection (task t8). It asks
the control plane which records are still awaiting a decision — for the runs
named in a cycle manifest, or (``--all-runs``) for every run in the namespace
— and reports each one.

An agent's completion claim lands ``proposed`` by construction (PRD §10.4) —
it is a claim that work happened, not evidence that it did. Leaving one
undecided at a stage boundary means the stage advanced on an unread report.
So this exits non-zero while any remain, and the fix is to decide them
through the review surface (``scripts/decide-claims.py``), never to relax the
gate.

Note what the query is NOT. Ledger records are immutable, so confirming a
claim does not flip it to ``confirmed`` — it appends a ``review`` record
whose ``reviewed_refs`` name the claim it decided. A gate phrased as "zero
records at authority proposed" therefore can never pass, no matter how
diligently every claim is read; it would be a gate that fails on correct
behaviour. The decidable question is whether each proposed record has been
*reviewed*.

Since task t30 that question is the control plane's to answer: GET
/v1alpha1/pending-decisions applies exactly this rule (proposed, no review
record naming it, not superseded) server-side, so the gate and the decision
surface can no longer disagree about what "undecided" means. ``--all-runs``
asks it without a manifest at all, which is the version of this gate that
needs no hand-maintained list of run ids.

    scripts/ledger-gate.py                      # read docs/triage/cycle-runs.txt
    scripts/ledger-gate.py --all-runs           # every run, no manifest needed
    scripts/ledger-gate.py --run 01M0... --run 01M0...
    scripts/ledger-gate.py --json

Exit codes: 0 every claim decided; 1 at least one still proposed;
2 the control plane could not be reached (the gate could not run at all,
which is not the same as passing).
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
MANIFEST = ROOT / "docs" / "triage" / "cycle-runs.txt"
DEFAULT_API = "http://192.168.1.146:18080"


def api_base() -> str:
    """Resolve the control plane the same way the nodes-operator skill does."""
    if os.environ.get("NODES_API_URL"):
        return os.environ["NODES_API_URL"].rstrip("/")
    env = Path.home() / ".culture-nodes" / "operator.env"
    if env.is_file():
        for line in env.read_text(encoding="utf-8").splitlines():
            key, _, value = line.partition("=")
            if key.strip() == "NODES_API_URL":
                return value.strip().strip("\"'").rstrip("/")
    return DEFAULT_API


def manifest_runs() -> list[str]:
    if not MANIFEST.is_file():
        raise SystemExit(
            f"ledger-gate: no run manifest at {MANIFEST.relative_to(ROOT)} and no --run given; "
            "the gate cannot decide what 'this cycle' means"
        )
    runs = []
    for line in MANIFEST.read_text(encoding="utf-8").splitlines():
        line = line.split("#", 1)[0].strip()
        if line:
            runs.append(line.split()[0])
    return runs


def fetch_pending(base: str, run_id: str | None = None) -> list[dict]:
    """Run groups still awaiting a decision, per the control plane's own rule."""
    url = f"{base}/v1alpha1/pending-decisions"
    if run_id:
        url += f"?run_id={run_id}"
    try:
        with urllib.request.urlopen(url, timeout=20) as response:  # noqa: S310
            return json.load(response).get("items", [])
    except urllib.error.HTTPError as exc:
        raise SystemExit(f"ledger-gate: HTTP {exc.code} from {url}") from exc
    except OSError as exc:
        # Distinguished from "gate failed": we never got to ask the question.
        print(f"ledger-gate: cannot reach the control plane at {base}: {exc}", file=sys.stderr)
        raise SystemExit(2) from exc


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--run", action="append", default=[], metavar="RUN_ID")
    parser.add_argument(
        "--all-runs",
        action="store_true",
        help="ask across every run in the namespace instead of reading the cycle manifest",
    )
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args()

    base = api_base()

    undecided: list[dict] = []
    if args.all_runs:
        runs = []
        groups = fetch_pending(base)
    else:
        runs = args.run or manifest_runs()
        groups = []
        for run_id in runs:
            groups.extend(fetch_pending(base, run_id))
    for group in groups:
        if group["run_id"] not in runs:
            runs.append(group["run_id"])
        for record in group.get("records", []):
            undecided.append(
                {
                    "run_id": group["run_id"],
                    "record_id": record.get("id"),
                    "record_type": record.get("record_type"),
                }
            )

    if args.json:
        json.dump(
            {"runs": runs, "undecided": undecided, "passed": not undecided},
            sys.stdout,
            indent=2,
        )
        print()
    elif undecided:
        print(
            f"ledger-gate: {len(undecided)} record(s) across {len(runs)} run(s) "
            "are still awaiting a decision:",
            file=sys.stderr,
        )
        for row in undecided:
            print(f"  {row['run_id']}  {row['record_id']}  ({row['record_type']})", file=sys.stderr)
        print(
            "hint: decide each through the review surface — "
            "scripts/decide-claims.py <run-id> --verdict confirm|reject --why '<reason>'",
            file=sys.stderr,
        )
    elif args.all_runs:
        print("ledger-gate: no run in the namespace has a record awaiting a decision")
    else:
        print(f"ledger-gate: {len(runs)} run(s) checked, nothing awaiting a decision")

    return 1 if undecided else 0


if __name__ == "__main__":
    raise SystemExit(main())
