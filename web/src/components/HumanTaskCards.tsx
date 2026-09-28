import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ApiError, decideHumanTask, getLedger } from "../api/client";
import type { HumanTask, HumanTaskDecisionResult } from "../api/types";
import { taskContextFacts } from "../domain/human-task-context";
import { isNoticeTask, taskAllowedOutcomes } from "../domain/human-task-notice";
import { isUndecidable } from "../domain/inbox-tabs";
import AuthorityChip from "./AuthorityChip";
import ErrorNotice from "./ErrorNotice";
import { HumanTaskAudit, HumanTaskFacts } from "./HumanTaskContext";
import NoticeFacts from "./NoticeFacts";
import OutcomeButtons from "./OutcomeButtons";
import StatusChip from "./StatusChip";

/**
 * The two human-task cards the Inbox renders (moved out of routes/Inbox.tsx
 * in task t46, when the page became the one place to decide and needed room
 * for the review tab).
 *
 * A card reads top to bottom as the decision does (issue #332): the
 * question, then what it is about — read from the API's server-side
 * resolution of the task's context refs — then the run link and the
 * decision. The audit ids and raw refs sit behind a collapsed disclosure.
 *
 * The decision is `OutcomeButtons`, shared with the ticket page (task t12):
 * one rendering of "offer what DecideHumanTask accepts and nothing else".
 * Beside it, since task t46 (owner decision d19), an OPTIONAL note — the
 * input the ledger review surface always offered as its rationale. Optional
 * here, because an approval's outcome is itself the answer; a note is sent
 * only when one was written, and the API records it on the ledger decision
 * record, as the confirming review's rationale, and on the decided task.
 */

/** The API's bound on a decision note (engine.MaxDecisionNoteRunes). */
export const DECISION_NOTE_MAX = 2000;

const asApiError = (cause: unknown): ApiError =>
  cause instanceof ApiError
    ? cause
    : new ApiError(0, String(cause), "check the browser console");

export function PendingTaskCard({
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
  const [note, setNote] = useState("");

  useEffect(() => {
    const controller = new AbortController();
    getLedger(task.run_id, controller.signal)
      .then((ledger) => {
        if (!controller.signal.aborted) setLedgerVersion(ledger.ledger_version);
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
    const trimmed = note.trim();
    try {
      const decided = await decideHumanTask(task.id, {
        outcome,
        decider_actor_id: actorId,
        // A task with a decision schema gets a schema-valid payload; one
        // without gets none, rather than an invented empty object.
        response: request.decision_schema_ref ? { outcome } : undefined,
        expected_ledger_version: ledgerVersion,
        ...(trimmed ? { note: trimmed } : {}),
      });
      setResult(decided);
      onDecided();
    } catch (cause) {
      setSubmitError(asApiError(cause));
    } finally {
      setSubmitting(false);
    }
  };

  const notice = isNoticeTask(task);
  const facts = notice ? null : taskContextFacts(task);
  const undecidable = isUndecidable(task);
  const noteId = `note-${task.id}`;

  return (
    <li className="inbox-card" data-human-task-id={task.id}>
      <div className="inbox-card__head">
        <StatusChip state="waiting" />
        <code className="inbox-card__id">{task.id}</code>
        <span className="inbox-card__kind">{task.kind}</span>
      </div>

      {facts ? <HumanTaskFacts facts={facts} /> : <NoticeFacts task={task} />}

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
                <span className="inbox-card__overdue"> overdue since {overdueSince}</span>
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
          {undecidable ? (
            <p className="muted inbox-card__undecidable">cannot be decided here</p>
          ) : (
            <div className="inbox-card__field">
              <label htmlFor={noteId}>Note (optional, recorded with the decision)</label>
              <textarea
                id={noteId}
                rows={2}
                maxLength={DECISION_NOTE_MAX}
                value={note}
                disabled={actorId === null || submitting}
                onChange={(event) => setNote(event.target.value)}
              />
            </div>
          )}
          <OutcomeButtons
            taskId={task.id}
            outcomes={taskAllowedOutcomes(task)}
            disabled={actorId === null || ledgerVersion === null}
            busy={submitting}
            onChoose={(outcome) => void decide(outcome)}
          />
          {submitError ? <ErrorNotice error={submitError} /> : null}
        </>
      ) : (
        <p className="inbox-card__result" role="status">
          decision recorded — outcome <strong>{result.outcome}</strong>, run now{" "}
          <strong>{result.run_state}</strong>
          {result.next_node_id ? <>, next node {result.next_node_id}</> : null}
          {result.note ? <>, with your note</> : null}
        </p>
      )}

      <HumanTaskAudit
        task={task}
        facts={facts}
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

/** What the stored response says, readably: outcome and decider. */
function decisionSummary(response: unknown): { outcome?: string; decider?: string } {
  if (!response || typeof response !== "object" || Array.isArray(response)) return {};
  const value = response as Record<string, unknown>;
  const text = (v: unknown) => (typeof v === "string" && v.trim() ? v : undefined);
  return { outcome: text(value.outcome), decider: text(value.decider_actor_id) };
}

/** A decided task, read-only: the resolution as a confirmed human review. */
export function DecidedTaskCard({ task }: { task: HumanTask }) {
  const facts = isNoticeTask(task) ? null : taskContextFacts(task);
  const summary = decisionSummary(task.response);
  return (
    <li className="inbox-card inbox-card--decided" data-human-task-id={task.id}>
      <div className="inbox-card__head">
        <AuthorityChip authority="confirmed" />
        <code className="inbox-card__id">{task.id}</code>
        <span className="inbox-card__kind">{task.kind}</span>
      </div>
      {facts ? <HumanTaskFacts facts={facts} /> : <NoticeFacts task={task} />}
      <dl className="inbox-card__request">
        <div>
          <dt>run</dt>
          <dd>
            <Link to={`/runs/${task.run_id}`}>{task.run_id}</Link>
          </dd>
        </div>
        {summary.outcome ? (
          <div>
            <dt>outcome</dt>
            <dd>
              <strong>{summary.outcome}</strong>
              {summary.decider ? <> by {summary.decider}</> : null}
            </dd>
          </div>
        ) : null}
        {task.resolved_at ? (
          <div>
            <dt>resolved</dt>
            <dd>
              <time dateTime={task.resolved_at}>{task.resolved_at}</time>
            </dd>
          </div>
        ) : null}
        {task.note ? (
          <div>
            <dt>note</dt>
            <dd>
              <p className="inbox-card__note">{task.note}</p>
            </dd>
          </div>
        ) : null}
      </dl>
      {task.response !== undefined && task.response !== null ? (
        <pre className="inbox-card__response">{JSON.stringify(task.response, null, 2)}</pre>
      ) : (
        <p className="muted">No decision payload was recorded.</p>
      )}
      <HumanTaskAudit task={task} facts={facts} />
    </li>
  );
}
