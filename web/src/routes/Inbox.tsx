import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { setAgentState } from "../agent-state/store";
import {
  ApiError,
  decideHumanTask,
  getLedger,
  listHumanTasks,
} from "../api/client";
import type { HumanTask, HumanTaskDecisionResult } from "../api/types";
import AuthorityChip from "../components/AuthorityChip";
import ErrorNotice from "../components/ErrorNotice";
import { HumanTaskAudit, HumanTaskFacts } from "../components/HumanTaskContext";
import { taskContextFacts } from "../domain/human-task-context";
import {
  DEFAULT_INBOX_TAB,
  INBOX_TABS,
  type InboxTab,
  parseInboxTab,
  partitionInbox,
} from "../domain/inbox-tabs";
import { formatRelativeTime } from "../domain/run-board";
import { SignedInAs } from "../components/IdentityGate";
import OutcomeButtons from "../components/OutcomeButtons";
import SegmentedToggle from "../components/SegmentedToggle";
import StatusChip from "../components/StatusChip";
import type { SharedEventType } from "../hooks/useSharedEvents";
import { useSnapshotReconcile } from "../hooks/useSnapshotReconcile";
import { useWhoami } from "../hooks/useWhoami";

/**
 * Every event that means a human task changed shape — a new one created, or
 * one just decided (possibly from another tab/operator) — a stable
 * module-level reference, as useSharedEvents requires (issue #46).
 */
const INBOX_EVENT_TYPES = [
  "dev.culture.nodes.human-task.created",
  "dev.culture.nodes.human-task.decided",
] as const satisfies readonly SharedEventType[];

const TAB_LABELS: Record<InboxTab, string> = {
  open: "Open",
  waiting: "Waiting",
  decided: "Decided",
};

const TAB_EMPTY: Record<InboxTab, string> = {
  open: "Nothing open.",
  waiting: "Nothing waiting past its deadline.",
  decided: "Nothing decided yet.",
};

/** Mirrors the Mesh view's attribution-refresh discipline (Mesh.tsx). */
const REFRESH_DEBOUNCE_MS = 4000;

/**
 * The Inbox view (task t14, issue #38b): every human task the control plane
 * is waiting on, actionable from the browser.
 *
 * Pending tasks come from `GET /v1alpha1/human-tasks?status=pending` and
 * render the §9.9 request payload exactly as the engine stored it — what
 * the human is actually being shown, never re-derived, and absent fields
 * stay absent (no fabricated deadlines). Each card carries one button per
 * outcome the engine will accept. Decided tasks
 * (`?status=decided`) render their resolution read-only under the
 * confirmed-authority chip, since a committed human decision IS a confirmed
 * ledger review (PRD §10.8).
 *
 * The decision itself is `OutcomeButtons` — the same component the Decisions
 * queue and the ticket page offer (task t12). It used to be a second, hand-
 * rolled radio fieldset plus a submit here, which is exactly the drift
 * `allowed_outcomes` exists to prevent: two independent renderings of "offer
 * what DecideHumanTask accepts and nothing else" are two chances for one of
 * them to offer an outcome that 400s (`expired`, #265) or hide one that would
 * have worked. There is one now, and it takes the free-text JSON payload and
 * note with it: the response is derived from the task's own decision schema,
 * as it already was on the other two surfaces.
 *
 * Who decides is not part of the form (task t9, spec c8). Until then the
 * page held a deployment-shared bearer per tab and asked for a decider id
 * in free text; both are gone. The decider is the actor
 * `useWhoami` says the signed-in principal is bound to, it is shown on the
 * page as a fact rather than a field, the request carries no credential
 * (the Cloudflare edge cookie is the credential), and an unbound or
 * signed-out state disables every submit.
 *
 * A card reads top to bottom as the decision does (issue #332): the
 * question, then what it is about — PR, ticket, findings with file links,
 * the agent's summary or reason — read from the API's server-side
 * resolution of the task's context refs (`resolved_context`), then the run
 * link and the decision. The audit ids and the raw JSON-pointer refs sit
 * behind a collapsed "audit" disclosure: kept, not in the way.
 *
 * `expected_ledger_version` is a real read, not a fabrication: each pending
 * card fetches its run's ledger once and submits the version it actually
 * read, so a concurrent write is refused by the stale guard instead of
 * silently raced.
 *
 * Three tabs filter the list (task t44), persisted as `?tab=`: **Open**
 * (default) is pending with no deadline or a future one — what a person can
 * act on now; **Waiting** is pending with a deadline already passed (stale,
 * still decidable, labelled "overdue since …"); **Decided** is every
 * non-pending task — decided and expired alike — newest first. Expired tasks
 * come from their own `?status=expired` read, since `?status=decided` never
 * returns them. The partition rule lives in `domain/inbox-tabs.ts`.
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
  const [error, setError] = useState<ApiError | null>(null);
  const whoami = useWhoami();
  // Bumped after a recorded decision, and (task t30, issue #46) by a
  // debounced human-task event on the shared cross-run stream. The effect
  // refetches WITHOUT nulling the lists first (stale-while-revalidate), so
  // the decided card's "decision recorded" confirmation survives the
  // refresh.
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
    INBOX_EVENT_TYPES,
    scheduleReload,
  );

  useEffect(() => {
    const controller = new AbortController();
    // "ready" means initial-load-settled and must never regress to
    // "loading" on a refresh (task t30's hard convention) — only the very
    // first render (reloadKey === 0) sets it; every later reload (decision
    // submit or SSE event) stays "ready" throughout, stale-while-revalidate.
    const isInitialLoad = reloadKey === 0;
    if (isInitialLoad) setAgentState({ status: "loading", run: null });
    setError(null);

    const toApiError = (cause: unknown): ApiError =>
      cause instanceof ApiError
        ? cause
        : new ApiError(0, String(cause), "check the browser console");

    Promise.all([
      listHumanTasks(controller.signal, { status: "pending" }),
      listHumanTasks(controller.signal, { status: "decided" }),
      listHumanTasks(controller.signal, { status: "expired" }),
    ])
      .then(([pendingList, decidedList, expiredList]) => {
        if (controller.signal.aborted) return;
        setPending(pendingList.items);
        // Decided and expired are both terminal; the Decided tab shows both.
        setDecided([...decidedList.items, ...expiredList.items]);
        if (isInitialLoad) resolveSnapshot();
        setAgentState({ status: "ready", run: null });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setPending((prev) => prev ?? []);
        setDecided((prev) => prev ?? []);
        setError(toApiError(cause));
        if (isInitialLoad) resolveSnapshot();
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
  }, [reloadKey, resolveSnapshot]);

  const actorId = whoami.status === "bound" ? whoami.actorId : null;
  const loaded = pending !== null && decided !== null;
  const clock = now ?? new Date();
  const tabs = partitionInbox([...(pending ?? []), ...(decided ?? [])], clock);
  const shown = tabs[tab];

  return (
    <section className="view-rail inbox-view">
      <h1>Inbox</h1>
      <p className="muted">
        Human tasks the control plane is waiting on. A decision here is a
        confirmed, human-authority ledger review — it routes the paused run.
      </p>

      <SignedInAs verb="Deciding" whoami={whoami} />

      {error ? <ErrorNotice error={error} /> : null}
      {!loaded ? (
        <p className="muted" id="inbox-loading">
          Loading inbox…
        </p>
      ) : pending.length === 0 && decided.length === 0 ? (
        <p className="muted" id="inbox-empty">
          No human tasks yet. A run pauses here when it reaches an approval
          node.
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
                <span className="inbox-tab__count" data-count={tabs[name].length}>
                  {tabs[name].length}
                </span>
              </button>
            ))}
          </SegmentedToggle>

          {shown.length === 0 ? (
            <p className="muted" id="inbox-tab-empty">
              {TAB_EMPTY[tab]}
            </p>
          ) : tab === "decided" ? (
            <ul className="inbox-list" id="inbox-decided">
              {shown.map((task) => (
                <DecidedTaskCard key={task.id} task={task} />
              ))}
            </ul>
          ) : (
            <ul className="inbox-list" id={`inbox-${tab}`}>
              {shown.map((task) => (
                <PendingTaskCard
                  key={task.id}
                  task={task}
                  actorId={actorId}
                  overdueSince={
                    tab === "waiting" && task.request?.deadline
                      ? formatRelativeTime(task.request.deadline, clock)
                      : undefined
                  }
                  onDecided={() => setReloadKey((key) => key + 1)}
                />
              ))}
            </ul>
          )}
        </>
      )}
    </section>
  );
}

function PendingTaskCard({
  task,
  actorId,
  overdueSince,
  onDecided,
}: {
  task: HumanTask;
  /** The signed-in principal's actor, or null when nothing can be recorded. */
  actorId: string | null;
  /** Relative time since a passed deadline (Waiting tab); absent otherwise. */
  overdueSince?: string;
  onDecided: () => void;
}) {
  const [ledgerVersion, setLedgerVersion] = useState<number | null>(null);
  const [submitError, setSubmitError] = useState<ApiError | null>(null);
  const [result, setResult] = useState<HumanTaskDecisionResult | null>(null);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    getLedger(task.run_id, controller.signal)
      .then((ledger) => {
        if (!controller.signal.aborted)
          setLedgerVersion(ledger.ledger_version);
      })
      .catch(() => {
        /* the guard row keeps saying "reading…"; submit stays disabled */
      });
    return () => controller.abort();
  }, [task.run_id]);

  const request = task.request ?? {};

  /**
   * Record the decision (task t12). `expected_ledger_version` is a real read,
   * not a fabrication: the card fetched its run's ledger once and submits the
   * version it actually read, so a concurrent write is refused by the stale
   * guard instead of silently raced.
   */
  const decide = async (outcome: string) => {
    if (actorId === null || ledgerVersion === null || submitting) return;
    setSubmitError(null);
    setSubmitting(true);
    try {
      const decided = await decideHumanTask(task.id, {
        outcome,
        decider_actor_id: actorId,
        // A task with a decision schema gets a schema-valid payload; one
        // without gets none, rather than an invented empty object.
        response: request.decision_schema_ref ? { outcome } : undefined,
        expected_ledger_version: ledgerVersion,
      });
      setResult(decided);
      onDecided();
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

  const facts = taskContextFacts(task);

  return (
    <li className="inbox-card" data-human-task-id={task.id}>
      <div className="inbox-card__head">
        <StatusChip state="waiting" />
        <code className="inbox-card__id">{task.id}</code>
        <span className="inbox-card__kind">{task.kind}</span>
      </div>

      <HumanTaskFacts facts={facts} />

      <dl className="inbox-card__request">
        <div>
          <dt>run</dt>
          <dd>
            <Link to={`/runs/${task.run_id}`}>{task.run_id}</Link>
          </dd>
        </div>
        <div>
          <dt>created</dt>
          <dd>
            <time dateTime={task.created_at}>{task.created_at}</time>
          </dd>
        </div>
        {request.deadline ? (
          <div>
            <dt>deadline</dt>
            <dd>
              <time dateTime={request.deadline}>{request.deadline}</time>
              {overdueSince ? (
                <span className="inbox-card__overdue">
                  {" "}
                  overdue since {overdueSince}
                </span>
              ) : null}
            </dd>
          </div>
        ) : null}
        {request.approver_ref ? (
          <div>
            <dt>approver</dt>
            <dd>{request.approver_ref}</dd>
          </div>
        ) : null}
        {request.decision_schema_ref ? (
          <div>
            <dt>decision schema</dt>
            <dd>
              <code>{request.decision_schema_ref}</code>
            </dd>
          </div>
        ) : null}
      </dl>

      {result === null ? (
        <>
          <OutcomeButtons
            taskId={task.id}
            outcomes={request.allowed_outcomes ?? []}
            disabled={actorId === null || ledgerVersion === null}
            busy={submitting}
            onChoose={(outcome) => void decide(outcome)}
          />
          {submitError ? <ErrorNotice error={submitError} /> : null}
        </>
      ) : (
        <p className="inbox-card__result" role="status">
          decision recorded — outcome <strong>{result.outcome}</strong>, run
          now <strong>{result.run_state}</strong>
          {result.next_node_id ? <>, next node {result.next_node_id}</> : null}
        </p>
      )}

      <HumanTaskAudit
        task={task}
        ledgerGuard={
          ledgerVersion === null ? (
            <span className="muted">reading the run's ledger…</span>
          ) : (
            <code>{ledgerVersion}</code>
          )
        }
      />
    </li>
  );
}

/** A decided task, read-only: the resolution as a confirmed human review. */
function DecidedTaskCard({ task }: { task: HumanTask }) {
  return (
    <li
      className="inbox-card inbox-card--decided"
      data-human-task-id={task.id}
    >
      <div className="inbox-card__head">
        <AuthorityChip authority="confirmed" />
        <code className="inbox-card__id">{task.id}</code>
        <span className="inbox-card__kind">{task.kind}</span>
      </div>
      <HumanTaskFacts facts={taskContextFacts(task)} />
      <dl className="inbox-card__request">
        <div>
          <dt>run</dt>
          <dd>
            <Link to={`/runs/${task.run_id}`}>{task.run_id}</Link>
          </dd>
        </div>
        {task.resolved_at ? (
          <div>
            <dt>resolved</dt>
            <dd>
              <time dateTime={task.resolved_at}>{task.resolved_at}</time>
            </dd>
          </div>
        ) : null}
      </dl>
      {task.response !== undefined && task.response !== null ? (
        <pre className="inbox-card__response">
          {JSON.stringify(task.response, null, 2)}
        </pre>
      ) : (
        <p className="muted">No decision payload was recorded.</p>
      )}
      <HumanTaskAudit task={task} />
    </li>
  );
}

export default Inbox;
