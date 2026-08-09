import { Link } from "react-router-dom";
import type { Run } from "../api/types";
import { formatRelativeTime, RUN_STATE_ACCENT_VAR } from "../domain/run-board";
import RunStateChip from "./RunStateChip";

export interface RunCardProps {
  run: Run;
  reducedMotion?: boolean;
  /** Test seam for `formatRelativeTime`; defaults to the real clock. */
  now?: Date;
}

/** Same 20-char digest-slicing convention as LedgerTable.tsx/RunsList.tsx. */
function shortDigest(digest: string): string {
  return digest.length > 20 ? `${digest.slice(0, 20)}…` : digest;
}

/** Run ids run longer than a node id, so the card shortens the same way. */
function shortRunId(id: string): string {
  return id.length > 20 ? `${id.slice(0, 20)}…` : id;
}

/**
 * A run card for the runs board (PRD §8.6 Operations): workflow reference,
 * run id, status, and last-updated — the whole run list's `Run` record, no
 * more, because the board renders committed API state only (honesty
 * condition h5). The card *is* the link: an `<a>` inherits Enter-to-activate
 * and the tab order for free, so no bespoke key handling is needed the way
 * NodeCard's canvas button needs one.
 *
 * The `is-pulse` / reduced-motion badge pair for a running card is the same
 * pattern NodeCard uses for an in-flight attempt (styles/app.css reuses its
 * `node-attempt-pulse` keyframe verbatim) — the same fact, "this is moving
 * right now", carried as motion when allowed and as text when it isn't.
 */
export function RunCard({ run, reducedMotion = false, now }: RunCardProps) {
  const relative = formatRelativeTime(run.updated_at, now);
  const pulsing = run.state === "running" && !reducedMotion;
  const classes = [
    "run-card",
    `run-card--${run.state}`,
    pulsing ? "is-pulse" : "",
  ]
    .filter(Boolean)
    .join(" ");
  const label = `run ${run.id}, ${run.state}, updated ${relative}`;

  return (
    <Link
      className={classes}
      to={`/runs/${encodeURIComponent(run.id)}`}
      data-run-id={run.id}
      data-run-state={run.state}
      aria-label={label}
      style={{ ["--node-accent" as string]: RUN_STATE_ACCENT_VAR[run.state] }}
    >
      <span className="run-card__rail" aria-hidden="true" />
      <div className="run-card__head">
        <span className="run-card__dot" aria-hidden="true" />
        <span className="run-card__id">{shortRunId(run.id)}</span>
      </div>
      <div className="run-card__meta">
        <code className="run-card__workflow" title={run.workflow_digest}>
          {shortDigest(run.workflow_digest)}
        </code>
      </div>
      <div className="run-card__status">
        <RunStateChip state={run.state} />
        <time className="run-card__updated" dateTime={run.updated_at}>
          {relative}
        </time>
        {run.state === "running" && reducedMotion ? (
          <span className="run-card__badge run-card__badge--live">
            updating live
          </span>
        ) : null}
      </div>
    </Link>
  );
}

export default RunCard;
