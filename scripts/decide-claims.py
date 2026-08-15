#!/usr/bin/env python3
"""Decide a run's proposed ledger records through the review surface.

PRD §10.4: an agent may only create ``proposed`` records, and no actor
promotes its own proposal. A human decides. This is the operator's side of
that contract — it creates a review request over the records still proposed
on a run, then commits it with a verdict, so the decision lands as a first
-class ledger event rather than as an operator's silence.

    scripts/decide-claims.py <run-id> --verdict confirm --why "<reason>"
    scripts/decide-claims.py <run-id> --dry-run

``--why`` is mandatory on a real decision: a confirmation with no stated
reason is the failure mode the authority model exists to prevent, because it
is indistinguishable from not having read the claim.

Exit codes: 0 decided (or nothing to decide); 1 refused; 2 the control plane
could not be reached.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

DEFAULT_API = "http://192.168.1.146:18080"


def api_base() -> str:
    if os.environ.get("NODES_API_URL"):
        return os.environ["NODES_API_URL"].rstrip("/")
    env = Path.home() / ".culture-nodes" / "operator.env"
    if env.is_file():
        for line in env.read_text(encoding="utf-8").splitlines():
            key, _, value = line.partition("=")
            if key.strip() == "NODES_API_URL":
                return value.strip().strip("\"'").rstrip("/")
    return DEFAULT_API


def request(url: str, payload: dict | None = None) -> dict:
    data = None if payload is None else json.dumps(payload).encode()
    headers = {"content-type": "application/json"} if data else {}
    req = urllib.request.Request(url, data=data, headers=headers)  # noqa: S310
    try:
        with urllib.request.urlopen(req, timeout=30) as response:  # noqa: S310
            body = response.read()
            return json.loads(body) if body else {}
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode(errors="replace")[:400]
        print(f"decide-claims: HTTP {exc.code} from {url}: {detail}", file=sys.stderr)
        raise SystemExit(1) from exc
    except OSError as exc:
        print(f"decide-claims: cannot reach the control plane: {exc}", file=sys.stderr)
        raise SystemExit(2) from exc


def first_human_actor(base: str) -> str | None:
    """The reviewer defaults to a registered human, mirroring `nodes-op grade`."""
    actors = request(f"{base}/v1alpha1/actors").get("items", [])
    for actor in actors:
        if actor.get("kind") == "human":
            return actor.get("id")
    return None


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("run_id")
    parser.add_argument("--verdict", choices=("confirm", "reject"))
    parser.add_argument("--why", help="why the claim was confirmed or rejected (mandatory)")
    parser.add_argument("--reviewer", help="reviewer actor id (defaults to a registered human)")
    parser.add_argument("--dry-run", action="store_true", help="list what would be decided")
    args = parser.parse_args()

    base = api_base()
    ledger = request(f"{base}/v1alpha1/runs/{args.run_id}/ledger")
    version = ledger.get("ledger_version")
    proposed = [i["id"] for i in ledger.get("items", []) if i.get("authority") == "proposed"]

    if not proposed:
        print(f"decide-claims: run {args.run_id} has no proposed records; nothing to decide")
        return 0

    if args.dry_run:
        print(f"decide-claims: {len(proposed)} proposed record(s) at ledger_version {version}:")
        for record_id in proposed:
            print(f"  {record_id}")
        return 0

    if not args.verdict or not args.why:
        print(
            "decide-claims: --verdict and --why are both mandatory on a real decision; "
            "a confirmation with no stated reason cannot be told apart from an unread one",
            file=sys.stderr,
        )
        return 1

    reviewer = args.reviewer or first_human_actor(base)
    if not reviewer:
        print(
            "decide-claims: no reviewer given and no registered human actor found; "
            "pass --reviewer <actor-id> rather than letting an agent decide its own claim",
            file=sys.stderr,
        )
        return 1

    review = request(
        f"{base}/v1alpha1/runs/{args.run_id}/reviews",
        {"record_ids": proposed, "ledger_version": version, "reviewer_actor_id": reviewer},
    )
    review_id = review.get("id")
    request(
        f"{base}/v1alpha1/reviews/{review_id}/commit",
        {
            "decisions": {record_id: args.verdict for record_id in proposed},
            "expected_ledger_version": version,
        },
    )
    print(
        f"decide-claims: {len(proposed)} record(s) on run {args.run_id} {args.verdict}ed "
        f"by {reviewer} via review {review_id}"
    )
    print(f"  reason: {args.why}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
