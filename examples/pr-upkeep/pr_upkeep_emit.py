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

import urllib.parse

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

#: How many findings one emitted pr-upkeep fact carries: ONE — the one the fix
#: node will actually work ("take the HIGHEST-PRIORITY item ... and work only
#: that one item"). A fact carrying the whole list made every id on it
#: undispatchable for as long as the run holding it lived, which is until a
#: human merges (issue #268; README, "One finding per fact").
FINDINGS_PER_EVENT = 1

#: The run state that means "working it right now". Every other state — and a
#: run whose state a future control plane spells differently — is treated as
#: ended, which is the safe direction: an ended run only ever contributes to
#: clause 2, which is scoped to one head sha.
RUN_STATE_RUNNING = "running"


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


def dispatched_finding_ids(listed: dict | list | None) -> tuple[set, dict]:
    """The run listing -> (ids being worked now, {head_sha: ids worked there}).

    Takes one page or every page of the listing, because a page boundary is
    an artefact of how the listing is read and says nothing about a finding:
    both clauses are answered over the union.

    A triggered run's input IS the event payload, so `input.findings` is
    exactly what was dispatched for it and `input.head_sha` is the commit it
    was dispatched against. Malformed rows are skipped rather than raising:
    this is a dedupe, and one unreadable run must not stop a whole tick.
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
            findings = run_input.get("findings")
            ids = {
                finding["id"]
                for finding in (findings if isinstance(findings, list) else [])
                if isinstance(finding, dict) and finding.get("id")
            }
            if not ids:
                continue
            if run.get("state") == RUN_STATE_RUNNING:
                in_flight |= ids
            head_sha = run_input.get("head_sha")
            if isinstance(head_sha, str) and head_sha:
                by_head.setdefault(head_sha, set()).update(ids)
    return in_flight, by_head


def undispatched_findings(
    findings: list[dict], in_flight: set, worked_at_head: set = frozenset()
) -> tuple:
    """Split findings into (emit these, in flight, already worked at this head).

    The two refusals are reported separately because they are different facts
    a reader would act on differently: `in flight` may be waiting on a human
    right now, while `worked at this head` is settled until the PR moves. A
    finding in both is reported as in flight — the more actionable of the two.
    """
    kept, skipped, worked = [], [], []
    for finding in findings:
        finding_id = finding.get("id")
        if finding_id in in_flight:
            skipped.append(finding_id)
        elif finding_id in worked_at_head:
            worked.append(finding_id)
        else:
            kept.append(finding)
    return kept, skipped, worked


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
