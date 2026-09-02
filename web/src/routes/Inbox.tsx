import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { setAgentState } from "../agent-state/store";
import {
  ApiError,
  decideHumanTask,
  getLedger,
  listHumanTasks,
} from "../api/client";
import type {
  HumanTask,
  HumanTaskBinding,
  HumanTaskDecisionResult,
} from "../api/types";
import AuthorityChip from "../components/AuthorityChip";
import ErrorNotice from "../components/ErrorNotice";
import { SignedInAs } from "../components/IdentityGate";
import OutcomeButtons from "../components/OutcomeButtons";
import StatusChip from "../components/StatusChip";
import { useSharedEvents, type SharedEventType } from "../hooks/useSharedEvents";
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
 * `expected_ledger_version` is a real read, not a fabrication: each pending
 * card fetches its run's ledger once and submits the version it actually
 * read, so a concurrent write is refused by the stale guard instead of
 * silently raced.
 */
export function Inbox() {
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

  useSharedEvents(INBOX_EVENT_TYPES, scheduleReload);

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
    ])
      .then(([pendingList, decidedList]) => {
        if (controller.signal.aborted) return;
        setPending(pendingList.items);
        setDecided(decidedList.items);
        setAgentState({ status: "ready", run: null });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setPending((prev) => prev ?? []);
        setDecided((prev) => prev ?? []);
        setError(toApiError(cause));
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
  }, [reloadKey]);

  const actorId = whoami.status === "bound" ? whoami.actorId : null;
  const loaded = pending !== null && decided !== null;

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
          <h2>Pending</h2>
          {pending.length === 0 ? (
            <p className="muted">Nothing is waiting on a human right now.</p>
          ) : (
            <ul className="inbox-list" id="inbox-pending">
              {pending.map((task) => (
                <PendingTaskCard
                  key={task.id}
                  task={task}
                  actorId={actorId}
                  onDecided={() => setReloadKey((key) => key + 1)}
                />
              ))}
            </ul>
          )}

          {decided.length > 0 ? (
            <>
              <h2>Decided</h2>
              <ul className="inbox-list" id="inbox-decided">
                {decided.map((task) => (
                  <DecidedTaskCard key={task.id} task={task} />
                ))}
              </ul>
            </>
          ) : null}
        </>
      )}
    </section>
  );
}

/** Shorten a sha256 digest the way the Workflows table does. */
function shortDigest(digest: string): string {
  return digest.length > 21 ? `${digest.slice(0, 20)}…` : digest;
}

/**
 * Render one context ref the way the workflow declares it: a pointer as the
 * pointer, a literal (issue #73) as the declared value. A literal is shown
 * rather than summarised because it is the whole reason the shape exists — the
 * reader should be able to name what the task observes.
 */
function renderBinding(ref: HumanTaskBinding): string {
  return typeof ref === "string" ? ref : JSON.stringify(ref.literal);
}

function PendingTaskCard({
  task,
  actorId,
  onDecided,
}: {
  task: HumanTask;
  /** The signed-in principal's actor, or null when nothing can be recorded. */
  actorId: string | null;
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
  const audit = request.audit;

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

  return (
    <li className="inbox-card" data-human-task-id={task.id}>
      <div className="inbox-card__head">
        <StatusChip state="waiting" />
        <code className="inbox-card__id">{task.id}</code>
        <span className="inbox-card__kind">{task.kind}</span>
      </div>

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
        {request.context_refs ? (
          <div>
            <dt>context refs</dt>
            <dd>
              {request.context_refs.from ? (
                <code>{request.context_refs.from}</code>
              ) : null}
              {request.context_refs.bindings ? (
                <ul className="inbox-card__bindings">
                  {Object.entries(request.context_refs.bindings).map(
                    ([name, ref]) => (
                      <li key={name}>
                        {name}: <code>{renderBinding(ref)}</code>
                      </li>
                    ),
                  )}
                </ul>
              ) : null}
            </dd>
          </div>
        ) : null}
        {audit ? (
          <div>
            <dt>audit</dt>
            <dd>
              <dl className="inbox-card__audit">
                {audit.node_id ? (
                  <div>
                    <dt>node</dt>
                    <dd>
                      <code>{audit.node_id}</code>
                    </dd>
                  </div>
                ) : null}
                {audit.token_id ? (
                  <div>
                    <dt>token</dt>
                    <dd>
                      <code>{audit.token_id}</code>
                    </dd>
                  </div>
                ) : null}
                {audit.workflow_digest ? (
                  <div>
                    <dt>workflow</dt>
                    <dd>
                      <code title={audit.workflow_digest}>
                        {shortDigest(audit.workflow_digest)}
                      </code>
                    </dd>
                  </div>
                ) : null}
                {audit.from_node ? (
                  <div>
                    <dt>arrived via</dt>
                    <dd>
                      {audit.from_node} → {audit.from_outcome}
                    </dd>
                  </div>
                ) : null}
              </dl>
            </dd>
          </div>
        ) : null}
        <div>
          <dt>ledger guard</dt>
          <dd>
            {ledgerVersion === null ? (
              <span className="muted">reading the run's ledger…</span>
            ) : (
              <code>{ledgerVersion}</code>
            )}
          </dd>
        </div>
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
    </li>
  );
}

export default Inbox;
