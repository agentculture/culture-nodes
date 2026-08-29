#!/usr/bin/env python3
"""Post a frame snapshot to the control plane after a devague move (task
t13, issue #199 / #230; the projection is plan task t9's
POST /v1alpha1/tickets/{id}/frame, append-only and versioned).

The session on the developer lane runs this after every move it commits,
so the ticket page shows the frame as it stands — not a session's summary
of it. The snapshot is the frame file devague itself wrote
(`.devague/frames/<slug>.json`), sent byte-for-byte as the `frame` object:
GET /v1alpha1/tickets/{id} then returns claim states equal to what devague
recorded (t9's own acceptance).

Custody of the decision token: the route is decision-token guarded, and
the token is a GRANTED env value on the lane (`NODES_HUMAN_DECISION_TOKEN`
in the developer bridge's `claude_env`) — never a workflow literal, never
an argv, never printed. This script reads it from the environment only
and exits 2, naming the variable, when it is absent.

Stdlib-only, like every helper beside a workflow in examples/.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

TOKEN_ENV = "NODES_HUMAN_DECISION_TOKEN"
API_ENV = "NODES_API_URL"
DEFAULT_POSTED_BY = "actor://company/developer"


def frame_path(repo: Path, slug: str) -> Path:
    return repo / ".devague" / "frames" / f"{slug}.json"


def post_frame(api_url: str, ticket_id: str, frame: dict, posted_by: str, token: str) -> dict:
    """POST the snapshot; returns the created TicketFrame (with `version`).
    Raises urllib.error.HTTPError on a non-2xx answer — the caller decides
    what a 401 (wrong or missing grant) means for its run."""
    body = json.dumps({"frame": frame, "posted_by": posted_by}).encode("utf-8")
    request = urllib.request.Request(
        f"{api_url.rstrip('/')}/v1alpha1/tickets/{ticket_id}/frame",
        data=body,
        method="POST",
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}"},
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.loads(response.read().decode("utf-8"))


def main(argv: list[str] | None = None, env: dict[str, str] | None = None) -> int:
    env = os.environ if env is None else env
    parser = argparse.ArgumentParser(prog="post_frame.py")
    parser.add_argument("--ticket", required=True, help="the Jira issue key, e.g. SCRUM-9")
    parser.add_argument("--slug", help="frame slug (default: the ticket id lowercased)")
    parser.add_argument("--repo", default=".", help="checkout holding .devague/")
    parser.add_argument("--frame-file", help="explicit snapshot path (overrides --slug/--repo)")
    parser.add_argument("--posted-by", default=DEFAULT_POSTED_BY)
    args = parser.parse_args(argv)

    api_url = env.get(API_ENV, "").strip()
    token = env.get(TOKEN_ENV, "").strip()
    if not api_url or not token:
        missing = [name for name, value in ((API_ENV, api_url), (TOKEN_ENV, token)) if not value]
        print(
            f"post_frame: {', '.join(missing)} not set — the decision token is a granted "
            "environment value on this lane, not an argument",
            file=sys.stderr,
        )
        return 2

    slug = args.slug or args.ticket.strip().lower()
    path = Path(args.frame_file) if args.frame_file else frame_path(Path(args.repo), slug)
    try:
        frame = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        print(f"post_frame: cannot read frame snapshot {path}: {exc}", file=sys.stderr)
        return 1

    try:
        created = post_frame(api_url, args.ticket, frame, args.posted_by, token)
    except urllib.error.HTTPError as exc:
        print(f"post_frame: {exc.code} from the control plane for {args.ticket}", file=sys.stderr)
        return 1
    except urllib.error.URLError as exc:
        print(f"post_frame: cannot reach {api_url}: {exc.reason}", file=sys.stderr)
        return 1
    print(json.dumps({"ticket_id": created.get("ticket_id"), "version": created.get("version")}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
