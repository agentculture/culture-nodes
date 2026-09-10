"""What the PR-upkeep sweep dispatches this tick, and under what cursor.

The third module the sweep's runner fetches (its own granted URL + sha256,
`PR_UPKEEP_SWEEP_EMIT_SOURCE_URL` / `_SHA256`). Like ``pr_upkeep_jira`` it is
a *decision* module: it holds no credential, opens no socket, and writes
nothing — ``sweep.py`` does the reads and remains the sole event emitter, and
hands the run listing here to be judged.

# The whole invariant, in two clauses

A finding is emitted only when BOTH hold:

1. **No run is working it.** Not at this head sha, not at any other — a run in
   flight is a piece of work in flight, and a second dispatch of the same
   finding is a second `human-merges-pr` approval for one change. On prod
   `pr236-qodo-1` once sat in four running runs at once.
2. **No run has ALREADY worked it at THIS head sha.** A run that ended is not
   an invitation to try again: the actor may have answered `no_change` ("not
   actually a defect"), or a person may have rejected the fix. The finding is
   still open on the source surface either way, and dispatching it again at
   the same commit re-buys an answer that was already given.

Clause 2 is the one that is easy to lose, and losing it costs money. Until
issue #268 the watermark alone enforced it by accident: a fact's cursor was
`{head_sha, newest_comment_at}`, identical on every tick, so the control plane
answered every repeat with `duplicate=true` (`deliverSignalEventTx` compares
the whole watermark for equality against the single row stored per source
key). Adding the dispatched finding id to the cursor — which #268 had to do,
or a PR's second finding could never be dispatched at an unmoved head —
removed that accident. Two findings that both end `no_change` would then
alternate forever, one billable agent session every 30 minutes, each one
re-working a finding an actor had already declined. So the clause the cursor
used to imply is now stated here and enforced against the run listing.

The net effect is exactly the old contract plus the one thing #268 wanted:
**nothing new until the PR moves, except a finding nobody has worked yet.**
"""

from __future__ import annotations

import re
import urllib.parse

_ISSUE_KEY_RE = re.compile(r"(?<![A-Z0-9])([A-Z][A-Z0-9]+-\d+)(?![A-Z0-9])")


def _correlated_issue_key(pull: dict, jira_project: str | None = None) -> str | None:
    head = (pull.get("head") or {}).get("ref") or ""
    pattern = _ISSUE_KEY_RE
    if jira_project:
        pattern = re.compile(r"(?<![A-Z0-9])(" + re.escape(jira_project) + r"-\d+)(?![A-Z0-9])")
    match = pattern.search(head) or pattern.search(pull.get("body") or "")
    return match.group(1) if match else None


def opened_pr_fact(pull: dict, repository: str, jira_project: str | None = None) -> dict | None:
    """Build the once-per-PR board fact for a correlatable open PR."""
    if pull.get("state") not in (None, "open"):
        return None
    issue_key = _correlated_issue_key(pull, jira_project)
    opened_at = pull.get("created_at")
    if not issue_key or not opened_at:
        return None
    return {
        "source": "github_pr",
        "repository": repository,
        "number": pull.get("number"),
        "url": pull.get("html_url") or "",
        "opened_at": opened_at,
        "issue_key": issue_key,
    }


def merged_pr_fact(pull: dict, repository: str, jira_project: str | None = None) -> dict | None:
    """Build the freeze fact, correlating by head branch first, then body."""
    if not pull.get("merged_at"):
        return None
    issue_key = _correlated_issue_key(pull, jira_project)
    if not issue_key:
        return None
    return {
        "source": "github_pr",
        "repository": repository,
        "number": pull.get("number"),
        "url": pull.get("html_url") or "",
        "merged_at": pull["merged_at"],
        "issue_key": issue_key,
    }


def work_item_for_pull(pull: dict, repository: str, jira_project: str | None = None) -> str:
    """The key of the work item a PR belongs to. Never empty.

    Two shapes, and only two (spec c41/c42, issue #310): the correlated Jira
    key -- head branch first, then PR body, narrowed to `jira_project` when one
    is configured, the same correlation `pr.opened`/`pr.merged` already use --
    or else the TRANSIENT ``gh:<owner>/<repo>#<n>`` form. The gh form is a
    placeholder that never survives intake: a PR without a ticket gets an
    orphan ticket created and the item re-keyed to it (t4), so anything that
    joins on the work item downstream sees a Jira key.

    Non-empty by construction because the engine stamps a minted run's
    ``work_item`` only from a non-empty string (internal/engine/trigger.go
    ``workItemFromPayload``): an empty value would mint a run that
    ``GET /v1alpha1/runs?work_item=`` can never find.
    """
    return _correlated_issue_key(pull, jira_project) or f"gh:{repository}#{pull.get('number')}"


def upkeep_pr_fact(
    pull: dict, repository: str, dispatched: list[dict], jira_project: str | None = None
) -> dict:
    """Build the pr-upkeep.pr payload: one PR, one finding, one work item.

    This is the run input verbatim (a triggered run's input IS the event
    payload), so every key here must be admitted by workflow.yaml's input
    contract, which is ``additionalProperties: false``. It carries
    ``work_item`` and deliberately NO ``subject``: subject re-enters the
    one-active-run-per-subject guard #268 removed, and no ``category`` --
    the work item is its own run column (decision c41), not a category.
    """
    return {
        "source": "github_pr",
        "repository": repository,
        "number": pull.get("number"),
        "head_sha": pull.get("head_sha") or "",
        "findings": dispatched,
        "work_item": work_item_for_pull(pull, repository, jira_project),
    }


def closed_pr_fact(pull: dict, repository: str, jira_project: str | None = None) -> dict | None:
    """Build the pr.closed fact for a PR closed WITHOUT merge (t12; spec c37).

    The human-inbox tracker has observed close-without-merge since #71
    (``github_pr_closed``, tracker.py CLOSED_OBSERVATION_KIND) but nothing
    ever turned it into a fact, so a declined PR's parked upkeep runs stayed
    running forever. This is the fact; its consumer -- the cleanup node that
    cancels those runs with reason ``pr_closed`` and reports the PR's refs --
    is task t13, not this module.

    Mirrors ``merged_pr_fact`` with three deliberate differences: a merged PR
    is never closed-unmerged (``merged_at`` wins, whatever ``state`` says);
    the correlation is ``work_item_for_pull`` rather than the bare Jira key,
    so a ticketless PR still produces a fact (its runs are just as parked);
    and ``closed_at`` doubles as the immutable watermark, so a PR GitHub
    reports closed without a timestamp yields no fact rather than one with
    no cursor. Closed pulls arrive raw from GitHub (``head.sha``), open ones
    normalised (``head_sha``); both shapes are read.
    """
    if pull.get("merged_at") or pull.get("state") != "closed":
        return None
    closed_at = pull.get("closed_at")
    if not closed_at:
        return None
    fact = {
        "source": "github_pr",
        "repository": repository,
        "number": pull.get("number"),
        "head_sha": pull.get("head_sha") or (pull.get("head") or {}).get("sha") or "",
        "closed_at": closed_at,
        "work_item": work_item_for_pull(pull, repository, jira_project),
    }
    if pull.get("html_url"):
        fact["url"] = pull["html_url"]
    return fact


def closed_pull_event(
    pull: dict, repository: str, jira_project: str | None = None
) -> tuple[str, dict, str, dict, str] | None:
    """One closed-listing PR -> ``(name, payload, source_key, watermark, subject)``.

    GitHub's ``state=closed`` listing holds merged and declined PRs alike, and
    exactly one lifecycle fact applies to each: ``pr.merged`` when
    ``merged_at`` is set, ``pr.closed`` otherwise. A merged PR that
    correlates to no ticket stays silent (the pre-t12 rule) and does NOT fall
    through to ``pr.closed``. Both facts are re-emitted every pass by design:
    the control plane keys on source_key + watermark and answers a repeat
    with ``duplicate=true`` (internal/store/postgres/signal.go), and both
    watermarks are immutable timestamps, so consumers see one fact per
    closure, not one per sweep. Unlike ``pr-upkeep.pr``, both carry a
    ``subject`` -- they mint no run, so the one-active-run-per-subject guard
    #268 removed is not re-entered.
    """
    number = pull.get("number")
    merged = merged_pr_fact(pull, repository, jira_project)
    if merged is not None:
        key = f"github:{repository}:pr:{number}:merged"
        return "pr.merged", merged, key, {"merged_at": merged["merged_at"]}, merged["issue_key"]
    closed = closed_pr_fact(pull, repository, jira_project)
    if closed is not None:
        key = f"github:{repository}:pr:{number}:closed"
        return "pr.closed", closed, key, {"closed_at": closed["closed_at"]}, closed["work_item"]
    return None


#: The workflow whose runs the dedupe consults, one page and at most how many
#: pages of it. The control plane's run listing is cursor-paginated and
#: newest-first, capped at 500 rows a page (`parseLimit(r, 50, 500)` in
#: internal/api/runs.go), so ONE page is not the population clause 2 asks
#: about: a PR can sit at an unmoved head while newer runs push the run that
#: already worked its finding off the first page, and that finding is then
#: dispatched — and paid for — a second time. The dedupe therefore follows
#: `next_cursor` (`next_run_cursor`) instead of reading one page.
#:
#: The page bound is what keeps a growing run history from turning every tick
#: into an unbounded walk. Past it the sweep can only re-emit (the pre-t12
#: behaviour), never wrongly suppress — a listing that cannot see a run cannot
#: claim its finding is in flight — and the sweep says on stderr that it
#: stopped early, because past that point clause 2 is no longer guaranteed.
PR_UPKEEP_WORKFLOW_KEY = "pr-upkeep"
RUNS_PAGE_LIMIT = 500
RUNS_MAX_PAGES = 20

#: The unit ONE emitted pr-upkeep fact carries: every undispatched finding on
#: the highest-priority finding's FILE (task t14; issue #309).
#:
#: This is the middle of three positions, and both ends are known to be wrong.
#: A fact carrying the WHOLE PR made every id on it undispatchable for as long
#: as the run holding it lived, which is until a human merges — one fix per PR
#: per merge, measured on PR #267 (issue #268). A fact carrying exactly ONE
#: finding fixed that and made bundling impossible: no run ever held two
#: findings, so nothing could see that thirteen of them were the same rule in
#: the same file, and each bought its own session, its own PR update and its
#: own approval.
#:
#: The file is the unit because it is the unit a fix actually has: one edit to
#: one file answers every same-rule finding in it at once, and the findings a
#: fix does NOT take are stale the moment it pushes — their line numbers moved.
#: So a file's findings are dispatched together, judged together by the
#: `analyse` node, and the ones outside the package it works are deferred to
#: the post-push re-scan rather than dispatched against a commit that no
#: longer exists. Findings on the PR's OTHER files are untouched by that and
#: stay dispatchable on the next tick, which is the property #268 bought.
FINDING_PACKAGE_KEY = "file"

#: The run states that mean "over" (engine.RunState.Terminal). Everything
#: else counts as in flight for clause 1 — `created` and `waiting` as well as
#: `running`, matching the engine's own ActiveRunStatuses.
#:
#: Qodo finding 2 on PR #269, and the direction is deliberate. Reading only
#: `running` as in flight was wrong twice over: `waiting` is a real
#: nonterminal state (a run frozen behind its ticket reaches it), and a state
#: a future control plane adds would silently land on the permissive side. An
#: unknown state therefore counts as IN FLIGHT — a finding wrongly held back
#: shows up in `skipped_findings` where a reader can see it, while a finding
#: wrongly released is a second billable session and a second approval for one
#: change, seen by nobody. The comment this replaces called the opposite "the
#: safe direction", which was exactly backwards.
TERMINAL_RUN_STATES = frozenset({"completed", "failed", "cancelled"})


def runs_query(limit: int = RUNS_PAGE_LIMIT, cursor: str = "") -> str:
    """The query string for one page of the run listing the dedupe needs.

    Deliberately NOT filtered to `state=running`: clause 2 is a question about
    runs that have ENDED, and a listing that cannot see them cannot answer it.

    `cursor` is a `next_cursor` handed straight back — the control plane calls
    it opaque and refuses anything it did not mint, so it is passed through
    url-encoded and never parsed here.
    """
    params = {"workflow_key": PR_UPKEEP_WORKFLOW_KEY}
    if cursor:
        params["cursor"] = cursor
    return f"{urllib.parse.urlencode(params)}&limit={limit}"


def next_run_cursor(listed: dict | None) -> str:
    """The cursor for the page after `listed`, or "" when it is the last one.

    A listing that omits `next_cursor`, or answers with something that is not
    a string, ends the walk: the alternative is a paging loop driven by a
    value the control plane never minted.
    """
    cursor = (listed or {}).get("next_cursor") if isinstance(listed, dict) else None
    return cursor if isinstance(cursor, str) else ""


def dispatched_finding_ids(listed: dict | list | None, repository: str = "") -> tuple[set, dict]:
    """The run listing -> (ids being worked now, {head_sha: ids worked there}).

    Takes one page or every page of the listing, because a page boundary is
    an artefact of how the listing is read and says nothing about a finding:
    both clauses are answered over the union.

    A triggered run's input IS the event payload, so `input.findings` is
    exactly what was dispatched for it and `input.head_sha` is the commit it
    was dispatched against. Malformed rows are skipped rather than raising:
    this is a dedupe, and one unreadable run must not stop a whole tick.

    `repository` scopes the answer to the one repo this cycle sweeps (Qodo
    finding 4 on PR #269). Finding ids are NOT repository-qualified —
    `pr236-qodo-1` names a PR number and an index, nothing more — so two
    configured repositories sharing a commit sha and a PR number would answer
    each other's questions, and a fork and its upstream share commit shas by
    construction. A run that declares no repository is kept rather than
    filtered: it cannot be ruled out, and for a dedupe the safe direction is
    to suppress.
    """
    pages = [listed] if isinstance(listed, dict) else (listed or [])
    in_flight: set = set()
    by_head: dict = {}
    for page in pages:
        if not isinstance(page, dict):
            continue
        for run in page.get("items") or []:
            run_input = run.get("input")
            if not isinstance(run_input, dict):
                continue
            run_repository = run_input.get("repository")
            if repository and isinstance(run_repository, str) and run_repository != repository:
                continue
            findings = run_input.get("findings")
            ids = {
                finding["id"]
                for finding in (findings if isinstance(findings, list) else [])
                if isinstance(finding, dict) and finding.get("id")
            }
            if not ids:
                continue
            if run.get("state") not in TERMINAL_RUN_STATES:
                in_flight |= ids
            head_sha = run_input.get("head_sha")
            if isinstance(head_sha, str) and head_sha:
                by_head.setdefault(head_sha, set()).update(ids)
    return in_flight, by_head


def finding_package_key(finding: dict) -> str | None:
    """Which package a finding belongs to, or None when it belongs to no one.

    The file it names (``FINDING_PACKAGE_KEY``). A finding with no file — a
    failed CI check run names a job, not a path — is its own package of one:
    nothing about it says another finding would be answered by the same edit.
    """
    return finding.get(FINDING_PACKAGE_KEY) or None


def finding_package(findings: list[dict]) -> list[dict]:
    """The findings ONE pr-upkeep.pr fact carries, off a PRIORITISED list.

    The highest-priority finding decides the file; the package is every
    finding on that file, in priority order. The rest of the PR is left for
    the next tick — this is a dispatch bound, not a triage: which of these
    findings is actually worth fixing is the `analyse` node's judgment, and
    the ones it does not package are deferred to the post-push re-scan.
    """
    if not findings:
        return []
    key = finding_package_key(findings[0])
    if key is None:
        return findings[:1]
    return [finding for finding in findings if finding_package_key(finding) == key]


def _held_package_keys(findings: list[dict], in_flight: set, worked_at_head: set) -> tuple:
    """(keys held in flight, keys settled at this head) — from ANY member."""
    flying, settled = set(), set()
    for finding in findings:
        key = finding_package_key(finding)
        if key is None:
            continue
        if finding.get("id") in in_flight:
            flying.add(key)
        elif finding.get("id") in worked_at_head:
            settled.add(key)
    return flying, settled - flying


def undispatched_findings(
    findings: list[dict], in_flight: set, worked_at_head: set = frozenset()
) -> tuple:
    """Split findings into (emit these, in flight, already worked at this head).

    The two refusals are reported separately because they are different facts
    a reader would act on differently: `in flight` may be waiting on a human
    right now, while `worked at this head` is settled until the PR moves. A
    finding in both is reported as in flight — the more actionable of the two.

    Both refusals apply to the whole PACKAGE, not only to the finding that
    triggered them (task t14). A run holding one finding on a file is a fix
    being written for that file, and releasing the file's other findings
    would mint a second run pushing a second change to the same lines — the
    collision this lane already cannot serialise, arriving through a door
    #268's one-finding-per-fact rule used to keep shut. So a package is held
    whole and released whole, and the summary names every id it held, not
    just the one the control plane had already seen.
    """
    flying, settled = _held_package_keys(findings, in_flight, worked_at_head)
    kept, skipped, worked = [], [], []
    for finding in findings:
        finding_id = finding.get("id")
        key = finding_package_key(finding)
        if finding_id in in_flight or key in flying:
            skipped.append(finding_id)
        elif finding_id in worked_at_head or key in settled:
            worked.append(finding_id)
        else:
            kept.append(finding)
    return kept, skipped, worked


def pushback_findings(listed: dict | list | None, repository: str = "") -> list[dict]:
    """The PUSHBACK verdicts the `analyse` node recorded, off the run listing.

    A pushback is a finding a PERSON declined on the PR thread, so it is the
    one analysis verdict that has to leave the run: nobody watching the
    control plane is the person who wrote the reply, and the finding will be
    re-read from its source surface on every later tick regardless. The tick
    summary names it so the PR owner sees their own objection was heard,
    rather than watching the loop propose the same fix again.

    It costs no extra read: `fetch_dispatched_findings` already walks the
    whole `pr-upkeep` run listing for the dedupe, and a listing row carries
    the run's `output` beside its `input` (`internal/api/types.go`,
    `RunOut.Output`) — which, since workflow 2.5.0, IS the analysis document
    (the end node's `output.from` points at `/nodes/analyse/output`).

    Newest-first wins on a repeated id: the listing is newest-first, so the
    first verdict seen for a finding is the most recent judgment of it.
    Malformed rows are skipped rather than raising, for the same reason
    ``dispatched_finding_ids`` skips them — this is a report, and one
    unreadable run must not cost a whole tick.
    """
    pages = [listed] if isinstance(listed, dict) else (listed or [])
    pushbacks: list[dict] = []
    seen: set = set()
    for page in pages:
        if not isinstance(page, dict):
            continue
        for run in page.get("items") or []:
            if not isinstance(run, dict):
                continue
            run_input = run.get("input")
            run_repository = run_input.get("repository") if isinstance(run_input, dict) else None
            if repository and isinstance(run_repository, str) and run_repository != repository:
                continue
            output = run.get("output")
            verdicts = output.get("verdicts") if isinstance(output, dict) else None
            for verdict in verdicts if isinstance(verdicts, list) else []:
                if not isinstance(verdict, dict) or verdict.get("verdict") != "PUSHBACK":
                    continue
                finding_id = verdict.get("id")
                if not finding_id or finding_id in seen:
                    continue
                seen.add(finding_id)
                pushbacks.append(
                    {
                        "id": finding_id,
                        "reason": verdict.get("reason", ""),
                        "run_id": run.get("id", ""),
                    }
                )
    return pushbacks


def _comment_timestamp(comment: dict) -> str:
    """A comment's position for "newest" comparisons.

    Checks BOTH schemas the sweep reads comments from: GitHub issue-comment
    objects (`updated_at`/`created_at`) and Jira Cloud v3 comment objects
    (`updated`/`created` — no `_at` suffix). `updated` is preferred over
    `created`, matching the GitHub-side preference, so an edited comment
    still counts as the newest touch on the thread.
    """
    return str(
        comment.get("updated_at")
        or comment.get("created_at")
        or comment.get("updated")
        or comment.get("created")
        or ""
    )


def newest_comment_timestamp(comments: list[dict]) -> str:
    """The newest touch on a PR's comments — half of the emission cursor.

    It lives beside ``emission_watermark`` rather than in ``sweep.py``
    because it is a decision about what the cursor MEANS, not a read: the
    sweep fetches the comments, this module says which of them the watermark
    is. (It moved here in task t14, when the sweep's own file had no room
    left under the 1000-line hard limit — which is the signal the lane doc's
    "Changing the sweep" recipe says to read as *this concern belongs in a
    module of its own*.)
    """
    return max((_comment_timestamp(c) for c in comments), default="")


def emission_watermark(head_sha: str, newest_comment_at: str, findings: list[dict]) -> dict:
    """The cursor guarding one PR's pr-upkeep fact.

    Head sha and newest comment answer *did this PR move*; the dispatched
    finding id answers *is this a different piece of work*. The finding has to
    be in here because the control plane's duplicate check is an equality test
    of the whole watermark against the row stored for this source key
    (internal/store/postgres/signal.go), so at an unmoved head the second
    finding would be answered `duplicate=true` and never mint a run.

    A PR with no findings keeps the two-key watermark it has always had, so
    rolling this out does not re-deliver every clean PR's current head. The id
    is read with `.get` because a finding that reached here without one should
    cost this tick its cursor precision, not raise out of the emitter with a
    KeyError no `attempting` stage would name.
    """
    watermark = {"head_sha": head_sha, "newest_comment_at": newest_comment_at}
    if findings:
        watermark["finding"] = findings[0].get("id", "")
    return watermark
