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
import {
  clearDecisionToken,
  getDecisionToken,
  setDecisionActorID,
  setDecisionToken,
} from "../api/decision-token";
import type { HumanTask, PendingDecisionRun, ReviewCommitResult } from "../api/types";
import AuthorityChip from "../components/AuthorityChip";
import DeciderActorField, { useDeciderActorID } from "../components/DeciderActorField";
import ErrorNotice from "../components/ErrorNotice";
import OutcomeButtons from "../components/OutcomeButtons";
import { useSharedEvents, type SharedEventType } from "../hooks/useSharedEvents";

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
 *   - The reviewer is typed in, never inferred. The token authenticates the
 *     deployment, not the person; who decided is a separate, explicit claim,
 *     and the API refuses any reviewer the registry does not record as a
 *     human.
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
  const [actorID, setActorID] = useDeciderActorID();
  const [tokenHeld, setTokenHeld] = useState(getDecisionToken() !== null);
  const [submitting, setSubmitting] = useState<string | null>(null);
  const [error, setError] = useState<ApiError | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    Promise.all([
      listHumanTasks(controller.signal, { status: "pending", limit: 500 }),
      listPendingDecisions(controller.signal, { limit: 500 }),
    ]).then(([taskList, claimList]) => {
      if (controller.signal.aborted) return;
      setTasks(taskList.items);
      setClaims(claimList.items);
      for (const group of claimList.items) {
        setVersions((current) => ({ ...current, [group.run_id]: group.ledger_version }));
      }
      for (const runID of new Set(taskList.items.map((task) => task.run_id))) {
        getLedger(runID, controller.signal).then((ledger) => {
          if (!controller.signal.aborted)
            setVersions((current) => ({ ...current, [runID]: ledger.ledger_version }));
        }).catch(() => undefined);
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
  const combined = [
    ...(tasks ?? []).map((task) => ({ type: "task" as const, task })),
    ...claimItems.map((claim) => ({ type: "claim" as const, ...claim })),
  ];
  const visible = combined.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE);
  const grouped = new Map<string, typeof visible>();
  for (const item of visible) {
    const runID = item.type === "task" ? item.task.run_id : item.group.run_id;
    grouped.set(runID, [...(grouped.get(runID) ?? []), item]);
  }
  const token = tokenHeld ? getDecisionToken() : null;

  async function choose(task: HumanTask, outcome: string) {
    const ledgerVersion = versions[task.run_id];
    if (!token || !actorID.trim() || ledgerVersion === undefined) return;
    setDecisionActorID(actorID.trim());
    setSubmitting(task.id);
    setError(null);
    try {
      await decideHumanTask(task.id, {
        outcome,
        decider_actor_id: actorID.trim(),
        response: task.request.decision_schema_ref ? { outcome } : undefined,
        expected_ledger_version: ledgerVersion,
      }, token);
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
      <TokenPanel held={tokenHeld} onHold={(value) => { setDecisionToken(value); setTokenHeld(true); }} onClear={() => { clearDecisionToken(); setTokenHeld(false); }} />
      <DeciderActorField id="pending-decider-actor" value={actorID} onChange={setActorID} />
      {error ? <ErrorNotice error={error} /> : null}
      {tasks === null ? <p className="muted">Loading pending decisions…</p> : total === 0 ? <p className="muted">Nothing is awaiting a decision.</p> : (
        <>
          <p className="muted">Page {page + 1} of {pageCount}</p>
          {Array.from(grouped, ([runID, items]) => (
            <section key={runID} data-run-id={runID}>
              <h2>{tickets[runID] ? <>Ticket {tickets[runID]} · </> : null}Run <Link to={`/runs/${runID}`}>{runID}</Link></h2>
              <ul className="decisions-list">
                {items.map((item) => item.type === "task" ? (
                  <li className="inbox-card" key={item.task.id} data-human-task-id={item.task.id} data-testid={`pending-task-${item.task.id}`}>
                    <code>{item.task.id}</code> · {item.task.kind}
                    <OutcomeButtons
                      taskId={item.task.id}
                      outcomes={item.task.request.allowed_outcomes ?? []}
                      disabled={!token || !actorID.trim() || versions[item.task.run_id] === undefined}
                      busy={submitting === item.task.id}
                      onChoose={(outcome) => void choose(item.task, outcome)}
                    />
                  </li>
                ) : (
                  <li className="inbox-card" key={item.record.id}>
                    <label><input type="checkbox" defaultChecked /> include this record in the verdict</label>{" "}
                    <code>{item.record.id}</code> · {item.record.record_type}
                    <pre>{JSON.stringify(item.record.data, null, 2)}</pre>
                  </li>
                ))}
              </ul>
            </section>
          ))}
          <div>
            <button type="button" disabled={page === 0} onClick={() => setPage((value) => value - 1)}>Previous page</button>{" "}
            <button type="button" disabled={page + 1 >= pageCount} onClick={() => setPage((value) => value + 1)}>Next page</button>
          </div>
        </>
      )}
    </section>
  );
}

function findTicketKey(input: unknown): string | null {
  if (!input || typeof input !== "object") return null;
  for (const [key, value] of Object.entries(input)) {
    if (["ticket_key", "issue_key", "jira_key"].includes(key) && typeof value === "string") return value;
    const nested = findTicketKey(value);
    if (nested) return nested;
  }
  return null;
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
  const [tokenHeld, setTokenHeld] = useState(getDecisionToken() !== null);
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

  useSharedEvents(DECISION_EVENT_TYPES, scheduleReload);

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
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
  }, [reloadKey]);

  const token = tokenHeld ? getDecisionToken() : null;

  return (
    <section className="view-rail decisions-view">
      <h1>Decisions</h1>
      <p className="muted">
        Every proposed record no review has decided. An agent saying it is done
        is a completion claim, not evidence — a decision here is a human&apos;s,
        recorded as its own ledger record naming who decided and why.
      </p>

      <TokenPanel
        held={tokenHeld}
        onHold={(value) => {
          setDecisionToken(value);
          setTokenHeld(true);
        }}
        onClear={() => {
          clearDecisionToken();
          setTokenHeld(false);
        }}
      />

      {error ? <ErrorNotice error={error} /> : null}

      {recorded.length > 0 ? (
        <ul className="decisions-recorded" id="decisions-recorded" role="status">
          {recorded.map((entry) => (
            <li key={entry.reviewId}>
              <strong>{entry.verdict}</strong>ed {entry.recordCount} record(s)
              on run <code>{entry.runId}</code> — review{" "}
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
              <RunDecisionCard
                key={group.run_id}
                group={group}
                token={token}
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

/**
 * Token entry, indicator, and clear affordance — the same contract the Inbox
 * states (risk r1, api/decision-token.ts): the input is `type="password"`,
 * its draft is dropped the moment the token is held, and the held value is
 * never rendered back into the page.
 */
function TokenPanel({
  held,
  onHold,
  onClear,
}: {
  held: boolean;
  onHold: (token: string) => void;
  onClear: () => void;
}) {
  const [draft, setDraft] = useState("");
  return (
    <div className="inbox-token" id="decisions-token">
      <div className="inbox-token__entry">
        <label htmlFor="decision-token-input">Decision token</label>
        <input
          id="decision-token-input"
          type="password"
          autoComplete="off"
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
        />
        <button
          type="button"
          className="author-workflow__button"
          disabled={draft === ""}
          onClick={() => {
            onHold(draft);
            setDraft("");
          }}
        >
          Hold token
        </button>
        {held ? (
          <button
            type="button"
            className="author-workflow__button"
            onClick={onClear}
          >
            Clear token
          </button>
        ) : null}
      </div>
      <p
        className="inbox-token__state muted"
        id="decision-token-state"
        data-token-held={held}
      >
        {held
          ? "token held — this tab only; cleared when the tab closes"
          : "no token held — decisions are disabled until one is entered"}
      </p>
    </div>
  );
}

/** One recorded decision, kept above the list after its run leaves it. */
interface RecordedDecision {
  reviewId: string;
  runId: string;
  verdict: "confirm" | "reject";
  recordCount: number;
  ledgerVersion: number;
}

/**
 * One run's undecided records and the form that decides them.
 *
 * The whole group is decided with one verdict and one rationale, because that
 * is the shape a review transaction has (PRD §10.8: all of it applies or none
 * of it does). Deciding a subset is deciding a different frame, so it is a
 * second review — the checkbox per record selects which records this review
 * covers.
 */
function RunDecisionCard({
  group,
  token,
  onDecided,
}: {
  group: PendingDecisionRun;
  token: string | null;
  onDecided: (entry: RecordedDecision) => void;
}) {
  const [selected, setSelected] = useState<string[]>(() =>
    group.records.map((record) => record.id),
  );
  const [verdict, setVerdict] = useState<"confirm" | "reject">("confirm");
  const [reviewer, setReviewer] = useState("");
  const [rationale, setRationale] = useState("");
  const [submitError, setSubmitError] = useState<ApiError | null>(null);
  const [result, setResult] = useState<ReviewCommitResult | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const canSubmit =
    token !== null &&
    !submitting &&
    result === null &&
    selected.length > 0 &&
    reviewer.trim() !== "" &&
    rationale.trim() !== "";

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!canSubmit || token === null) return;

    setSubmitError(null);
    setSubmitting(true);
    try {
      // The ledger version submitted is the one this page READ, never a
      // fresh fetch: that is what makes the stale guard meaningful. If the
      // run moved since the list loaded, the API refuses the commit with a
      // 409 and writes nothing, which is the correct outcome — the frame
      // this operator read is no longer the frame they would be deciding.
      const review = await createReview(
        group.run_id,
        {
          record_ids: selected,
          ledger_version: group.ledger_version,
          reviewer_actor_id: reviewer.trim(),
        },
        token,
      );
      const decisions: Record<string, "confirm" | "reject"> = {};
      for (const id of selected) decisions[id] = verdict;

      const committed = await commitReview(
        review.id,
        {
          decisions,
          expected_ledger_version: group.ledger_version,
          rationale: rationale.trim(),
        },
        token,
      );
      setResult(committed);
      onDecided({
        reviewId: committed.review_id,
        runId: group.run_id,
        verdict,
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
    <li className="inbox-card decisions-card" data-run-id={group.run_id}>
      <div className="inbox-card__head">
        <AuthorityChip authority="proposed" />
        <code className="inbox-card__id">
          <Link to={`/runs/${group.run_id}`}>{group.run_id}</Link>
        </code>
        <span className="inbox-card__kind">
          ledger version {group.ledger_version}
        </span>
      </div>

      <ul className="decisions-records">
        {group.records.map((record) => (
          <li key={record.id} data-record-id={record.id}>
            <label className="decisions-record__select">
              <input
                type="checkbox"
                checked={selected.includes(record.id)}
                onChange={(event) =>
                  setSelected((current) =>
                    event.target.checked
                      ? [...current, record.id]
                      : current.filter((id) => id !== record.id),
                  )
                }
                aria-label={`include this record in the verdict (${record.id})`}
              />
              include this record in the verdict · <code>{record.id}</code> · {record.record_type} · from{" "}
              {record.origin_actor_id ?? "an unnamed actor"} (
              {record.origin_kind})
            </label>
            {/* The payload in full: a decision on a claim nobody read is the
                failure this whole surface exists to prevent. */}
            <pre className="decisions-record__data">
              {JSON.stringify(record.data, null, 2)}
            </pre>
          </li>
        ))}
      </ul>

      {result === null ? (
        <form className="inbox-card__form" onSubmit={submit}>
          <fieldset className="inbox-card__outcomes">
            <legend>Verdict</legend>
            <label className="inbox-card__outcome">
              <input
                type="radio"
                name={`verdict-${group.run_id}`}
                value="confirm"
                checked={verdict === "confirm"}
                onChange={() => setVerdict("confirm")}
              />
              confirm
            </label>
            <label className="inbox-card__outcome">
              <input
                type="radio"
                name={`verdict-${group.run_id}`}
                value="reject"
                checked={verdict === "reject"}
                onChange={() => setVerdict("reject")}
              />
              reject
            </label>
          </fieldset>
          <div className="inbox-card__field">
            <label htmlFor={`reviewer-${group.run_id}`}>
              Reviewer actor id
            </label>
            <input
              id={`reviewer-${group.run_id}`}
              type="text"
              value={reviewer}
              onChange={(event) => setReviewer(event.target.value)}
            />
          </div>
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
    </li>
  );
}

export default Decisions;
