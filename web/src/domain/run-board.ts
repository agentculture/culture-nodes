import type { Run, RunState } from "../api/types";

/**
 * The runs board's column vocabulary — verbatim the `RunState` enum from
 * api/openapi/openapi.yaml (`created, running, waiting, completed, failed,
 * cancelled`), in the order the spec declares it. The board must never
 * invent a state the API does not report (e.g. a "queued" column), and must
 * never drop one either — every run the API returns lands in exactly one of
 * these, because every value of `Run.state` appears here once.
 */
export const RUN_STATE_COLUMNS: RunState[] = [
  "created",
  "running",
  "waiting",
  "completed",
  "failed",
  "cancelled",
];

/**
 * Column/card accent, one culture-design token per state — reusing the same
 * `--nodes-*` roles styles/app.css already declares for node execution state
 * (themselves copied from culture-design/palette.ts's TERMINAL_PALETTE) and
 * the shared `--accent-strong` token from tokens.css. No new colour is
 * introduced here; this is indirection onto existing tokens only.
 */
export const RUN_STATE_ACCENT_VAR: Record<RunState, string> = {
  created: "var(--nodes-idle)",
  running: "var(--accent-strong)",
  waiting: "var(--nodes-warn)",
  completed: "var(--nodes-ok)",
  failed: "var(--nodes-danger)",
  cancelled: "var(--nodes-danger)",
};

/**
 * Bucket runs by their own committed `state` — one bucket per RUN_STATE_COLUMNS
 * entry, always present even when empty, so a column never has to guess
 * whether it was omitted because it was empty or because something broke.
 *
 * This is a straight partition on `run.state`, nothing more: the list
 * endpoint carries no node-run detail, so there is no way (and no need) to
 * distinguish *why* a run is waiting — an approval pause and any other
 * external wait both report `state: "waiting"` and both land here, under
 * "waiting", honestly (PRD honesty condition h5 — render committed API
 * state, never invent a distinction the API did not make).
 */
export function groupRunsByState(runs: Run[]): Record<RunState, Run[]> {
  const grouped = Object.fromEntries(
    RUN_STATE_COLUMNS.map((state) => [state, [] as Run[]]),
  ) as Record<RunState, Run[]>;
  for (const run of runs) {
    const bucket = grouped[run.state];
    if (bucket) bucket.push(run);
  }
  return grouped;
}

const RELATIVE_UNITS: { unit: Intl.RelativeTimeFormatUnit; seconds: number }[] = [
  { unit: "year", seconds: 31536000 },
  { unit: "month", seconds: 2592000 },
  { unit: "day", seconds: 86400 },
  { unit: "hour", seconds: 3600 },
  { unit: "minute", seconds: 60 },
];

/**
 * `updated_at` as relative time ("5 minutes ago"), the form the board's
 * cards render it in. `now` defaults to the real clock and is a parameter
 * purely so callers (and tests) can pin it — the function itself invents
 * nothing, it only formats the timestamp the API already sent.
 */
export function formatRelativeTime(iso: string, now: Date = new Date()): string {
  const then = new Date(iso);
  if (Number.isNaN(then.getTime())) return iso;

  const diffSeconds = Math.round((now.getTime() - then.getTime()) / 1000);
  if (Math.abs(diffSeconds) < 60) return "just now";

  const rtf = new Intl.RelativeTimeFormat("en", { numeric: "always" });
  for (const { unit, seconds } of RELATIVE_UNITS) {
    if (Math.abs(diffSeconds) >= seconds) {
      const value = Math.round(diffSeconds / seconds);
      return rtf.format(-value, unit);
    }
  }
  return "just now";
}
