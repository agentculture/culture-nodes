import type { HumanTask } from "../api/types";
import { taskDecidableOutcomes } from "./human-task-notice";

/**
 * The Inbox's tabs (task t44, issue 332 follow-up; widened to the one
 * decision page by task t46, owner decision d19). The owner's rule:
 * "By default show only what's open. (Not the waiting expired of the past or
 * decided), then add tabs for waiting, decided." — and then "one place to
 * decide", so the proposed ledger records the Decisions page used to list
 * are the `review` tab here.
 *
 * - `act`: pending, with no deadline or a deadline still in the future —
 *   the actionable ones, and the default. `?tab=open` (t44's name for it)
 *   still lands here.
 * - `review`: proposed ledger records awaiting a confirm/reject review.
 *   Not a task partition — the page fills it from pending-decisions.
 * - `waiting`: pending, but the deadline has already passed and nobody
 *   decided (graph-era approvals, mostly). Still decidable. Also every
 *   pending task that offers NO outcome a person may select (task t45): it
 *   is not something to act on now, so it does not count in Open — but it
 *   is never hidden either; its card says it "cannot be decided here".
 * - `decided`: every task that is no longer pending — decided, expired, or
 *   any other terminal status — newest first, read-only.
 */
export type InboxTab = "act" | "review" | "waiting" | "decided";

export const DEFAULT_INBOX_TAB: InboxTab = "act";

export const INBOX_TABS: readonly InboxTab[] = ["act", "review", "waiting", "decided"];

/**
 * `?tab=` → a tab; anything unknown or absent falls back to `act`. `open`
 * is t44's deep link for the same tab and keeps working.
 */
export function parseInboxTab(value: string | null): InboxTab {
  if (value === "open") return "act";
  return value === "act" || value === "review" || value === "waiting" || value === "decided"
    ? value
    : DEFAULT_INBOX_TAB;
}

/**
 * The task's deadline as epoch milliseconds, or null when it has none. A
 * missing, non-string or unparseable deadline is "no deadline" — the task
 * is treated as open rather than guessed overdue.
 */
export function taskDeadlineMs(task: HumanTask): number | null {
  const raw: unknown = task.request?.deadline;
  if (typeof raw !== "string" || raw.trim() === "") return null;
  const ms = Date.parse(raw);
  return Number.isNaN(ms) ? null : ms;
}

/**
 * A pending task with nothing a person may select — no declared outcome, or
 * only the engine's own `expired`. Since task t45 the API reports
 * `["acknowledged"]` for every notice, so this should not occur; the guard
 * keeps such a task visible (Waiting) instead of counting it as open work.
 */
export function isUndecidable(task: HumanTask): boolean {
  return task.status === "pending" && taskDecidableOutcomes(task).length === 0;
}

/** A pending task whose deadline is at or before `now`. */
export function isOverdue(task: HumanTask, now: Date): boolean {
  if (task.status !== "pending") return false;
  const deadline = taskDeadlineMs(task);
  return deadline !== null && deadline <= now.getTime();
}

export interface InboxPartition {
  act: HumanTask[];
  waiting: HumanTask[];
  decided: HumanTask[];
}

function resolvedMs(task: HumanTask): number {
  const ms = Date.parse(task.resolved_at ?? task.created_at);
  return Number.isNaN(ms) ? 0 : ms;
}

/**
 * Split every fetched task into its tab (every tab but `review`). Membership is decided by the
 * task's own `status`, not by which list call returned it, and a task seen
 * twice (one list call racing a decision) is kept once, first sighting wins.
 */
export function partitionInbox(
  tasks: readonly HumanTask[],
  now: Date,
): InboxPartition {
  const seen = new Set<string>();
  const out: InboxPartition = { act: [], waiting: [], decided: [] };
  for (const task of tasks) {
    if (seen.has(task.id)) continue;
    seen.add(task.id);
    if (task.status !== "pending") out.decided.push(task);
    else if (isOverdue(task, now) || isUndecidable(task)) out.waiting.push(task);
    else out.act.push(task);
  }
  // Newest resolution first; Array.prototype.sort is stable, so ties keep
  // the API's newest-created-first order.
  out.decided.sort((a, b) => resolvedMs(b) - resolvedMs(a));
  return out;
}
