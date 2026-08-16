"""Poll declared GitHub merge observations beside the human inbox bridge.

The tracker has deliberately narrow custody and reach: it reads pending
task files from the bridge's durable store, calls GitHub with
optional ``GITHUB_TOKEN`` authentication, and submits an observed result
through the bridge's own HTTP inbox surface. It never uses a callback
credential; callback delivery remains the bridge server's job.

It touches the Culture Nodes control plane exactly once, at startup, and
only to READ: `verify_bridge_serves_actor` refuses to run unless the bridge
this tracker submits to is the bridge that serves the actor it observes
(issue #72). Nothing in a poll cycle calls the control plane.

That guard used to be an ADDRESS comparison — the actor's registered
`endpoint_ref` against this tracker's own bridge URL. Migration 0036
(issue #121) drops the column, so as of task t7 the check asks the bridge
itself and reads dial-in presence, which is keyed by actor_key and carries
no address. See `verify_bridge_serves_actor` for exactly what the new
evidence proves, and what it no longer can.

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
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Callable

from human_inbox_bridge.config import Config, ConfigError
from human_inbox_bridge.store import HumanTask

try:
    from human_inbox_bridge import nudge as _nudge_mod  # noqa: F401
except ImportError:
    _nudge_mod = None  # type: ignore[assignment]

logger = logging.getLogger("human_inbox_bridge.tracker")

GITHUB_API = "https://api.github.com"
OBSERVATION_KIND = "github_pr_merged"

#: issue #71 — the pr-upkeep decision node's observable. A task declaring
#: this kind is watching one PR thread for a human's answer to a question
#: the flow posted there, exactly the way OBSERVATION_KIND watches for a
#: merge. See `reply_observation_for` and `qualifying_reply` for the
#: "which reply counts" rule, and the MergeTracker.run_cycle docstring for
#: the shared-budget arithmetic that keeps this kind inside t35's ceiling.
REPLY_OBSERVATION_KIND = "github_pr_reply"

#: issue #71 — the decision node's OTHER terminal path: the PR closed
#: without merging while a question was still pending. Distinct from
#: REPLY_OBSERVATION_KIND's collection_method so the ledger record is
#: honest about which fact was actually observed.
CLOSED_OBSERVATION_KIND = "github_pr_closed"

#: GitHub logins the reply-detector never treats as a human answer. The
#: flow's own automation (the Qodo review bot; whatever identity posts the
#: decision question itself) shares the same PR thread it is now watching,
#: so authorship — not content — is the filter. Extend via
#: HUMAN_INBOX_TRACKER_REPLY_IGNORED_LOGINS; this default is always kept in
#: the union (an operator cannot accidentally re-admit the review bot).
DEFAULT_REPLY_IGNORED_LOGINS = frozenset({"qodo-code-review[bot]"})

ANONYMOUS_REQUESTS_PER_HOUR = 60
AUTHENTICATED_REQUESTS_PER_HOUR = 5_000

#: Fraction of GitHub's hourly ceiling the tracker will plan to consume.
#:
#: Planning for the whole ceiling is planning to be rate-limited. Two reasons,
#: and the anonymous lane feels both hardest because 60/hour leaves no room to
#: absorb either:
#:
#: 1. The anonymous quota is counted **per source IP**, not per process. The
#:    tracker shares its host with the operator's ``gh`` CLI, deploy scripts
#:    and anything else that touches api.github.com — none of which the
#:    tracker can see or account for.
#: 2. Our cycle clock and GitHub's hourly reset window are independent. Even
#:    alone and exactly at the limit, drift between the two puts two cycles'
#:    worth of requests inside one of GitHub's windows at the boundary.
#:
#: Half is a deliberate choice rather than a tuned one: it survives an equally
#: busy co-tenant on the same IP. The cost is detection latency (the anonymous
#: lane polls every 120s instead of 60s), which a human merge gate can afford.
#: Override with HUMAN_INBOX_TRACKER_RATE_UTILIZATION when the host is known to
#: be a sole tenant.
DEFAULT_RATE_UTILIZATION = 0.5
_REPO_RE = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")


class TrackerConfigError(Exception):
    """Raised when the tracker environment cannot produce a safe config."""


class BridgeIdentityError(TrackerConfigError):
    """Raised when the bridge this tracker submits to does not serve the
    actor it observes (issue #72) — or when that cannot be established.

    A TrackerConfigError subclass because it is exactly that: a deployment
    whose configuration cannot be run safely, refused at startup rather
    than half-run.
    """


@dataclass(frozen=True)
class TrackerConfig:
    state_dir: str = ".human-inbox-bridge-state"
    bridge_url: str = "http://127.0.0.1:8087"
    bridge_token: str = ""
    github_token: str = ""
    #: The bridge's own `actor_id` (Config.actor_id) — the actor_key this
    #: tracker observes, and the one the startup identity check confirms its
    #: bridge serves.
    actor_id: str = ""
    #: Base URL of the Culture Nodes control plane, read ONCE at startup for
    #: the dial-in-presence half of that check. Empty degrades the guard to
    #: its local half rather than switching it off; see
    #: `verify_bridge_serves_actor` for what that costs.
    control_plane_url: str = ""
    default_repo: str | None = None
    poll_seconds: float = 60.0
    github_request_budget: int = 50
    http_timeout_seconds: float = 30.0
    # Nudge transport settings (all optional; absent means nudging is disabled).
    nudge_channel_id: str = ""
    nudge_bot_token: str = ""
    nudge_interval_seconds: float = 300.0
    nudge_global_throttle_seconds: float = 10.0
    nudge_escalation_after_seconds: float = 600.0
    rate_utilization: float = DEFAULT_RATE_UTILIZATION
    #: issue #71 — logins the reply-kind "which reply counts" rule ignores.
    #: Always includes DEFAULT_REPLY_IGNORED_LOGINS regardless of what the
    #: environment adds (see from_env).
    reply_ignored_logins: frozenset[str] = field(
        default_factory=lambda: DEFAULT_REPLY_IGNORED_LOGINS
    )

    def __post_init__(self) -> None:
        """Clamp cadence and budget to the active GitHub API lane.

        GitHub's limits are hourly, while the tracker is configured per
        cycle. Applying the same ceiling at construction time keeps direct
        callers and environment-loaded production config honest alike.

        The plan targets a *fraction* of the ceiling, never the whole of it —
        see DEFAULT_RATE_UTILIZATION for why spending the last request is
        planning to be refused.
        """
        if not 0.0 < self.rate_utilization <= 1.0:
            raise TrackerConfigError(
                f"rate_utilization must be >0 and <=1 (got {self.rate_utilization!r}) — "
                "it is the fraction of GitHub's hourly ceiling the tracker plans to use; "
                "set HUMAN_INBOX_TRACKER_RATE_UTILIZATION to a value in that range"
            )
        requests_per_hour = self.github_requests_per_hour * self.rate_utilization
        minimum_poll_seconds = 3600.0 / requests_per_hour
        effective_poll_seconds = max(self.poll_seconds, minimum_poll_seconds)
        safe_cycle_budget = max(
            1,
            int(requests_per_hour * effective_poll_seconds / 3600.0),
        )
        object.__setattr__(self, "poll_seconds", effective_poll_seconds)
        object.__setattr__(
            self,
            "github_request_budget",
            min(self.github_request_budget, safe_cycle_budget),
        )

    @property
    def github_lane(self) -> str:
        return "authenticated" if self.github_token else "anonymous"

    @property
    def github_requests_per_hour(self) -> int:
        if self.github_token:
            return AUTHENTICATED_REQUESTS_PER_HOUR
        return ANONYMOUS_REQUESTS_PER_HOUR

    @classmethod
    def from_env(cls, env: dict[str, str] | None = None) -> "TrackerConfig":
        env = os.environ if env is None else env
        try:
            bridge_cfg = Config.load(env=env)
        except ConfigError as exc:
            raise TrackerConfigError(str(exc)) from exc
        github_token = env.get("GITHUB_TOKEN", "").strip()

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
        rate_utilization = _float_env(
            env, "HUMAN_INBOX_TRACKER_RATE_UTILIZATION", DEFAULT_RATE_UTILIZATION
        )
        extra_ignored = {
            login.strip()
            for login in env.get("HUMAN_INBOX_TRACKER_REPLY_IGNORED_LOGINS", "").split(",")
            if login.strip()
        }
        reply_ignored_logins = DEFAULT_REPLY_IGNORED_LOGINS | frozenset(extra_ignored)
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
            actor_id=bridge_cfg.actor_id,
            control_plane_url=env.get("HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL", "")
            .strip()
            .rstrip("/"),
            github_token=github_token,
            default_repo=default_repo,
            poll_seconds=poll_seconds,
            github_request_budget=request_budget,
            http_timeout_seconds=timeout_seconds,
            rate_utilization=rate_utilization,
            # Nudge transport (opt-in: all four DISCORD_NUDGE_* vars must be
            # present for nudging to be enabled).
            nudge_channel_id=env.get("DISCORD_NUDGE_CHANNEL_ID", "").strip(),
            nudge_bot_token=env.get("DISCORD_NUDGE_BOT_TOKEN", "").strip(),
            nudge_interval_seconds=_float_env(env, "DISCORD_NUDGE_INTERVAL_SECONDS", 300.0),
            nudge_global_throttle_seconds=_float_env(
                env, "DISCORD_NUDGE_GLOBAL_THROTTLE_SECONDS", 10.0
            ),
            nudge_escalation_after_seconds=_float_env(
                env, "DISCORD_NUDGE_ESCALATION_AFTER_SECONDS", 600.0
            ),
            reply_ignored_logins=reply_ignored_logins,
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
    rate_limited: bool
    retry_after_seconds: float


GithubFetch = Callable[..., dict[str, Any]]
BridgeSubmit = Callable[..., dict[str, Any]]


def _valid_repo(repo: object) -> bool:
    return isinstance(repo, str) and _REPO_RE.fullmatch(repo.strip()) is not None


def _observed_pr(raw: dict[str, Any], task: HumanTask) -> int | None:
    """Return which pull request an observation watches, or None.

    The `observe` block says WHAT is being watched; the PR number says which
    one, and the two do not have the same lifetime. Culture Nodes declares
    the kind as a literal binding written into the workflow text (issue #73)
    and that literal is fixed at publish time, so a workflow that loops over
    one PR after another binds the number separately — from the node that
    just opened the PR. `pr` is therefore read from the observation first and
    from the task's own input otherwise, exactly the fallback `repo` has
    always had. A declaration that supplies neither stays on the manual lane,
    and a value that is not a positive integer is malformed wherever it came
    from: the fallback widens where the number may come from, never what
    counts as one.
    """
    for candidate in (raw.get("pr"), task.extra_input.get("pr")):
        if candidate is None:
            continue
        if isinstance(candidate, bool) or not isinstance(candidate, int) or candidate <= 0:
            return None
        return candidate
    return None


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

    pr = _observed_pr(raw, task)
    if pr is None:
        return None
    repo = raw.get("repo") or task.extra_input.get("repo") or default_repo
    if not _valid_repo(repo):
        return None

    success_outcome = task.extra_input.get("success_outcome")
    outcome = success_outcome.strip() if isinstance(success_outcome, str) else ""
    return Observation(repo=repo.strip(), pr=pr, outcome=outcome or "merged")


@dataclass(frozen=True)
class ReplyObservation:
    """A parsed `observe: {kind: github_pr_reply, ...}` declaration.

    Three outcomes because a decision node genuinely has three ways to stop
    waiting (issue #71): a qualifying reply lands (``answered_outcome``),
    the PR is merged out from under the question (``merged_outcome`` — the
    strongest possible "yes" a human can give), or the PR closes unmerged
    (``dropped_outcome`` — the run must not wait forever on a dead PR).
    """

    repo: str
    pr: int
    since: str
    answered_outcome: str
    merged_outcome: str
    dropped_outcome: str


def _clean_outcome(value: object, default: str) -> str:
    return value.strip() if isinstance(value, str) and value.strip() else default


def reply_observation_for(task: HumanTask, default_repo: str | None) -> ReplyObservation | None:
    """Return the declared reply observation on a pending task, if complete.

    Mirrors `observation_for`'s shape exactly (undeclared/malformed stays
    manual-only). ``since`` is read from the task's OWN `created_at` — the
    moment the decision parked, i.e. the moment the question went up on the
    PR — never from caller input, so a task cannot be declared with a
    backdated watermark that scoops up an unrelated older comment.
    """
    raw = task.extra_input.get("observe")
    if not isinstance(raw, dict) or raw.get("kind") != REPLY_OBSERVATION_KIND:
        return None

    pr = _observed_pr(raw, task)
    if pr is None:
        return None
    repo = raw.get("repo") or task.extra_input.get("repo") or default_repo
    if not _valid_repo(repo):
        return None

    since = task.created_at.strip() if isinstance(task.created_at, str) else ""
    return ReplyObservation(
        repo=repo.strip(),
        pr=pr,
        since=since,
        answered_outcome=_clean_outcome(raw.get("answered_outcome"), "answered"),
        merged_outcome=_clean_outcome(raw.get("merged_outcome"), "merged"),
        dropped_outcome=_clean_outcome(raw.get("dropped_outcome"), "dropped"),
    )


def fetch_github_pull(repo: str, pr: int, *, token: str, timeout_seconds: float) -> dict[str, Any]:
    """GET one pull request, authenticating only when a token is present."""
    url = f"{GITHUB_API}/repos/{repo}/pulls/{pr}"
    headers = {
        "Accept": "application/vnd.github+json",
        "User-Agent": "culture-nodes-human-inbox-tracker",
        "X-GitHub-Api-Version": "2022-11-28",
    }
    if token:
        headers["Authorization"] = f"Bearer {token}"
    request = urllib.request.Request(  # noqa: S310 - fixed GitHub API host
        url,
        headers=headers,
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


def fetch_github_issue_comments(
    repo: str, pr: int, *, since: str, token: str, timeout_seconds: float
) -> list[dict[str, Any]]:
    """GET the issue-comment thread on one PR, GitHub's own `since` filter
    scoping the response to comments posted after the decision was parked.

    Costs one GitHub request, same as `fetch_github_pull` — this call and
    that one TOGETHER are what a reply-kind group's full check costs (see
    MergeTracker._check_reply_group's arithmetic note).
    """
    query = {"per_page": "100"}
    if since:
        query["since"] = since
    url = f"{GITHUB_API}/repos/{repo}/issues/{pr}/comments?{urllib.parse.urlencode(query)}"
    headers = {
        "Accept": "application/vnd.github+json",
        "User-Agent": "culture-nodes-human-inbox-tracker",
        "X-GitHub-Api-Version": "2022-11-28",
    }
    if token:
        headers["Authorization"] = f"Bearer {token}"
    request = urllib.request.Request(  # noqa: S310 - fixed GitHub API host
        url,
        headers=headers,
    )
    with urllib.request.urlopen(  # nosec B310 - request has a fixed GitHub HTTPS origin
        request, timeout=timeout_seconds
    ) as response:  # noqa: S310
        data = json.load(response)
    if not isinstance(data, list):
        raise ValueError("GitHub issue comments response was not a JSON array")
    return data


def qualifying_reply(
    comments: list[dict[str, Any]], *, ignored_logins: frozenset[str]
) -> dict[str, Any] | None:
    """The first comment (oldest first, GitHub's default order) that counts
    as the human's answer — the "which reply counts" rule issue #71 asks
    for, stated and justified here rather than left implicit.

    Two filters, both structural rather than content-based:

    1. **Freshness** — the caller already scoped the fetch to
       `since=<task.created_at>` (see `reply_observation_for`), so every
       comment this function sees was posted strictly after the decision
       node asked its question. No comment from BEFORE the question can
       ever qualify.
    2. **Authorship** — the comment's author must not be one of the flow's
       own automated identities (DEFAULT_REPLY_IGNORED_LOGINS plus any
       operator-configured additions). The flow's own posted question is
       itself a comment on this same thread; without this filter the
       tracker would immediately "reply to itself".

    Deliberately NOT a content/marker rule: requiring a magic prefix like
    "approve:" would make a human's ordinary PR reply invisible to the
    tracker unless they remembered the incantation. Given (1) and (2), the
    next human comment on a thread that JUST received a question IS the
    answer in context — there is no other reason for a person to comment on
    this specific PR at this specific moment. This is what keeps the flow
    from resuming on an unrelated "thanks": an unrelated aside would have to
    be posted by a non-bot author strictly after the question, on THIS PR,
    which in practice only the person answering the question does.
    """
    for comment in comments:
        login = (comment.get("user") or {}).get("login", "")
        if login in ignored_logins:
            continue
        body = comment.get("body")
        if not isinstance(body, str) or not body.strip():
            continue
        return comment
    return None


def submit_reply_observation(
    bridge_url: str,
    bridge_token: str,
    task: HumanTask,
    outcome: str,
    note: str,
    collection_method: str,
    observed_extra: dict[str, str],
    *,
    timeout_seconds: float,
) -> dict[str, Any]:
    """Submit one observed PR-thread answer through the bridge's
    authenticated surface. Mirrors `submit_observation`'s shape, generalized
    to the collection methods `mapping.py` accepts alongside
    `github_pr_merged` (`github_pr_reply`'s `reference`, `github_pr_closed`'s
    `reference`)."""
    submission = {
        "outcome": outcome,
        "note": note,
        "observed": {"collection_method": collection_method, **observed_extra},
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


def _github_rate_limit_backoff(
    exc: urllib.error.HTTPError,
    *,
    now: float,
    fallback_seconds: float,
) -> tuple[float | None, str]:
    """Return a retry epoch only when GitHub identifies a quota response."""
    try:
        body = exc.read().decode("utf-8", errors="replace")
    except (OSError, ValueError):
        body = ""
    try:
        parsed = json.loads(body)
    except (json.JSONDecodeError, TypeError):
        parsed = None
    message = parsed.get("message", "") if isinstance(parsed, dict) else body
    message = message.strip() if isinstance(message, str) else ""

    headers = exc.headers
    remaining = headers.get("X-RateLimit-Remaining", "") if headers is not None else ""
    reset = headers.get("X-RateLimit-Reset", "") if headers is not None else ""
    retry_after = headers.get("Retry-After", "") if headers is not None else ""
    lower_message = message.lower()
    is_rate_limit = exc.code == 429 or remaining.strip() == "0" or bool(retry_after)
    is_rate_limit = (
        is_rate_limit or "rate limit" in lower_message or "abuse detection" in lower_message
    )
    if exc.code not in (403, 429) or not is_rate_limit:
        return None, message

    backoff_until = 0.0
    try:
        backoff_until = float(reset)
    except (TypeError, ValueError):
        pass
    if backoff_until <= now:
        try:
            backoff_until = now + max(0.0, float(retry_after))
        except (TypeError, ValueError):
            backoff_until = 0.0
    if backoff_until <= now:
        # Secondary-limit responses do not always carry a reset epoch. One
        # normal effective cycle is the smallest honest delay in that case.
        backoff_until = now + fallback_seconds
    return backoff_until, message


# Imported after its dependencies are defined so tracker_cycle can use this
# module without a circular-import race. This is the compatibility re-export.
from human_inbox_bridge.tracker_cycle import MergeTracker  # noqa: E402
from human_inbox_bridge.tracker_identity import (  # noqa: E402,F401
    BridgeIdentity,
    ConfirmedIdentity,
    verify_bridge_serves_actor,
)


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
        # Before any polling: the bridge this tracker will submit through
        # must be the one the control plane dispatches this actor's work to.
        # BridgeIdentityError is a TrackerConfigError — a deployment that
        # cannot be run safely, refused rather than half-run.
        verify_bridge_serves_actor(cfg)
    except TrackerConfigError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    startup_message = (
        "GitHub polling lane=%s ceiling=%d requests/hour " "poll_seconds=%.1f request_budget=%d"
    )
    logger.info(
        startup_message,
        cfg.github_lane,
        cfg.github_requests_per_hour,
        cfg.poll_seconds,
        cfg.github_request_budget,
    )
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
