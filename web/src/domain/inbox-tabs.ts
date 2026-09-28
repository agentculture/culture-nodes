import type { HumanTask } from "../api/types";

/**
 * The Inbox's three tabs (task t44, issue 332 follow-up). The owner's rule:
 * "By default show only what's open. (Not the waiting expired of the past or
 * decided), then add tabs for waiting, decided."
 *
 * - `open`: pending, with no deadline or a deadline still in the future —
 *   the actionable ones, and the default.
 * - `waiting`: pending, but the deadline has already passed and nobody
 *   decided (graph-era approvals, mostly). Still decidable.
 * - `decided`: every task that is no longer pending — decided, expired, or
 *   any other terminal status — newest first, read-only.
 */
export type InboxTab = "open" | "waiting" | "decided";

export const DEFAULT_INBOX_TAB: InboxTab = "open";

export const INBOX_TABS: readonly InboxTab[] = ["open", "waiting", "decided"];

/** `?tab=` → a tab; anything unknown or absent falls back to `open`. */
export function parseInboxTab(value: string | null): InboxTab {
  return value === "open" || value === "waiting" || value === "decided"
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

/** A pending task whose deadline is at or before `now`. */
export function isOverdue(task: HumanTask, now: Date): boolean {
  if (task.status !== "pending") return false;
  const deadline = taskDeadlineMs(task);
  return deadline !== null && deadline <= now.getTime();
}

export interface InboxPartition {
  open: HumanTask[];
  waiting: HumanTask[];
  decided: HumanTask[];
}

function resolvedMs(task: HumanTask): number {
  const ms = Date.parse(task.resolved_at ?? task.created_at);
  return Number.isNaN(ms) ? 0 : ms;
}

/**
 * Split every fetched task into the three tabs. Membership is decided by the
 * task's own `status`, not by which list call returned it, and a task seen
 * twice (one list call racing a decision) is kept once, first sighting wins.
 */
export function partitionInbox(
  tasks: readonly HumanTask[],
  now: Date,
): InboxPartition {
  const seen = new Set<string>();
  const out: InboxPartition = { open: [], waiting: [], decided: [] };
  for (const task of tasks) {
    if (seen.has(task.id)) continue;
    seen.add(task.id);
    if (task.status !== "pending") out.decided.push(task);
    else if (isOverdue(task, now)) out.waiting.push(task);
    else out.open.push(task);
  }
  // Newest resolution first; Array.prototype.sort is stable, so ties keep
  // the API's newest-created-first order.
  out.decided.sort((a, b) => resolvedMs(b) - resolvedMs(a));
  return out;
}
