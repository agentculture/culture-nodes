import { useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { ApiError, commitReview, createReview } from "../api/client";
import type { ReviewedRecord } from "../api/decisionTypes";
import type { PendingDecisionRun, ReviewCommitResult } from "../api/types";
import AuthorityChip from "./AuthorityChip";
import ErrorNotice from "./ErrorNotice";
import RunDecisionCard, {
  RecordPayload,
  confirmAllVerdicts,
  recordsWithVerdict,
  type RecordVerdict,
  type RunVerdicts,
} from "./RunDecisionCard";

/**
 * The review half of the one decision page (task t46, owner decision d19):
 * what the Decisions page (`/decisions`, task t30 / issue #99) did, moved
 * here intact so the Inbox's To review tab renders it rather than a copy.
 *
 * Three properties it refuses to fudge, all the Decisions page's:
 *
 *   - The rationale is required by the form, as it is by the API. A
 *     confirmation with no stated reason cannot be told apart from an unread
 *     one. (A human TASK's note is optional; a review's rationale is not.)
 *   - The reviewer is the signed-in principal's actor, never typed (task t9).
 *   - The record payload is rendered in full (the shared RunDecisionCard).
 *
 * Deciding does not change the records decided: the ledger appends a review
 * record naming each one, and the claim keeps reading `proposed` forever.
 */

/** One recorded review, kept above the list after its run leaves it. */
export interface RecordedDecision {
  reviewId: string;
  runId: string;
  verdict: "confirm" | "reject" | "mixed";
  recordCount: number;
  ledgerVersion: number;
}

/** The confirmations of this sitting, which outlive the cards that made them. */
export function RecordedDecisions({ recorded }: { recorded: RecordedDecision[] }) {
  if (recorded.length === 0) return null;
  return (
    <ul className="decisions-recorded" id="decisions-recorded" role="status">
      {recorded.map((entry) => (
        <li key={entry.reviewId}>
          Recorded a <strong>{entry.verdict}</strong> decision on {entry.recordCount}{" "}
          record(s) of run <code>{entry.runId}</code> — review <code>{entry.reviewId}</code>,
          ledger now at version {entry.ledgerVersion}. The records decided are unchanged:
          a review names them, it never rewrites them.
        </li>
      ))}
    </ul>
  );
}

/**
 * One run's undecided records and the form that decides them. The verdict is
 * per record, the grain `POST /v1alpha1/reviews/{id}/commit` decides at; a
 * record left at "not now" is not named by this review and stays pending.
 */
export function ProposedRunDecision({
  group,
  actorId,
  onDecided,
}: {
  group: PendingDecisionRun;
  /** The signed-in principal's actor, or null when nothing can be recorded. */
  actorId: string | null;
  onDecided: (entry: RecordedDecision) => void;
}) {
  const [verdicts, setVerdicts] = useState<RunVerdicts>(() => confirmAllVerdicts(group));
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
      // fresh fetch: that is what makes the stale guard meaningful.
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
        verdict:
          rejected.length === 0 ? "confirm" : confirmed.length === 0 ? "reject" : "mixed",
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
          decision recorded — {result.records.length} review record(s) appended by review{" "}
          <code>{result.review_id}</code>; the run&apos;s ledger is now at version{" "}
          {result.ledger_version}. The records decided are unchanged: a review names them,
          it never rewrites them.
        </p>
      )}
    </RunDecisionCard>
  );
}

/**
 * A committed review of one record, read-only (the Decided tab): the record
 * as it was read — rendered by the same RecordPayload the review form uses —
 * the verdict, who decided and why.
 */
export function ReviewedRecordCard({ item }: { item: ReviewedRecord }) {
  const record = item.record;
  return (
    <li
      className="inbox-card inbox-card--decided decisions-card"
      data-reviewed-record-id={item.review_record_id}
    >
      <div className="inbox-card__head">
        <AuthorityChip authority={item.verdict === "reject" ? "rejected" : "confirmed"} />
        <code className="inbox-card__id">
          <Link to={`/runs/${item.run_id}`}>{item.run_id}</Link>
        </code>
        <span className="inbox-card__kind">review · {record.record_type}</span>
      </div>
      <div className="decisions-record__head">
        <code>{record.id}</code> · {record.record_type} · from{" "}
        {record.origin_actor_id ?? "an unnamed actor"} ({record.origin_kind})
      </div>
      <RecordPayload data={record.data} />
      <dl className="inbox-card__request">
        <div>
          <dt>verdict</dt>
          <dd>
            <strong>{item.verdict}</strong>
            {item.reviewer_actor_id ? <> by {item.reviewer_actor_id}</> : null}
          </dd>
        </div>
        <div>
          <dt>reviewed</dt>
          <dd>
            <time dateTime={item.reviewed_at}>{item.reviewed_at}</time>
          </dd>
        </div>
        {item.rationale ? (
          <div>
            <dt>why</dt>
            <dd>
              <p className="inbox-card__note">{item.rationale}</p>
            </dd>
          </div>
        ) : null}
      </dl>
    </li>
  );
}
