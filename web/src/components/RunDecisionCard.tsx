import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import type { PendingDecisionRun } from "../api/types";
import AuthorityChip from "./AuthorityChip";

/**
 * One run's undecided ledger records, and what a human is about to say about
 * each of them.
 *
 * This was `RunDecisionCard` inside Decisions.tsx until the ticket page had to
 * decide the same records (task t12, spec c11). It is shared rather than
 * copied for the reason `OutcomeButtons` is: two renderings of the record a
 * decider reads before confirming it are two chances for one of them to
 * summarise away the qualifying half — and a confirmation on a claim nobody
 * read is the exact failure PRD §10.4 exists to prevent.
 *
 * What the card renders and what it does NOT are both deliberate:
 *
 *   - The payload is rendered in full, with the `statement` lifted out as
 *     prose (task t27) so its newlines survive, and every other field left as
 *     the exact, untruncated JSON.
 *   - A verdict is offered PER RECORD, because that is the grain the ledger
 *     decides at (`POST /v1alpha1/reviews/{id}/commit` takes a verdict per
 *     record id). `skip` is a first-class third answer: a review names the
 *     records it covers, and leaving one out is a real choice, not an
 *     omission.
 *   - The card holds no state, opens no review and posts nothing. Deciding a
 *     run on its own (the `/decisions` queue) and deciding a whole ticket's
 *     runs in one action (the ticket page, decision c40) are different
 *     transactions over the same rendering, so the caller owns the form, the
 *     submit and the result.
 */

/** What a reader is saying about one record in the review being composed. */
export type RecordVerdict = "confirm" | "reject" | "skip";

/** Record id → the verdict it will carry into the review. */
export type RunVerdicts = Record<string, RecordVerdict>;

/**
 * The starting position: every record confirmed. It matches what the
 * `/decisions` card has always defaulted to (all records selected, verdict
 * `confirm`), and the rationale is still required either way — a decider
 * cannot get to a submit without stating what they read.
 */
export function confirmAllVerdicts(group: PendingDecisionRun): RunVerdicts {
  return Object.fromEntries(
    group.records.map((record) => [record.id, "confirm" as const]),
  );
}

/** The ids in `group` currently carrying `verdict`, in the served order. */
export function recordsWithVerdict(
  group: PendingDecisionRun,
  verdicts: RunVerdicts,
  verdict: Exclude<RecordVerdict, "skip">,
): string[] {
  return group.records
    .filter((record) => verdicts[record.id] === verdict)
    .map((record) => record.id);
}

/** The three answers, in the order they are offered. */
const VERDICT_CHOICES: ReadonlyArray<{ value: RecordVerdict; label: string }> = [
  { value: "confirm", label: "confirm" },
  { value: "reject", label: "reject" },
  { value: "skip", label: "not now" },
];

/**
 * A ledger record's payload, with its prose readable as prose (task t27).
 *
 * A `claim` record's `statement` is a paragraph a human wrote or an agent
 * composed — often several, with newlines. `JSON.stringify` renders those as
 * literal `\n` inside a quoted string, so the one field a decider must
 * actually READ was the one field they could not. The statement is lifted out
 * and rendered as text with its newlines intact; every other field still
 * renders as the exact JSON payload, unmodified and untruncated, below it.
 */
export function RecordPayload({ data }: { data: unknown }) {
  const statement = statementOf(data);
  const rest = statement === null ? data : withoutStatement(data);
  const restIsEmpty =
    rest !== null &&
    typeof rest === "object" &&
    !Array.isArray(rest) &&
    Object.keys(rest as Record<string, unknown>).length === 0;

  return (
    <>
      {statement !== null ? (
        <p className="decisions-record__statement">{statement}</p>
      ) : null}
      {restIsEmpty ? null : (
        <pre className="decisions-record__data">
          {JSON.stringify(rest, null, 2)}
        </pre>
      )}
    </>
  );
}

/** The record's `statement`, when it has one that is prose. */
function statementOf(data: unknown): string | null {
  if (!data || typeof data !== "object" || Array.isArray(data)) return null;
  const value = (data as Record<string, unknown>).statement;
  return typeof value === "string" && value.trim() !== "" ? value : null;
}

function withoutStatement(data: unknown): unknown {
  const { statement: _statement, ...rest } = data as Record<string, unknown>;
  return rest;
}

export interface RunDecisionCardProps {
  group: PendingDecisionRun;
  /** The verdict currently chosen for each record in `group`. */
  verdicts: RunVerdicts;
  onVerdictChange: (recordId: string, verdict: RecordVerdict) => void;
  /** True when no decision can be recorded at all (whoami not bound). */
  disabled?: boolean;
  /**
   * The records whose review has already committed. Their verdict controls
   * go, and each renders its unchanged `proposed` authority beside the
   * verdict the review recorded — because that is what actually happened: a
   * review names records, it never rewrites them (PRD §10.8).
   *
   * It is a set of ids rather than a flag on the card because a run's records
   * do not have to land together: a ticket batch that confirms two records
   * and rejects a third submits two reviews for that run, and the second can
   * conflict while the first commits.
   */
  reviewedRecordIds?: readonly string[];
  /** The caller's form, or its result: whatever decides this run. */
  children?: ReactNode;
}

export function RunDecisionCard({
  group,
  verdicts,
  onVerdictChange,
  disabled = false,
  reviewedRecordIds,
  children,
}: RunDecisionCardProps) {
  const reviewed = new Set(reviewedRecordIds ?? []);
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
            <div className="decisions-record__head">
              <code>{record.id}</code> · {record.record_type} · from{" "}
              {record.origin_actor_id ?? "an unnamed actor"} (
              {record.origin_kind})
            </div>
            {/* The payload in full: a decision on a claim nobody read is the
                failure this whole surface exists to prevent. The statement
                renders as prose so it can actually be read (task t27); the
                rest is still the exact JSON, untruncated. */}
            <RecordPayload data={record.data} />
            {reviewed.has(record.id) ? (
              <ReviewedRecord verdict={verdicts[record.id] ?? "skip"} />
            ) : (
              <fieldset
                className="decisions-record__verdict"
                aria-label={`Verdict for ${record.id}`}
              >
                <legend className="decisions-record__verdict-legend">
                  This record
                </legend>
                {VERDICT_CHOICES.map((choice) => (
                  <label key={choice.value} className="inbox-card__outcome">
                    <input
                      type="radio"
                      name={`verdict-${record.id}`}
                      value={choice.value}
                      checked={verdicts[record.id] === choice.value}
                      disabled={disabled}
                      onChange={() => onVerdictChange(record.id, choice.value)}
                    />
                    {choice.label}
                  </label>
                ))}
              </fieldset>
            )}
          </li>
        ))}
      </ul>

      {children}
    </li>
  );
}

/**
 * What one record looks like after its run's review committed: the record's
 * own authority, which did not move, and the review that names it, which is a
 * separate fact.
 */
function ReviewedRecord({ verdict }: { verdict: RecordVerdict }) {
  if (verdict === "skip") {
    return (
      <p className="decisions-record__decided muted">
        left out of this review — still awaiting a decision
      </p>
    );
  }
  return (
    <p className="decisions-record__decided">
      <AuthorityChip authority="proposed" />
      <span> — the record, unchanged · review </span>
      <AuthorityChip authority={verdict === "confirm" ? "confirmed" : "rejected"} />
    </p>
  );
}

export default RunDecisionCard;
