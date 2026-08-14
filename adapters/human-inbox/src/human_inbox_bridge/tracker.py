"""Poll declared GitHub merge observations beside the human inbox bridge.

The tracker has deliberately narrow custody and reach: it reads pending
task files from the bridge's durable store, calls GitHub with
optional ``GITHUB_TOKEN`` authentication, and submits an observed result
through the bridge's own HTTP inbox surface. It never uses a callback
credential; callback delivery remains the bridge server's job.

It touches the Culture Nodes control plane exactly once, at startup, and
only to READ: `verify_bridge_serves_actor` resolves the configured actor's
registered `endpoint_ref` and refuses to run when that endpoint is not the
bridge this tracker submits to (issue #72). Nothing in a poll cycle calls
the control plane.

Run continuously with ``python -m human_inbox_bridge.tracker`` or execute
one cycle with ``python -m human_inbox_bridge.tracker --once``.
"""

from __future__ import annotations

import argparse
import ipaddress
import json
import logging
import os
import re
import socket
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Callable

from human_inbox_bridge.config import Config, ConfigError
from human_inbox_bridge.store import STATUS_PENDING, HumanTask, TaskStore

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
    #: The bridge's own `actor_id` (Config.actor_id) — the actor_key whose
    #: registered endpoint the startup identity check resolves.
    actor_id: str = ""
    #: Base URL of the Culture Nodes control plane, read ONCE at startup for
    #: that check. Empty disables it; see `verify_bridge_serves_actor` for
    #: what that costs.
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


# --- startup identity check (issue #72) --------------------------------
#
# Why this is a refusal and not a warning: the bridge's idempotency store is
# per-bridge and file-based — one JSON file per key under Config.state_dir —
# so it can only deduplicate submissions that pass through the SAME bridge
# process's state directory. Two bridges serving one actor do not see each
# other's replays at all. "One logical human inbox" is therefore enforced by
# deployment convention, and this check is the only mechanism that can
# notice the convention has been broken. A tracker that keeps running
# against the wrong bridge submits observations no store can dedupe.


@dataclass(frozen=True)
class ActorEndpoint:
    """The live registration row for one actor_key: its newest revision."""

    actor_key: str
    revision: int
    endpoint_ref: str


def fetch_actors(control_plane_url: str, *, timeout_seconds: float) -> list[dict[str, Any]]:
    """GET every registered actor row from the control plane.

    Unauthenticated on purpose: `GET /v1alpha1/actors` is the read-only half
    of the actors noun (spec decision c45 — only registration and human
    decisions carry a bearer token), and this tracker holds no control-plane
    credential of any kind.
    """
    url = f"{control_plane_url.rstrip('/')}/v1alpha1/actors"
    request = urllib.request.Request(  # noqa: S310 - operator-configured control plane
        url,
        headers={
            "Accept": "application/json",
            "User-Agent": "culture-nodes-human-inbox-tracker",
        },
    )
    with urllib.request.urlopen(  # nosec B310 - operator-configured control-plane URL
        request, timeout=timeout_seconds
    ) as response:  # noqa: S310
        data = json.load(response)
    items = data.get("items") if isinstance(data, dict) else None
    if not isinstance(items, list):
        raise ValueError("control-plane actor list response had no 'items' array")
    return [item for item in items if isinstance(item, dict)]


def newest_actor_revision(items: list[dict[str, Any]], actor_key: str) -> ActorEndpoint | None:
    """The highest-revision row for *actor_key*, or None if it has none.

    Actor identity is append-only: an endpoint move appends a new revision
    rather than updating the old row, so the newest revision — not any row
    that happens to match — is the one this tracker must agree with.
    """
    newest: ActorEndpoint | None = None
    for item in items:
        if item.get("actor_key") != actor_key:
            continue
        revision = item.get("revision")
        if isinstance(revision, bool) or not isinstance(revision, int):
            continue
        endpoint = item.get("endpoint_ref")
        candidate = ActorEndpoint(
            actor_key=actor_key,
            revision=revision,
            endpoint_ref=endpoint if isinstance(endpoint, str) else "",
        )
        if newest is None or candidate.revision > newest.revision:
            newest = candidate
    return newest


def _numeric_addresses(host: str) -> list[ipaddress.IPv4Address | ipaddress.IPv6Address]:
    """*host* as IP addresses: itself when it is already a literal, its DNS
    answers otherwise. A name that does not resolve yields nothing, which
    the caller treats as "not local" (and so, fails closed)."""
    try:
        return [ipaddress.ip_address(host)]
    except ValueError:
        pass
    try:
        infos = socket.getaddrinfo(host, None, proto=socket.IPPROTO_TCP)
    except (OSError, UnicodeError):
        return []
    resolved = []
    for info in infos:
        try:
            resolved.append(ipaddress.ip_address(info[4][0]))
        except ValueError:  # pragma: no cover - getaddrinfo returns literals
            continue
    return resolved


def _is_local_address(host: str) -> bool:
    """Whether *host* is an address this machine itself answers on.

    Loopback is trivially local. For anything else, a UDP socket is
    *connected* to the address — which sends no packet — and its source
    address inspected: the kernel picks the destination itself as the source
    only when the destination is one of this host's own addresses. That is
    what makes `http://127.0.0.1:8090` and the actor row's
    `http://192.168.1.157:8090` recognisable as the same bridge on the host
    that owns .157, and NOT the same bridge anywhere else.
    """
    for address in _numeric_addresses(host):
        if address.is_loopback:
            return True
        family = socket.AF_INET6 if address.version == 6 else socket.AF_INET
        try:
            with socket.socket(family, socket.SOCK_DGRAM) as probe:
                probe.settimeout(0.1)
                # Discard port; connect(2) on a UDP socket sends nothing, it
                # only asks the kernel to pick a source address.
                probe.connect((str(address), 9))
                if ipaddress.ip_address(probe.getsockname()[0]) == address:
                    return True
        except (OSError, ValueError):
            # No route to the address (or no such interface). Unresolvable
            # is not local — the caller fails closed on that, by design.
            continue
    return False


def _host_port(url: str) -> tuple[str, int]:
    """Split an endpoint into the pair that identifies a bridge process.

    Scheme is deliberately NOT part of the identity: two schemes reaching
    the same host and port are the same listening process, and the port
    comparison already separates the cases where they are not.
    """
    parsed = urllib.parse.urlsplit(url.strip())
    if parsed.scheme not in ("http", "https") or not parsed.hostname:
        raise ValueError(f"{url!r} is not an http(s) URL with a host")
    port = parsed.port or (443 if parsed.scheme == "https" else 80)
    return parsed.hostname.lower(), port


def verify_bridge_serves_actor(
    cfg: TrackerConfig,
    *,
    actor_fetch: Callable[..., list[dict[str, Any]]] | None = None,
    is_local_address: Callable[[str], bool] | None = None,
) -> ActorEndpoint | None:
    """Refuse to start unless this tracker's bridge is the actor's bridge.

    Returns the resolved registration row, or None when no control plane is
    configured and the check was skipped. Raises `BridgeIdentityError` for
    every other unhappy path — including an unreachable control plane, since
    an unverified identity is not a verified one and the systemd unit
    restarts (an outage costs retries, not an unguarded window).

    Both seams default to the module-level implementations and are resolved
    at call time rather than bound as defaults, so a test can monkeypatch
    either without reaching past `main`.
    """
    fetch = fetch_actors if actor_fetch is None else actor_fetch
    is_local = _is_local_address if is_local_address is None else is_local_address

    if not cfg.control_plane_url:
        logger.warning(
            "no control plane configured (HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL): starting "
            "WITHOUT checking that %s is the bridge registered for actor %r. The bridge's "
            "idempotency store is per-bridge and file-based, so it cannot deduplicate "
            "submissions made through a second bridge serving the same actor — this check is "
            "the only guard against that, and it is now inactive",
            cfg.bridge_url,
            cfg.actor_id,
        )
        return None

    try:
        items = fetch(cfg.control_plane_url, timeout_seconds=cfg.http_timeout_seconds)
    except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
        raise BridgeIdentityError(
            f"cannot resolve actor {cfg.actor_id!r} from the control plane at "
            f"{cfg.control_plane_url}: {exc}. Refusing to start: an unverified bridge identity "
            "is not a verified one, and the per-bridge idempotency store cannot deduplicate a "
            "split deployment"
        ) from exc

    registered = newest_actor_revision(items, cfg.actor_id)
    if registered is None:
        raise BridgeIdentityError(
            f"the control plane at {cfg.control_plane_url} has no actor registered under "
            f"{cfg.actor_id!r}, so this tracker cannot confirm that its bridge "
            f"({cfg.bridge_url}) serves the actor it observes. Set HUMAN_INBOX_BRIDGE_ACTOR_ID "
            "to the registered actor_key, or register the actor first"
        )

    try:
        actor_host, actor_port = _host_port(registered.endpoint_ref)
    except ValueError as exc:
        raise BridgeIdentityError(
            f"actor {registered.actor_key!r} (revision {registered.revision}) is registered "
            f"with an endpoint this tracker cannot compare against its own bridge "
            f"({cfg.bridge_url}): {exc}"
        ) from exc
    try:
        bridge_host, bridge_port = _host_port(cfg.bridge_url)
    except ValueError as exc:
        raise BridgeIdentityError(
            f"HUMAN_INBOX_TRACKER_BRIDGE_URL is not usable as a bridge endpoint: {exc}"
        ) from exc

    # The port must always agree — a second bridge on this same host is a
    # second idempotency store, not this actor's inbox. Given that, two host
    # spellings are the same machine when they name the same address, or
    # when they are BOTH addresses of the machine this tracker runs on
    # (which is what makes a loopback bridge_url and the actor row's LAN
    # address recognisable as one bridge, and only on the host that owns it).
    same_bridge = actor_port == bridge_port and (
        actor_host == bridge_host
        or bool(set(_numeric_addresses(actor_host)) & set(_numeric_addresses(bridge_host)))
        or (is_local(actor_host) and is_local(bridge_host))
    )
    if not same_bridge:
        raise BridgeIdentityError(
            f"this tracker submits to {cfg.bridge_url}, but actor {registered.actor_key!r} "
            f"(revision {registered.revision}) is registered at {registered.endpoint_ref} — "
            "a different bridge. Refusing to start: the bridge idempotency store is per-bridge "
            "and file-based, so two bridges serving one actor cannot deduplicate each other's "
            "submissions. Run this tracker on the host serving the actor, or point "
            "HUMAN_INBOX_TRACKER_BRIDGE_URL at the registered endpoint"
        )

    logger.info(
        "bridge identity confirmed: %s serves actor %s (revision %d, registered %s)",
        cfg.bridge_url,
        registered.actor_key,
        registered.revision,
        registered.endpoint_ref,
    )
    return registered


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

    pr = raw.get("pr")
    if isinstance(pr, bool) or not isinstance(pr, int) or pr <= 0:
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


class MergeTracker:
    """One poller over the bridge task store, with injectable HTTP seams."""

    def __init__(
        self,
        cfg: TrackerConfig,
        *,
        github_fetch: GithubFetch = fetch_github_pull,
        bridge_submit: BridgeSubmit = submit_observation,
        reply_fetch: Callable[..., list[dict[str, Any]]] = fetch_github_issue_comments,
        reply_submit: Callable[..., dict[str, Any]] = submit_reply_observation,
        nudge_cfg: object | None = None,
        clock: Callable[[], float] = time.time,
    ) -> None:
        self.cfg = cfg
        # The tracker is a reader of bridge-owned task files. In particular,
        # it does not initialise or mutate the state directory when the
        # bridge has not created it yet.
        self.store = TaskStore(cfg.state_dir, create=False)
        self.github_fetch = github_fetch
        self.bridge_submit = bridge_submit
        self.reply_fetch = reply_fetch
        self.reply_submit = reply_submit
        self.nudge_cfg = nudge_cfg
        self.clock = clock
        self._github_backoff_until = 0.0
        self._next_group_index = 0
        self._next_reply_group_index = 0

    def run_cycle(self) -> CycleResult:
        """One poll cycle over both observation kinds, sharing ONE GitHub
        request budget (issue #71's rate-headroom requirement: a new
        observable kind must not multiply requests per cycle).

        Reply-kind groups are checked BEFORE merge-kind groups. This is a
        deliberate priority choice, not an extra allowance: a reply-kind
        group has a HUMAN actively blocked on it (the decision node is
        parked, mid-conversation, on a question the flow just asked),
        whereas a merge-kind group's human already has the PR to merge at
        their own pace. Checking reply groups first means, when the shared
        per-cycle budget is tight, it is spent on the more time-sensitive
        wait first — it does NOT raise `github_request_budget` or shorten
        `poll_seconds`, so the planned hourly request rate is unchanged
        (see TrackerConfig.__post_init__'s clamp, which this ordering does
        not touch).

        The arithmetic (t35's ceiling, DEFAULT_RATE_UTILIZATION=0.5): in the
        worst case — anonymous lane, GITHUB_TOKEN unset — the clamp yields
        poll_seconds=120 and github_request_budget=1, i.e. ONE GitHub
        request per 120s cycle, 30 requests/hour against the 60/hour
        anonymous ceiling, REGARDLESS of how many (kind, repo, pr) groups
        are pending — adding REPLY_OBSERVATION_KIND groups only adds more
        entrants to the SAME round-robin queue sharing that one request,
        never more requests per hour. A reply-kind group's FULL check
        (terminal-state GET via `github_fetch`, then a comments GET via
        `reply_fetch` when the PR is still open) costs up to two of those
        budget units, so at budget=1 an open reply-kind group takes at
        least two cycles (up to ~240s) to complete one full check pass —
        slower detection than a merge-kind group's one-call check, an
        explicit and accepted trade-off for staying inside the same
        ceiling, not a silent overrun. `_check_reply_group` degrades
        gracefully when the second call would exceed the remaining budget:
        it leaves the group pending rather than spending past the cap.
        """
        pending = self.store.list(status=STATUS_PENDING)
        grouped: dict[tuple[str, int], list[tuple[HumanTask, Observation]]] = {}
        reply_grouped: dict[tuple[str, int], list[tuple[HumanTask, ReplyObservation]]] = {}
        eligible = 0
        for task in pending:
            observation = observation_for(task, self.cfg.default_repo)
            if observation is not None:
                eligible += 1
                grouped.setdefault((observation.repo, observation.pr), []).append(
                    (task, observation)
                )
                continue
            reply_observation = reply_observation_for(task, self.cfg.default_repo)
            if reply_observation is not None:
                eligible += 1
                reply_grouped.setdefault((reply_observation.repo, reply_observation.pr), []).append(
                    (task, reply_observation)
                )

        github_requests = 0
        submissions = 0
        budget_exhausted = False
        rate_limited = False
        now = self.clock()
        grouped_items = list(grouped.items())
        if grouped_items:
            start = self._next_group_index % len(grouped_items)
            grouped_items = grouped_items[start:] + grouped_items[:start]

        if self._github_backoff_until > now:
            rate_limited = True
            logger.info(
                "GitHub rate-limit backoff active for %.0f more second(s); "
                "watched PRs wait for the next cycle",
                self._github_backoff_until - now,
            )
        else:
            self._github_backoff_until = 0.0

        reply_items = list(reply_grouped.items())
        if reply_items:
            start = self._next_reply_group_index % len(reply_items)
            reply_items = reply_items[start:] + reply_items[:start]

        for (repo, pr), tasks in reply_items:
            if rate_limited:
                break
            if github_requests >= self.cfg.github_request_budget:
                budget_exhausted = True
                break
            used, submitted, became_rate_limited = self._check_reply_group(
                repo,
                pr,
                tasks,
                budget_remaining=self.cfg.github_request_budget - github_requests,
            )
            github_requests += used
            submissions += submitted
            self._next_reply_group_index += 1
            if became_rate_limited:
                rate_limited = True
                break

        for (repo, pr), tasks in grouped_items:
            if rate_limited:
                break
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
            except urllib.error.HTTPError as exc:
                backoff_until, message = _github_rate_limit_backoff(
                    exc,
                    now=self.clock(),
                    fallback_seconds=self.cfg.poll_seconds,
                )
                if backoff_until is not None:
                    self._github_backoff_until = backoff_until
                    rate_limited = True
                    logger.warning(
                        "GitHub rate limit exhausted in %s lane while checking %s#%d; "
                        "backing off for %.0f second(s) until reset, then retrying next cycle%s",
                        self.cfg.github_lane,
                        repo,
                        pr,
                        max(0.0, backoff_until - self.clock()),
                        f" ({message})" if message else "",
                    )
                    break
                if exc.code == 403:
                    logger.warning(
                        "GitHub permission denied (not rate limited) in %s lane for %s#%d; "
                        "the repository may be private or the token may lack Pull requests: Read",
                        self.cfg.github_lane,
                        repo,
                        pr,
                    )
                else:
                    logger.warning("GitHub check failed for %s#%d: HTTP %d", repo, pr, exc.code)
                self._next_group_index += 1
                continue
            except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
                logger.warning("GitHub check failed for %s#%d: %s", repo, pr, exc)
                self._next_group_index += 1
                continue
            self._next_group_index += 1

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

        # Nudge cycle — runs after merge observation so it never blocks the
        # main path.  Idempotent: nudges are deduplicated by thread_id and
        # last_nudge_at timestamps.
        self._run_nudge_cycle(pending)

        return CycleResult(
            pending_tasks=len(pending),
            eligible_tasks=eligible,
            github_requests=github_requests,
            submissions=submissions,
            budget_exhausted=budget_exhausted,
            rate_limited=rate_limited,
            retry_after_seconds=max(0.0, self._github_backoff_until - self.clock()),
        )

    def _check_reply_group(
        self,
        repo: str,
        pr: int,
        tasks: list[tuple[HumanTask, ReplyObservation]],
        *,
        budget_remaining: int,
    ) -> tuple[int, int, bool]:
        """Check one (repo, pr) reply-kind group. Returns
        ``(requests_used, submissions, became_rate_limited)``.

        Terminal states are checked first and, when found, cost exactly ONE
        request (merged/closed both short-circuit before the comments
        call). Only a still-open PR spends the second request, and only
        when the remaining budget allows it — see run_cycle's docstring for
        the arithmetic this degrades against.
        """
        if budget_remaining <= 0:
            return 0, 0, False

        try:
            pull = self.github_fetch(
                repo, pr, token=self.cfg.github_token, timeout_seconds=self.cfg.http_timeout_seconds
            )
        except urllib.error.HTTPError as exc:
            backoff_until, message = _github_rate_limit_backoff(
                exc, now=self.clock(), fallback_seconds=self.cfg.poll_seconds
            )
            if backoff_until is not None:
                self._github_backoff_until = backoff_until
                logger.warning(
                    "GitHub rate limit exhausted in %s lane while checking reply state for "
                    "%s#%d; backing off for %.0f second(s) until reset%s",
                    self.cfg.github_lane,
                    repo,
                    pr,
                    max(0.0, backoff_until - self.clock()),
                    f" ({message})" if message else "",
                )
                return 1, 0, True
            logger.warning("GitHub reply-state check failed for %s#%d: HTTP %d", repo, pr, exc.code)
            return 1, 0, False
        except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
            logger.warning("GitHub reply-state check failed for %s#%d: %s", repo, pr, exc)
            return 1, 0, False

        submissions = 0
        if pull.get("merged") is True:
            merge_commit = pull.get("merge_commit_sha")
            merge_commit = merge_commit.strip() if isinstance(merge_commit, str) else ""
            if not merge_commit:
                logger.warning("merged PR %s#%d did not name a merge commit", repo, pr)
                return 1, 0, False
            for task, obs in tasks:
                if self._submit_reply(
                    task,
                    obs.merged_outcome,
                    f"observed {repo}#{pr} merged at commit {merge_commit} while a decision "
                    "was pending",
                    OBSERVATION_KIND,
                    {"merge_commit": merge_commit},
                ):
                    submissions += 1
            return 1, submissions, False

        if pull.get("state") == "closed":
            reference = pull.get("html_url") or f"https://github.com/{repo}/pull/{pr}"
            for task, obs in tasks:
                if self._submit_reply(
                    task,
                    obs.dropped_outcome,
                    f"observed {repo}#{pr} closed without merging while a decision was pending",
                    CLOSED_OBSERVATION_KIND,
                    {"reference": reference},
                ):
                    submissions += 1
            return 1, submissions, False

        # Still open: a second request, budget permitting, looks for a
        # qualifying reply. Every task in the group watches the same PR, so
        # the earliest `since` is the honest "question asked" watermark for
        # the whole group.
        if budget_remaining < 2:
            return 1, 0, False
        since = min((obs.since for _task, obs in tasks if obs.since), default="")
        try:
            comments = self.reply_fetch(
                repo,
                pr,
                since=since,
                token=self.cfg.github_token,
                timeout_seconds=self.cfg.http_timeout_seconds,
            )
        except urllib.error.HTTPError as exc:
            backoff_until, message = _github_rate_limit_backoff(
                exc, now=self.clock(), fallback_seconds=self.cfg.poll_seconds
            )
            if backoff_until is not None:
                self._github_backoff_until = backoff_until
                logger.warning(
                    "GitHub rate limit exhausted in %s lane while checking reply comments for "
                    "%s#%d; backing off for %.0f second(s) until reset%s",
                    self.cfg.github_lane,
                    repo,
                    pr,
                    max(0.0, backoff_until - self.clock()),
                    f" ({message})" if message else "",
                )
                return 2, 0, True
            logger.warning(
                "GitHub reply-comment check failed for %s#%d: HTTP %d", repo, pr, exc.code
            )
            return 2, 0, False
        except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
            logger.warning("GitHub reply-comment check failed for %s#%d: %s", repo, pr, exc)
            return 2, 0, False

        reply = qualifying_reply(comments, ignored_logins=self.cfg.reply_ignored_logins)
        if reply is None:
            return 2, 0, False
        reference = reply.get("html_url") or f"https://github.com/{repo}/pull/{pr}"
        for task, obs in tasks:
            if self._submit_reply(
                task,
                obs.answered_outcome,
                f"observed a reply on {repo}#{pr}",
                REPLY_OBSERVATION_KIND,
                {"reference": reference},
            ):
                submissions += 1
        return 2, submissions, False

    def _submit_reply(
        self,
        task: HumanTask,
        outcome: str,
        note: str,
        collection_method: str,
        observed_extra: dict[str, str],
    ) -> bool:
        try:
            self.reply_submit(
                self.cfg.bridge_url,
                self.cfg.bridge_token,
                task,
                outcome,
                note,
                collection_method,
                observed_extra,
                timeout_seconds=self.cfg.http_timeout_seconds,
            )
        except urllib.error.HTTPError as exc:
            # A 409 means a human or another tracker completed it after this
            # cycle's read. That race is already resolved.
            exc.read()
            if exc.code == 409:
                logger.info("task %s was already completed", task.invocation_id)
            else:
                logger.warning(
                    "bridge submit failed for task %s: HTTP %d", task.invocation_id, exc.code
                )
            return False
        except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
            logger.warning("bridge submit failed for task %s: %s", task.invocation_id, exc)
            return False
        return True

    def run_forever(self) -> None:
        while True:
            result = self.run_cycle()
            logger.info(
                "cycle: pending=%d eligible=%d github_requests=%d submissions=%d rate_limited=%s",
                result.pending_tasks,
                result.eligible_tasks,
                result.github_requests,
                result.submissions,
                result.rate_limited,
            )
            time.sleep(max(self.cfg.poll_seconds, result.retry_after_seconds))

    def _run_nudge_cycle(self, pending: list[HumanTask]) -> None:
        """Run the nudge cadence for pending tasks that have nudge transport.

        This method is idempotent and never blocks the main merge observation
        path.  It is a no-op when ``nudge_cfg`` is None or the nudge module
        is not available.
        """
        if self.nudge_cfg is None or _nudge_mod is None:
            return

        now = time.time()
        last_global_nudge = getattr(self, "_last_nudge_wall", 0.0)

        for task in pending:
            # Skip tasks already completed by the merge observation path.
            if task.status != STATUS_PENDING:
                continue

            nudge_state = task.nudge_state
            if nudge_state is None:
                # First nudge for this task — create a thread.
                try:
                    result = _nudge_mod.first_nudge(
                        channel_id=self.cfg.nudge_channel_id,
                        bot_token=self.cfg.nudge_bot_token,
                        instruction=task.instruction,
                        callback_url=task.callback_url,
                        callback_token=task.callback_token,
                        invocation_id=task.invocation_id,
                    )
                except Exception:  # noqa: BLE001 - best-effort, never block
                    logger.warning("first_nudge failed for %s", task.invocation_id, exc_info=True)
                    continue

                if result is None:
                    continue

                thread_id = result.get("thread_id")
                if not thread_id:
                    continue

                # Persist the new nudge state.
                task.nudge_state = {
                    "thread_id": thread_id,
                    "last_nudge_at": now,
                    "last_seen_message_id": "",
                    "escalation_level": 0,
                }
                try:
                    self.store.save(task)
                except Exception:  # noqa: BLE001
                    logger.warning(
                        "persisted nudge state failed for %s", task.invocation_id, exc_info=True
                    )
                continue

            # Task already has nudge state — check cadence.
            last_nudge_at = nudge_state.get("last_nudge_at", 0.0)
            elapsed = now - last_nudge_at
            if elapsed < self.cfg.nudge_interval_seconds:
                continue

            # Global throttle: don't send more than one nudge per interval.
            if elapsed - self.cfg.nudge_global_throttle_seconds < last_global_nudge:
                continue

            escalation_level = nudge_state.get("escalation_level", 0)
            escalation_after = self.cfg.nudge_escalation_after_seconds

            if elapsed >= escalation_after and escalation_level < 2:
                # Escalate: send a higher-priority nudge.
                try:
                    result = _nudge_mod.escalate(
                        channel_id=self.cfg.nudge_channel_id,
                        bot_token=self.cfg.nudge_bot_token,
                        thread_id=nudge_state["thread_id"],
                        instruction=task.instruction,
                        callback_url=task.callback_url,
                        callback_token=task.callback_token,
                        invocation_id=task.invocation_id,
                        escalation_level=escalation_level + 1,
                    )
                except Exception:  # noqa: BLE001
                    logger.warning("escalate failed for %s", task.invocation_id, exc_info=True)
                    continue

                if result is None:
                    continue

                nudge_state["last_nudge_at"] = now
                nudge_state["escalation_level"] = escalation_level + 1
                if result.get("message_id"):
                    nudge_state["last_seen_message_id"] = result["message_id"]
                try:
                    self.store.save(task)
                except Exception:  # noqa: BLE001
                    logger.warning(
                        "persisted escalation state failed for %s",
                        task.invocation_id,
                        exc_info=True,
                    )
                last_global_nudge = now
                continue

            # Normal cadence nudge.
            try:
                result = _nudge_mod.nudge(
                    channel_id=self.cfg.nudge_channel_id,
                    bot_token=self.cfg.nudge_bot_token,
                    thread_id=nudge_state["thread_id"],
                    instruction=task.instruction,
                    callback_url=task.callback_url,
                    callback_token=task.callback_token,
                    invocation_id=task.invocation_id,
                )
            except Exception:  # noqa: BLE001
                logger.warning("nudge failed for %s", task.invocation_id, exc_info=True)
                continue

            if result is None:
                continue

            nudge_state["last_nudge_at"] = now
            if result.get("message_id"):
                nudge_state["last_seen_message_id"] = result["message_id"]
            try:
                self.store.save(task)
            except Exception:  # noqa: BLE001
                logger.warning(
                    "persisted nudge state failed for %s", task.invocation_id, exc_info=True
                )
            last_global_nudge = now

        # Poll for replies on all threads that have nudge state.
        tasks_with_nudge = [t for t in pending if t.nudge_state is not None]
        for task in tasks_with_nudge:
            if task.status != STATUS_PENDING:
                continue
            nudge_state = task.nudge_state
            if nudge_state is None:
                continue
            thread_id = nudge_state.get("thread_id")
            if not thread_id:
                continue
            last_seen = nudge_state.get("last_seen_message_id", "")
            try:
                replies = _nudge_mod.poll_replies(
                    channel_id=self.cfg.nudge_channel_id,
                    bot_token=self.cfg.nudge_bot_token,
                    thread_id=thread_id,
                    last_message_id=last_seen,
                )
            except Exception:  # noqa: BLE001
                logger.warning("poll_replies failed for %s", task.invocation_id, exc_info=True)
                continue

            if not replies:
                continue

            for reply in replies:
                message_id = reply.get("message_id", "")
                if message_id and message_id > last_seen:
                    nudge_state["last_seen_message_id"] = message_id

                content = reply.get("content", "")
                if not content:
                    continue

                # A reply counts as a human submission.
                try:
                    self.bridge_submit(
                        self.cfg.bridge_url,
                        self.cfg.bridge_token,
                        task,
                        None,  # observation — nudge replies bypass this
                        "",  # merge_commit — not applicable
                        timeout_seconds=self.cfg.http_timeout_seconds,
                    )
                except Exception:  # noqa: BLE001
                    logger.warning(
                        "bridge submit for nudge reply failed for %s",
                        task.invocation_id,
                        exc_info=True,
                    )

            try:
                self.store.save(task)
            except Exception:  # noqa: BLE001
                logger.warning(
                    "persisted reply state failed for %s", task.invocation_id, exc_info=True
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
