import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { setAgentState } from "../agent-state/store";
import {
  ApiError,
  commitReview,
  createReview,
  decideHumanTask,
  getLedger,
  getRun,
  listHumanTasks,
  listPendingDecisions,
} from "../api/client";
import type { HumanTask, PendingDecisionRun, ReviewCommitResult } from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import { SignedInAs } from "../components/IdentityGate";
import OutcomeButtons from "../components/OutcomeButtons";
import RunDecisionCard, {
  RecordPayload,
  confirmAllVerdicts,
  recordsWithVerdict,
  type RecordVerdict,
  type RunVerdicts,
} from "../components/RunDecisionCard";
import { findTicketKey } from "../domain/ticket-key";
import type { SharedEventType } from "../hooks/useSharedEvents";
import { useSnapshotReconcile } from "../hooks/useSnapshotReconcile";
import { useWhoami } from "../hooks/useWhoami";

/**
 * Events that can change what is awaiting a decision: a new record appended,
 * or a review just committed (possibly by another operator). A stable
 * module-level reference, as useSharedEvents requires (issue #46).
 */
const DECISION_EVENT_TYPES = [
  "dev.culture.nodes.ledger.record-appended",
  "dev.culture.nodes.ledger.review-committed",
] as const satisfies readonly SharedEventType[];

/** Mirrors the Inbox/Mesh refresh discipline. */
const REFRESH_DEBOUNCE_MS = 4000;

/**
 * The Decisions view (`/decisions`, task t30 / issue #99): the affirmative
 * half of PRD §10.4, reachable by a human through the product.
 *
 * The refusal half — an agent may only propose, and no actor promotes its own
 * proposal — has always worked. What did not exist was a way for a person to
 * *make* the decision without hand-writing two authenticated HTTP calls. This
 * view lists every proposed record no review has decided, and lets a human
 * confirm or reject them: create a review over the run's records at the
 * ledger version this page read, then commit it with a verdict and a stated
 * rationale.
 *
 * Three properties this view refuses to fudge:
 *
 *   - The rationale is required by the form, as it is by the API. A
 *     confirmation with no stated reason cannot be told apart from an unread
 *     one.
 *   - The reviewer is the signed-in principal's actor, never typed (task
 *     t9, spec c8). Until then the token authenticated the deployment and
 *     the page asked for a reviewer id in free text; now Cloudflare Access
 *     verifies the person, `useWhoami` says which actor they are bound to,
 *     and the API refuses any reviewer the registry does not record as a
 *     human — a login bound to an agent actor still cannot confirm.
 *   - The record payload is rendered in full. A decider must read the claim,
 *     including the qualifying half of it — the same reason the operator's
 *     ledger output stopped truncating.
 *
 * Deciding does not change the records decided: the ledger appends a review
 * record naming each one, and the claim keeps reading `proposed` forever.
 * The list shrinks because the join changed, not because a record did.
 */
export function Decisions() {
  const [view, setView] = useState<"claims" | "pending">("claims");
  return (
    <>
      <nav aria-label="Decision views" className="decisions-tabs">
        <button type="button" onClick={() => setView("pending")} aria-pressed={view === "pending"}>
          Pending
        </button>
        <button type="button" onClick={() => setView("claims")} aria-pressed={view === "claims"}>
          Proposed claims
        </button>
      </nav>
      {view === "pending" ? <PendingDecisionsView /> : <ProposedClaimsView />}
    </>
  );
}

const PAGE_SIZE = 25;

function PendingDecisionsView() {
  const [tasks, setTasks] = useState<HumanTask[] | null>(null);
  const [claims, setClaims] = useState<PendingDecisionRun[]>([]);
  const [versions, setVersions] = useState<Record<string, number>>({});
  const [tickets, setTickets] = useState<Record<string, string>>({});
  const [page, setPage] = useState(0);
  const whoami = useWhoami();
  const [submitting, setSubmitting] = useState<string | null>(null);
  const [error, setError] = useState<ApiError | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    const allTasks = async () => {
      const items: HumanTask[] = [];
      let cursor: string | undefined;
      for (let page = 0; page < 40; page++) {
        const result = await listHumanTasks(controller.signal, { status: "pending", limit: 500, cursor });
        items.push(...result.items);
        if (!result.next_cursor) break;
        cursor = result.next_cursor;
      }
      return items;
    };
    const allClaims = async () => {
      const items: PendingDecisionRun[] = [];
      let cursor: string | undefined;
      for (let page = 0; page < 40; page++) {
        const result = await listPendingDecisions(controller.signal, { limit: 500, cursor });
        items.push(...result.items);
        if (!result.next_cursor) break;
        cursor = result.next_cursor;
      }
      return items;
    };
    Promise.all([allTasks(), allClaims()]).then(([taskItems, claimItems]) => {
      if (controller.signal.aborted) return;
      setTasks(taskItems);
      setClaims(claimItems);
      for (const group of claimItems) {
        setVersions((current) => ({ ...current, [group.run_id]: group.ledger_version }));
      }
      // A human task's guard version has to be READ; a claim group already
      // carries the version it was served at, so only the task runs are
      // fetched.
      for (const runID of new Set(taskItems.map((task) => task.run_id))) {
        getLedger(runID, controller.signal).then((ledger) => {
          if (!controller.signal.aborted)
            setVersions((current) => ({ ...current, [runID]: ledger.ledger_version }));
        }).catch(() => undefined);
      }
      // The ticket key, for BOTH halves: a task's card names the ticket it
      // belongs to, and since task t12 a claim group links to the ticket page
      // that decides it — so a claim-only run needs the lookup too.
      const runIDs = new Set([
        ...taskItems.map((task) => task.run_id),
        ...claimItems.map((group) => group.run_id),
      ]);
      for (const runID of runIDs) {
        getRun(runID, controller.signal).then((run) => {
          const ticket = findTicketKey(run.run.input);
          if (!controller.signal.aborted && ticket)
            setTickets((current) => ({ ...current, [runID]: ticket }));
        }).catch(() => undefined);
      }
    }).catch((cause: unknown) => {
      if (!controller.signal.aborted) {
        setTasks([]);
        setError(cause instanceof ApiError ? cause : new ApiError(0, String(cause), "check the browser console"));
      }
    });
    return () => controller.abort();
  }, []);

  const claimItems = claims.flatMap((group) =>
    group.records.map((record) => ({ group, record })),
  );
  const total = (tasks?.length ?? 0) + claimItems.length;
  const pageCount = Math.max(1, Math.ceil(total / PAGE_SIZE));
  const visiblePage = Math.min(page, pageCount - 1);
  useEffect(() => {
    if (page !== visiblePage) setPage(visiblePage);
  }, [page, visiblePage]);
  const combined = [
    ...(tasks ?? []).map((task) => ({ type: "task" as const, task })),
    ...claimItems.map((claim) => ({ type: "claim" as const, ...claim })),
  ];
  const visible = combined.slice(visiblePage * PAGE_SIZE, (visiblePage + 1) * PAGE_SIZE);
  const grouped = new Map<string, typeof visible>();
  for (const item of visible) {
    const runID = item.type === "task" ? item.task.run_id : item.group.run_id;
    grouped.set(runID, [...(grouped.get(runID) ?? []), item]);
  }
  const actorId = whoami.status === "bound" ? whoami.actorId : null;

  async function choose(task: HumanTask, outcome: string) {
    const ledgerVersion = versions[task.run_id];
    if (actorId === null || ledgerVersion === undefined) return;
    setSubmitting(task.id);
    setError(null);
    try {
      await decideHumanTask(task.id, {
        outcome,
        decider_actor_id: actorId,
        response: task.request.decision_schema_ref ? { outcome } : undefined,
        expected_ledger_version: ledgerVersion,
      });
      setTasks((current) => current?.filter((item) => item.id !== task.id) ?? []);
    } catch (cause) {
      setError(cause instanceof ApiError ? cause : new ApiError(0, String(cause), "check the browser console"));
    } finally {
      setSubmitting(null);
    }
  }

  return (
    <section className="view-rail decisions-view">
      <h1>Pending decisions</h1>
      <SignedInAs verb="Deciding" whoami={whoami} />
      {error ? <ErrorNotice error={error} /> : null}
      {tasks === null ? <p className="muted">Loading pending decisions…</p> : total === 0 ? <p className="muted">Nothing is awaiting a decision.</p> : (
        <>
          <p className="muted">Page {visiblePage + 1} of {pageCount}</p>
          {Array.from(grouped, ([runID, items]) => (
            <section key={runID} data-run-id={runID}>
              <h2>{tickets[runID] ? <>Ticket {tickets[runID]} · </> : null}Run <Link to={`/runs/${runID}`}>{runID}</Link></h2>
              {/* Claims are decided on the ticket page (task t12, spec c11).
                  This tab used to render a checkbox beside each one that
                  selected it into a verdict no form on this tab could submit —
                  an affordance that did nothing. It is replaced rather than
                  merely deleted (decision c33): the group links to the page
                  that CAN take the decision, or says plainly why it cannot. */}
              {items.some((item) => item.type === "claim") ? (
                tickets[runID] ? (
                  <p className="muted">
                    <Link to={`/tickets/${encodeURIComponent(tickets[runID])}`}>
                      Decide these claims on ticket {tickets[runID]}
                    </Link>
                  </p>
                ) : (
                  <p className="muted">
                    No ticket is recorded for this run — decide these claims
                    under Proposed claims.
                  </p>
                )
              ) : null}
              <ul className="decisions-list">
                {items.map((item) => item.type === "task" ? (
                  <li className="inbox-card" key={item.task.id} data-human-task-id={item.task.id} data-testid={`pending-task-${item.task.id}`}>
                    <code>{item.task.id}</code> · {item.task.kind}
                    <OutcomeButtons
                      taskId={item.task.id}
                      outcomes={item.task.request.allowed_outcomes ?? []}
                      disabled={actorId === null || versions[item.task.run_id] === undefined}
                      busy={submitting === item.task.id}
                      onChoose={(outcome) => void choose(item.task, outcome)}
                    />
                  </li>
                ) : (
                  <li className="inbox-card" key={item.record.id} data-record-id={item.record.id}>
                    <code>{item.record.id}</code> · {item.record.record_type}
                    <RecordPayload data={item.record.data} />
                  </li>
                ))}
              </ul>
            </section>
          ))}
          <div>
            <button type="button" disabled={visiblePage === 0} onClick={() => setPage((value) => value - 1)}>Previous page</button>{" "}
            <button type="button" disabled={visiblePage + 1 >= pageCount} onClick={() => setPage((value) => value + 1)}>Next page</button>
          </div>
        </>
      )}
    </section>
  );
}

function ProposedClaimsView() {
  const [groups, setGroups] = useState<PendingDecisionRun[] | null>(null);
  const [recordCount, setRecordCount] = useState(0);
  // Decisions recorded in this sitting, kept at page level rather than on the
  // card that made them. Found by driving this view against a live control
  // plane: a decided run leaves the pending list, so a confirmation rendered
  // inside its card disappears the moment the list refreshes — the operator
  // clicks "Record decision" and the card silently vanishes, which is
  // indistinguishable from the click having done nothing.
  const [recorded, setRecorded] = useState<RecordedDecision[]>([]);
  const [error, setError] = useState<ApiError | null>(null);
  const whoami = useWhoami();
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

  const { resolveSnapshot } = useSnapshotReconcile(
    DECISION_EVENT_TYPES,
    scheduleReload,
  );

  useEffect(() => {
    const controller = new AbortController();
    // "ready" must never regress to "loading" on a refresh: only the first
    // render reports loading, every later reload is stale-while-revalidate.
    if (reloadKey === 0) setAgentState({ status: "loading", run: null });
    setError(null);

    listPendingDecisions(controller.signal)
      .then((payload) => {
        if (controller.signal.aborted) return;
        setGroups(payload.items);
        setRecordCount(payload.record_count);
        if (reloadKey === 0) resolveSnapshot();
        setAgentState({ status: "ready", run: null });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setGroups((prev) => prev ?? []);
        setError(
          cause instanceof ApiError
            ? cause
            : new ApiError(0, String(cause), "check the browser console"),
        );
        if (reloadKey === 0) resolveSnapshot();
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
  }, [reloadKey, resolveSnapshot]);

  const actorId = whoami.status === "bound" ? whoami.actorId : null;

  return (
    <section className="view-rail decisions-view">
      <h1>Decisions</h1>
      <p className="muted">
        Every proposed record no review has decided. An agent saying it is done
        is a completion claim, not evidence — a decision here is a human&apos;s,
        recorded as its own ledger record naming who decided and why.
      </p>

      <SignedInAs verb="Reviewing" whoami={whoami} />

      {error ? <ErrorNotice error={error} /> : null}

      {recorded.length > 0 ? (
        <ul className="decisions-recorded" id="decisions-recorded" role="status">
          {recorded.map((entry) => (
            <li key={entry.reviewId}>
              Recorded a <strong>{entry.verdict}</strong> decision on{" "}
              {entry.recordCount} record(s) of run <code>{entry.runId}</code> —
              review{" "}
              <code>{entry.reviewId}</code>, ledger now at version{" "}
              {entry.ledgerVersion}. The records decided are unchanged: a
              review names them, it never rewrites them.
            </li>
          ))}
        </ul>
      ) : null}

      {groups === null ? (
        <p className="muted" id="decisions-loading">
          Loading pending decisions…
        </p>
      ) : groups.length === 0 ? (
        <p className="muted" id="decisions-empty">
          Nothing is awaiting a decision. Every proposed record has been
          confirmed or rejected.
        </p>
      ) : (
        <>
          <p className="muted" id="decisions-count">
            {recordCount} record(s) awaiting a decision across {groups.length}{" "}
            run(s).
          </p>
          <ul className="decisions-list" id="decisions-runs">
            {groups.map((group) => (
              <ProposedRunDecision
                key={group.run_id}
                group={group}
                actorId={actorId}
                onDecided={(entry) => {
                  setRecorded((current) => [entry, ...current]);
                  setReloadKey((key) => key + 1);
                }}
              />
            ))}
          </ul>
        </>
      )}
    </section>
  );
}

/** One recorded decision, kept above the list after its run leaves it. */
interface RecordedDecision {
  reviewId: string;
  runId: string;
  verdict: "confirm" | "reject" | "mixed";
  recordCount: number;
  ledgerVersion: number;
}

/**
 * One run's undecided records and the form that decides them.
 *
 * The rendering is the shared `RunDecisionCard` (task t12) — the ticket page
 * shows the same records, and two renderings of the claim a decider reads
 * before confirming it are two chances for one of them to summarise away the
 * qualifying half. What lives here is only what is different: this queue
 * decides ONE run per submit, so the rationale and the button belong to the
 * card.
 *
 * The verdict is per record, which is the grain
 * `POST /v1alpha1/reviews/{id}/commit` decides at: a run whose claim holds up
 * and whose evidence does not is one review with two answers, not two
 * reviews. A record left at "not now" is simply not named by this review and
 * stays awaiting a decision.
 */
function ProposedRunDecision({
  group,
  actorId,
  onDecided,
}: {
  group: PendingDecisionRun;
  /** The signed-in principal's actor, or null when nothing can be recorded. */
  actorId: string | null;
  onDecided: (entry: RecordedDecision) => void;
}) {
  const [verdicts, setVerdicts] = useState<RunVerdicts>(() =>
    confirmAllVerdicts(group),
  );
  const [rationale, setRationale] = useState("");
  const [submitError, setSubmitError] = useState<ApiError | null>(null);
  const [result, setResult] = useState<ReviewCommitResult | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const confirmed = recordsWithVerdict(group, verdicts, "confirm");
  const rejected = recordsWithVerdict(group, verdicts, "reject");
  const decided = [...confirmed, ...rejected];

  const canSubmit =
    actorId !== null &&
    !submitting &&
    result === null &&
    decided.length > 0 &&
    rationale.trim() !== "";

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!canSubmit || actorId === null) return;

    setSubmitError(null);
    setSubmitting(true);
    try {
      // The ledger version submitted is the one this page READ, never a
      // fresh fetch: that is what makes the stale guard meaningful. If the
      // run moved since the list loaded, the API refuses the commit with a
      // 409 and writes nothing, which is the correct outcome — the frame
      // this operator read is no longer the frame they would be deciding.
      const review = await createReview(group.run_id, {
        record_ids: decided,
        ledger_version: group.ledger_version,
        reviewer_actor_id: actorId,
      });
      const decisions: Record<string, "confirm" | "reject"> = {};
      for (const id of confirmed) decisions[id] = "confirm";
      for (const id of rejected) decisions[id] = "reject";

      const committed = await commitReview(review.id, {
        decisions,
        expected_ledger_version: group.ledger_version,
        rationale: rationale.trim(),
      });
      setResult(committed);
      onDecided({
        reviewId: committed.review_id,
        runId: group.run_id,
        verdict: rejected.length === 0 ? "confirm" : confirmed.length === 0 ? "reject" : "mixed",
        recordCount: committed.records.length,
        ledgerVersion: committed.ledger_version,
      });
    } catch (cause) {
      setSubmitError(
        cause instanceof ApiError
          ? cause
          : new ApiError(0, String(cause), "check the browser console"),
      );
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <RunDecisionCard
      group={group}
      verdicts={verdicts}
      onVerdictChange={(recordId: string, verdict: RecordVerdict) =>
        setVerdicts((current) => ({ ...current, [recordId]: verdict }))
      }
      disabled={actorId === null || submitting}
      reviewedRecordIds={result === null ? [] : decided}
    >
      {result === null ? (
        <form className="inbox-card__form" onSubmit={submit}>
          <div className="inbox-card__field">
            <label htmlFor={`rationale-${group.run_id}`}>
              Why (recorded on the decision)
            </label>
            <textarea
              id={`rationale-${group.run_id}`}
              rows={2}
              value={rationale}
              onChange={(event) => setRationale(event.target.value)}
            />
          </div>
          {submitError ? <ErrorNotice error={submitError} /> : null}
          <button
            type="submit"
            className="author-workflow__button author-workflow__button--primary"
            disabled={!canSubmit}
          >
            Record decision
          </button>
        </form>
      ) : (
        <p className="inbox-card__result" role="status">
          decision recorded — {result.records.length} review record(s) appended
          by review <code>{result.review_id}</code>; the run&apos;s ledger is
          now at version {result.ledger_version}. The records decided are
          unchanged: a review names them, it never rewrites them.
        </p>
      )}
    </RunDecisionCard>
  );
}

export default Decisions;
