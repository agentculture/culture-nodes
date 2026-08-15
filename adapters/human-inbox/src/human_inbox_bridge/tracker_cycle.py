"""Polling-cycle orchestration for the human-inbox GitHub tracker.

This module isolates the stateful, budget-aware polling loop from tracker.py's
configuration, observation parsing, HTTP operations, and command entry point.
The split makes the fairness and rate-limit policy readable as one concern;
tracker.py re-exports MergeTracker so existing callers keep their public path.
"""

from __future__ import annotations

import time
import urllib.error
from typing import Any, Callable

from human_inbox_bridge.store import STATUS_PENDING, HumanTask, TaskStore

from .tracker import (
    CLOSED_OBSERVATION_KIND,
    OBSERVATION_KIND,
    REPLY_OBSERVATION_KIND,
    BridgeSubmit,
    CycleResult,
    GithubFetch,
    Observation,
    ReplyObservation,
    TrackerConfig,
    _github_rate_limit_backoff,
    _nudge_mod,
    fetch_github_issue_comments,
    fetch_github_pull,
    logger,
    observation_for,
    qualifying_reply,
    reply_observation_for,
    submit_observation,
    submit_reply_observation,
)


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
