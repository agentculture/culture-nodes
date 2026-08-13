"""Poll declared GitHub merge observations beside the human inbox bridge.

The tracker has deliberately narrow custody and reach: it reads pending
task files from the bridge's durable store, calls GitHub with
``GITHUB_TOKEN``, and submits an observed result through the bridge's own
HTTP inbox surface. It never uses a callback credential and never calls
the Culture Nodes control plane directly; callback delivery remains the
bridge server's job.

Run continuously with ``python -m human_inbox_bridge.tracker`` or execute
one cycle with ``python -m human_inbox_bridge.tracker --once``.
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Any, Callable

from human_inbox_bridge.config import Config, ConfigError
from human_inbox_bridge.store import STATUS_PENDING, HumanTask, TaskStore

logger = logging.getLogger("human_inbox_bridge.tracker")

GITHUB_API = "https://api.github.com"
OBSERVATION_KIND = "github_pr_merged"
_REPO_RE = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")


class TrackerConfigError(Exception):
    """Raised when the tracker environment cannot produce a safe config."""


@dataclass(frozen=True)
class TrackerConfig:
    state_dir: str = ".human-inbox-bridge-state"
    bridge_url: str = "http://127.0.0.1:8087"
    bridge_token: str = ""
    github_token: str = ""
    default_repo: str | None = None
    poll_seconds: float = 60.0
    github_request_budget: int = 50
    http_timeout_seconds: float = 30.0

    @classmethod
    def from_env(cls, env: dict[str, str] | None = None) -> "TrackerConfig":
        env = os.environ if env is None else env
        try:
            bridge_cfg = Config.load(env=env)
        except ConfigError as exc:
            raise TrackerConfigError(str(exc)) from exc
        github_token = env.get("GITHUB_TOKEN", "").strip()
        if not github_token:
            raise TrackerConfigError("GITHUB_TOKEN is required")

        default_repo = (
            env.get("HUMAN_INBOX_TRACKER_DEFAULT_REPO")
            or env.get("HUMAN_INBOX_TRACKER_REPO")
            or None
        )
        if default_repo is not None and not _valid_repo(default_repo):
            raise TrackerConfigError(
                "HUMAN_INBOX_TRACKER_DEFAULT_REPO must be an owner/repository name"
            )

        poll_seconds = _float_env(env, "HUMAN_INBOX_TRACKER_POLL_SECONDS", 60.0)
        request_budget = _int_env(env, "HUMAN_INBOX_TRACKER_GITHUB_REQUEST_BUDGET", 50)
        timeout_seconds = _float_env(env, "HUMAN_INBOX_TRACKER_HTTP_TIMEOUT_SECONDS", 30.0)
        if poll_seconds <= 0:
            raise TrackerConfigError("HUMAN_INBOX_TRACKER_POLL_SECONDS must be greater than zero")
        if request_budget < 0:
            raise TrackerConfigError(
                "HUMAN_INBOX_TRACKER_GITHUB_REQUEST_BUDGET must be zero or greater"
            )
        if timeout_seconds <= 0:
            raise TrackerConfigError(
                "HUMAN_INBOX_TRACKER_HTTP_TIMEOUT_SECONDS must be greater than zero"
            )

        return cls(
            state_dir=env.get(
                "HUMAN_INBOX_TRACKER_STATE_DIR",
                bridge_cfg.state_dir,
            ),
            bridge_url=env.get(
                "HUMAN_INBOX_TRACKER_BRIDGE_URL",
                f"http://127.0.0.1:{bridge_cfg.port}",
            ).rstrip("/"),
            bridge_token=bridge_cfg.auth_token or "",
            github_token=github_token,
            default_repo=default_repo,
            poll_seconds=poll_seconds,
            github_request_budget=request_budget,
            http_timeout_seconds=timeout_seconds,
        )


@dataclass(frozen=True)
class Observation:
    repo: str
    pr: int
    outcome: str


@dataclass(frozen=True)
class CycleResult:
    pending_tasks: int
    eligible_tasks: int
    github_requests: int
    submissions: int
    budget_exhausted: bool


GithubFetch = Callable[..., dict[str, Any]]
BridgeSubmit = Callable[..., dict[str, Any]]


def _valid_repo(repo: object) -> bool:
    return isinstance(repo, str) and _REPO_RE.fullmatch(repo.strip()) is not None


def observation_for(task: HumanTask, default_repo: str | None) -> Observation | None:
    """Return the supported declaration on a pending task, if it is complete.

    Undeclared, malformed, and unsupported declarations are deliberately
    ignored: those tasks remain on the manual lane. ``success_outcome`` is
    an existing bridge-family input convention; merge tasks that omit it
    use the observation kind's unambiguous domain outcome, ``merged``.
    """
    raw = task.extra_input.get("observe")
    if not isinstance(raw, dict) or raw.get("kind") != OBSERVATION_KIND:
        return None

    pr = raw.get("pr")
    if isinstance(pr, bool) or not isinstance(pr, int) or pr <= 0:
        return None
    repo = raw.get("repo") or task.extra_input.get("repo") or default_repo
    if not _valid_repo(repo):
        return None

    success_outcome = task.extra_input.get("success_outcome")
    outcome = success_outcome.strip() if isinstance(success_outcome, str) else ""
    return Observation(repo=repo.strip(), pr=pr, outcome=outcome or "merged")


def fetch_github_pull(repo: str, pr: int, *, token: str, timeout_seconds: float) -> dict[str, Any]:
    """GET one pull request using only stdlib urllib."""
    url = f"{GITHUB_API}/repos/{repo}/pulls/{pr}"
    request = urllib.request.Request(  # noqa: S310 - fixed GitHub API host
        url,
        headers={
            "Accept": "application/vnd.github+json",
            "Authorization": f"Bearer {token}",
            "User-Agent": "culture-nodes-human-inbox-tracker",
            "X-GitHub-Api-Version": "2022-11-28",
        },
    )
    with urllib.request.urlopen(  # nosec B310 - request has a fixed GitHub HTTPS origin
        request, timeout=timeout_seconds
    ) as response:  # noqa: S310
        data = json.load(response)
    if not isinstance(data, dict):
        raise ValueError("GitHub pull response was not a JSON object")
    return data


def submit_observation(
    bridge_url: str,
    bridge_token: str,
    task: HumanTask,
    observation: Observation,
    merge_commit: str,
    *,
    timeout_seconds: float,
) -> dict[str, Any]:
    """Submit one observed merge through the bridge's authenticated surface."""
    submission = {
        "outcome": observation.outcome,
        "note": (f"observed {observation.repo}#{observation.pr} merged at commit {merge_commit}"),
        "observed": {
            "collection_method": OBSERVATION_KIND,
            "merge_commit": merge_commit,
        },
    }
    task_id = urllib.parse.quote(task.invocation_id, safe="")
    url = f"{bridge_url.rstrip('/')}/inbox/tasks/{task_id}/submit"
    headers = {"Content-Type": "application/json"}
    if bridge_token:
        headers["Authorization"] = f"Bearer {bridge_token}"
    request = urllib.request.Request(  # noqa: S310 - operator-configured sibling bridge
        url,
        data=json.dumps(submission).encode("utf-8"),
        method="POST",
        headers=headers,
    )
    with urllib.request.urlopen(  # nosec B310 - operator-configured sibling bridge URL
        request, timeout=timeout_seconds
    ) as response:  # noqa: S310
        data = json.load(response)
    if not isinstance(data, dict):
        raise ValueError("bridge submit response was not a JSON object")
    return data


class MergeTracker:
    """One poller over the bridge task store, with injectable HTTP seams."""

    def __init__(
        self,
        cfg: TrackerConfig,
        *,
        github_fetch: GithubFetch = fetch_github_pull,
        bridge_submit: BridgeSubmit = submit_observation,
    ) -> None:
        self.cfg = cfg
        # The tracker is a reader of bridge-owned task files. In particular,
        # it does not initialise or mutate the state directory when the
        # bridge has not created it yet.
        self.store = TaskStore(cfg.state_dir, create=False)
        self.github_fetch = github_fetch
        self.bridge_submit = bridge_submit

    def run_cycle(self) -> CycleResult:
        pending = self.store.list(status=STATUS_PENDING)
        grouped: dict[tuple[str, int], list[tuple[HumanTask, Observation]]] = {}
        eligible = 0
        for task in pending:
            observation = observation_for(task, self.cfg.default_repo)
            if observation is None:
                continue
            eligible += 1
            grouped.setdefault((observation.repo, observation.pr), []).append((task, observation))

        github_requests = 0
        submissions = 0
        budget_exhausted = False
        for (repo, pr), tasks in grouped.items():
            if github_requests >= self.cfg.github_request_budget:
                budget_exhausted = True
                break
            github_requests += 1
            try:
                pull = self.github_fetch(
                    repo,
                    pr,
                    token=self.cfg.github_token,
                    timeout_seconds=self.cfg.http_timeout_seconds,
                )
            except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
                logger.warning("GitHub check failed for %s#%d: %s", repo, pr, exc)
                continue

            # GitHub reports closed-unmerged PRs with merged=false. Only the
            # literal true state is unambiguous enough to leave the manual lane.
            if pull.get("merged") is not True:
                continue
            merge_commit = pull.get("merge_commit_sha")
            if not isinstance(merge_commit, str) or not merge_commit.strip():
                logger.warning("merged PR %s#%d did not name a merge commit", repo, pr)
                continue
            merge_commit = merge_commit.strip()

            for task, observation in tasks:
                try:
                    self.bridge_submit(
                        self.cfg.bridge_url,
                        self.cfg.bridge_token,
                        task,
                        observation,
                        merge_commit,
                        timeout_seconds=self.cfg.http_timeout_seconds,
                    )
                except urllib.error.HTTPError as exc:
                    # A 409 means a human or another tracker completed it
                    # after this cycle's read. That race is already resolved.
                    exc.read()
                    if exc.code == 409:
                        logger.info("task %s was already completed", task.invocation_id)
                    else:
                        logger.warning(
                            "bridge submit failed for task %s: HTTP %d",
                            task.invocation_id,
                            exc.code,
                        )
                    continue
                except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
                    logger.warning("bridge submit failed for task %s: %s", task.invocation_id, exc)
                    continue
                submissions += 1

        if budget_exhausted:
            logger.warning(
                "GitHub request budget exhausted after %d request(s); remaining tasks wait",
                github_requests,
            )
        return CycleResult(
            pending_tasks=len(pending),
            eligible_tasks=eligible,
            github_requests=github_requests,
            submissions=submissions,
            budget_exhausted=budget_exhausted,
        )

    def run_forever(self) -> None:
        while True:
            result = self.run_cycle()
            logger.info(
                "cycle: pending=%d eligible=%d github_requests=%d submissions=%d",
                result.pending_tasks,
                result.eligible_tasks,
                result.github_requests,
                result.submissions,
            )
            time.sleep(self.cfg.poll_seconds)


def _int_env(env: dict[str, str], name: str, default: int) -> int:
    raw = env.get(name)
    if raw is None:
        return default
    try:
        return int(raw)
    except ValueError as exc:
        raise TrackerConfigError(f"{name}={raw!r} is not an integer") from exc


def _float_env(env: dict[str, str], name: str, default: float) -> float:
    raw = env.get(name)
    if raw is None:
        return default
    try:
        return float(raw)
    except ValueError as exc:
        raise TrackerConfigError(f"{name}={raw!r} is not a number") from exc


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--once", action="store_true", help="Run one poll cycle and exit.")
    parser.add_argument("-v", "--verbose", action="store_true", help="Enable debug logging.")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    try:
        cfg = TrackerConfig.from_env()
    except TrackerConfigError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    tracker = MergeTracker(cfg)
    try:
        if args.once:
            tracker.run_cycle()
        else:
            tracker.run_forever()
    except KeyboardInterrupt:
        logger.info("tracker stopped")
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
