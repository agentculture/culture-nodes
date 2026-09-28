import { useCallback, useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { setAgentState } from "../agent-state/store";
import {
  ApiError,
  listHumanTasks,
  listPendingDecisions,
  listReviewedRecords,
} from "../api/client";
import type { ReviewedRecord } from "../api/decisionTypes";
import type { HumanTask, PendingDecisionRun } from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import { DecidedTaskCard, PendingTaskCard } from "../components/HumanTaskCards";
import { SignedInAs } from "../components/IdentityGate";
import {
  ProposedRunDecision,
  RecordedDecisions,
  ReviewedRecordCard,
  type RecordedDecision,
} from "../components/ReviewDecision";
import SegmentedToggle from "../components/SegmentedToggle";
import {
  DEFAULT_INBOX_TAB,
  INBOX_TABS,
  type InboxTab,
  parseInboxTab,
  partitionInbox,
} from "../domain/inbox-tabs";
import { formatRelativeTime } from "../domain/run-board";
import type { SharedEventType } from "../hooks/useSharedEvents";
import { useSnapshotReconcile } from "../hooks/useSnapshotReconcile";
import { useWhoami } from "../hooks/useWhoami";

/**
 * Every event that changes what is awaiting a decision or what was decided:
 * a human task created or decided, a ledger record appended (a new proposal),
 * a review committed — any of them possibly from another tab or operator. A
 * stable module-level reference, as useSharedEvents requires (issue #46).
 */
const INBOX_EVENT_TYPES = [
  "dev.culture.nodes.human-task.created",
  "dev.culture.nodes.human-task.decided",
  "dev.culture.nodes.ledger.record-appended",
  "dev.culture.nodes.ledger.review-committed",
] as const satisfies readonly SharedEventType[];

const TAB_LABELS: Record<InboxTab, string> = {
  act: "To act",
  review: "To review",
  waiting: "Waiting",
  decided: "Decided",
};

const TAB_EMPTY: Record<InboxTab, string> = {
  act: "Nothing to act on. A run pauses here when it reaches an approval node.",
  review: "Nothing is awaiting a review. Every proposed record has been confirmed or rejected.",
  waiting: "Nothing waiting past its deadline.",
  decided: "Nothing decided yet.",
};

/** Mirrors the Mesh view's attribution-refresh discipline (Mesh.tsx). */
const REFRESH_DEBOUNCE_MS = 4000;

/** How many recent reviews the Decided tab reads. */
const REVIEWED_LIMIT = 50;

type DecidedEntry =
  | { kind: "task"; at: number; task: HumanTask }
  | { kind: "review"; at: number; item: ReviewedRecord };

const ms = (iso: string | undefined): number => {
  const parsed = iso ? Date.parse(iso) : Number.NaN;
  return Number.isNaN(parsed) ? 0 : parsed;
};

/** Backend page size and page ceiling when following a list's cursors. */
const PAGE_LIMIT = 500;
const MAX_PAGES = 40;

/**
 * Every pending task, following `next_cursor` (the Decisions page's pending
 * view did this; the one page keeps it): a queue longer than one backend
 * page must still be reachable, not silently cut at the first 50.
 */
async function allPendingTasks(signal: AbortSignal): Promise<HumanTask[]> {
  const items: HumanTask[] = [];
  let cursor: string | undefined;
  for (let page = 0; page < MAX_PAGES; page++) {
    const result = await listHumanTasks(signal, { status: "pending", limit: PAGE_LIMIT, cursor });
    items.push(...result.items);
    if (!result.next_cursor) break;
    cursor = result.next_cursor;
  }
  return items;
}

/**
 * Every undecided record, following cursors, with a run split across two
 * backend pages merged back into ONE group — a review is per run.
 */
async function allPendingRecords(
  signal: AbortSignal,
): Promise<{ groups: PendingDecisionRun[]; count: number }> {
  const byRun = new Map<string, PendingDecisionRun>();
  let count = 0;
  let cursor: string | undefined;
  for (let page = 0; page < MAX_PAGES; page++) {
    const result = await listPendingDecisions(signal, cursor ? { cursor } : undefined);
    count += result.record_count;
    for (const group of result.items) {
      const seen = byRun.get(group.run_id);
      if (seen) seen.records.push(...group.records);
      else byRun.set(group.run_id, { ...group, records: [...group.records] });
    }
    if (!result.next_cursor) break;
    cursor = result.next_cursor;
  }
  return { groups: [...byRun.values()], count };
}

const asApiError = (cause: unknown): ApiError =>
  cause instanceof ApiError
    ? cause
    : new ApiError(0, String(cause), "check the browser console");

/**
 * The Inbox (`/inbox`): the ONE place a person decides (task t46, owner
 * decision d19 — "one place to decide"). It used to be two pages: this one
 * for human tasks (task t14) and Decisions (`/decisions`, task t30) for
 * proposed ledger records. `/decisions` now redirects to the To review tab.
 *
 * Four tabs, persisted as `?tab=`:
 *
 *   - **To act** (default; t44's `?tab=open` still lands here): pending human
 *     tasks that can be decided now — approvals, blocked-agent asks, notices.
 *     Each card offers exactly the task's allowed outcomes and an optional
 *     note recorded with the decision (components/HumanTaskCards.tsx).
 *   - **To review**: every proposed ledger record no review has decided
 *     (`GET /v1alpha1/pending-decisions`), grouped by run, the payload in
 *     full, a verdict per record and a REQUIRED rationale — the Decisions
 *     page's form, moved intact (components/ReviewDecision.tsx).
 *   - **Waiting**: pending tasks past their deadline, and tasks offering no
 *     outcome a person may select ("cannot be decided here") — t44/t45.
 *   - **Decided**: decided and expired tasks, with their notes, and the most
 *     recent committed reviews (`GET /v1alpha1/reviewed-records`), newest
 *     first, read-only.
 *
 * Who decides is not part of any form (task t9): the decider is the actor
 * `useWhoami` says the signed-in principal is bound to, and an unbound or
 * signed-out state disables every submit. A decision here is a confirmed,
 * human-authority ledger record; the records a review names are never
 * rewritten.
 */
export interface InboxProps {
  /** Test seam for the deadline split and relative times; defaults to now. */
  now?: Date;
}

export function Inbox({ now }: InboxProps = {}) {
  const [searchParams, setSearchParams] = useSearchParams();
  const tab = parseInboxTab(searchParams.get("tab"));
  const setTab = (next: InboxTab) => {
    setSearchParams((prev) => {
      const params = new URLSearchParams(prev);
      if (next === DEFAULT_INBOX_TAB) params.delete("tab");
      else params.set("tab", next);
      return params;
    });
  };
  const [pending, setPending] = useState<HumanTask[] | null>(null);
  const [decided, setDecided] = useState<HumanTask[] | null>(null);
  const [groups, setGroups] = useState<PendingDecisionRun[] | null>(null);
  const [recordCount, setRecordCount] = useState(0);
  const [reviewed, setReviewed] = useState<ReviewedRecord[] | null>(null);
  const [reviewedMore, setReviewedMore] = useState(false);
  // Reviews recorded in this sitting, kept at page level: a decided run
  // leaves the pending list on the next refresh, and a confirmation rendered
  // inside its card would vanish with it (found against a live plane, t30).
  const [recorded, setRecorded] = useState<RecordedDecision[]>([]);
  const [error, setError] = useState<ApiError | null>(null);
  const [reviewError, setReviewError] = useState<ApiError | null>(null);
  const whoami = useWhoami();
  // Bumped after a recorded decision and by a debounced event on the shared
  // stream. Reloads never null the lists first (stale-while-revalidate), so
  // a card's "decision recorded" confirmation survives the refresh.
  const [reloadKey, setReloadKey] = useState(0);
  const reloadTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const lastReload = useRef(0);

  const scheduleReload = useCallback(() => {
    if (reloadTimer.current) return;
    const elapsed = Date.now() - lastReload.current;
    const wait = Math.max(0, REFRESH_DEBOUNCE_MS - elapsed);
    reloadTimer.current = setTimeout(() => {
      reloadTimer.current = undefined;
      lastReload.current = Date.now();
      setReloadKey((key) => key + 1);
    }, wait);
  }, []);

  useEffect(
    () => () => {
      if (reloadTimer.current) clearTimeout(reloadTimer.current);
    },
    [],
  );

  const { resolveSnapshot } = useSnapshotReconcile(INBOX_EVENT_TYPES, scheduleReload);

  useEffect(() => {
    const controller = new AbortController();
    const aborted = () => controller.signal.aborted;
    // "ready" means initial-load-settled and must never regress to
    // "loading" on a refresh (task t30's hard convention).
    const isInitialLoad = reloadKey === 0;
    if (isInitialLoad) setAgentState({ status: "loading", run: null });
    setError(null);
    setReviewError(null);

    const tasks = Promise.all([
      allPendingTasks(controller.signal),
      listHumanTasks(controller.signal, { status: "decided" }),
      listHumanTasks(controller.signal, { status: "expired" }),
    ])
      .then(([pendingList, decidedList, expiredList]) => {
        if (aborted()) return;
        setPending(pendingList);
        // Decided and expired are both terminal; the Decided tab shows both.
        setDecided([...decidedList.items, ...expiredList.items]);
      })
      .catch((cause: unknown) => {
        if (aborted()) return;
        setPending((prev) => prev ?? []);
        setDecided((prev) => prev ?? []);
        setError(asApiError(cause));
      });

    // The review half fails on its own: a control plane without the
    // reviewed-records read must not blank the tasks a person can act on.
    const reviews = Promise.all([
      allPendingRecords(controller.signal),
      listReviewedRecords(controller.signal, { limit: REVIEWED_LIMIT }),
    ])
      .then(([pendingRecords, reviewedList]) => {
        if (aborted()) return;
        setGroups(pendingRecords.groups);
        setRecordCount(pendingRecords.count);
        setReviewed(reviewedList.items);
        setReviewedMore(Boolean(reviewedList.next_cursor));
      })
      .catch((cause: unknown) => {
        if (aborted()) return;
        setGroups((prev) => prev ?? []);
        setReviewed((prev) => prev ?? []);
        setReviewError(asApiError(cause));
      });

    void Promise.all([tasks, reviews]).then(() => {
      if (aborted()) return;
      if (isInitialLoad) resolveSnapshot();
      setAgentState({ status: "ready", run: null });
    });
    return () => controller.abort();
  }, [reloadKey, resolveSnapshot]);

  const actorId = whoami.status === "bound" ? whoami.actorId : null;
  const loaded = pending !== null && decided !== null && groups !== null && reviewed !== null;
  const clock = now ?? new Date();
  const tabs = partitionInbox([...(pending ?? []), ...(decided ?? [])], clock);

  const decidedEntries: DecidedEntry[] = [
    ...tabs.decided.map((task) => ({
      kind: "task" as const,
      at: ms(task.resolved_at ?? task.created_at),
      task,
    })),
    ...(reviewed ?? []).map((item) => ({
      kind: "review" as const,
      at: ms(item.reviewed_at),
      item,
    })),
  ].sort((a, b) => b.at - a.at);

  const counts: Record<InboxTab, string> = {
    act: String(tabs.act.length),
    review: String(recordCount),
    waiting: String(tabs.waiting.length),
    decided: `${decidedEntries.length}${reviewedMore ? "+" : ""}`,
  };

  const refresh = () => setReloadKey((key) => key + 1);

  return (
    <section className="view-rail inbox-view">
      <h1>Inbox</h1>
      <p className="muted">
        The one place to decide. Human tasks the control plane is waiting on,
        and agents&apos; proposed records awaiting a review. An agent saying it
        is done is a claim, not evidence — a decision here is a human&apos;s,
        recorded as a confirmed ledger record naming who decided and why.
      </p>

      <SignedInAs verb="Deciding" whoami={whoami} />

      {error ? <ErrorNotice error={error} /> : null}
      {reviewError ? <ErrorNotice error={reviewError} /> : null}
      <RecordedDecisions recorded={recorded} />

      {!loaded ? (
        <p className="muted" id="inbox-loading">
          Loading inbox…
        </p>
      ) : (
        <>
          <SegmentedToggle id="inbox-tabs" label="Inbox filter">
            {INBOX_TABS.map((name) => (
              <button
                key={name}
                type="button"
                id={`inbox-tab-${name}`}
                aria-pressed={tab === name}
                onClick={() => setTab(name)}
              >
                {TAB_LABELS[name]}{" "}
                <span className="inbox-tab__count" data-count={counts[name]}>
                  {counts[name]}
                </span>
              </button>
            ))}
          </SegmentedToggle>

          {tab === "review" ? (
            (groups ?? []).length === 0 ? (
              <p className="muted" id="inbox-tab-empty">
                {TAB_EMPTY.review}
              </p>
            ) : (
              <>
                <p className="muted" id="decisions-count">
                  {recordCount} record(s) awaiting a review across {(groups ?? []).length}{" "}
                  run(s). The rationale is required: a confirmation with no stated
                  reason cannot be told apart from an unread one.
                </p>
                <ul className="decisions-list" id="inbox-review">
                  {(groups ?? []).map((group) => (
                    <ProposedRunDecision
                      key={group.run_id}
                      group={group}
                      actorId={actorId}
                      onDecided={(entry) => {
                        setRecorded((current) => [entry, ...current]);
                        refresh();
                      }}
                    />
                  ))}
                </ul>
              </>
            )
          ) : tab === "decided" ? (
            decidedEntries.length === 0 ? (
              <p className="muted" id="inbox-tab-empty">
                {TAB_EMPTY.decided}
              </p>
            ) : (
              <ul className="inbox-list" id="inbox-decided">
                {decidedEntries.map((entry) =>
                  entry.kind === "task" ? (
                    <DecidedTaskCard key={entry.task.id} task={entry.task} />
                  ) : (
                    <ReviewedRecordCard
                      key={entry.item.review_record_id}
                      item={entry.item}
                    />
                  ),
                )}
              </ul>
            )
          ) : tabs[tab].length === 0 ? (
            <p className="muted" id="inbox-tab-empty">
              {TAB_EMPTY[tab]}
            </p>
          ) : (
            <ul className="inbox-list" id={`inbox-${tab}`}>
              {tabs[tab].map((task) => (
                <PendingTaskCard
                  key={task.id}
                  task={task}
                  actorId={actorId}
                  overdueSince={
                    tab === "waiting" && task.request?.deadline
                      ? formatRelativeTime(task.request.deadline, clock)
                      : undefined
                  }
                  onDecided={refresh}
                />
              ))}
            </ul>
          )}
        </>
      )}
    </section>
  );
}

export default Inbox;
