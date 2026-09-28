/**
 * Notices (task t45, issue 332 follow-up): human tasks that inform rather
 * than ask. The control plane raises two — `trigger_remint_exhausted` (a
 * trigger-created run failed and its bounded re-mints are spent) and
 * `schedule_failing` (a schedule keeps failing with the same reason). Neither
 * parks a node run; the one answer either takes is `acknowledged`, which
 * records who read it and moves nothing.
 *
 * Pure, like human-task-context.ts: the card renders from one list response.
 */

import type { HumanTask } from "../api/types";

export const OUTCOME_ACKNOWLEDGED = "acknowledged";

/** Mirrors engine.OutcomeExpired: never a decider's choice (issue 265). */
export const ENGINE_ONLY_OUTCOME = "expired";

const NOTICE_TITLES: Record<string, string> = {
  trigger_remint_exhausted: "Trigger re-mint attempts exhausted",
  schedule_failing: "Schedule keeps failing",
};

export function isNoticeTask(task: Pick<HumanTask, "kind">): boolean {
  return task.kind in NOTICE_TITLES;
}

/**
 * The outcomes the decision endpoint will accept: the API's top-level
 * `allowed_outcomes` (which reads a legacy notice as `["acknowledged"]`),
 * falling back to the stored request for a server that predates it. Never
 * invented client-side — the server is the one place the rule lives.
 */
export function taskAllowedOutcomes(task: HumanTask): string[] {
  return task.allowed_outcomes ?? task.request?.allowed_outcomes ?? [];
}

/** The subset a person may select: everything but the engine-only outcome. */
export function taskDecidableOutcomes(task: HumanTask): string[] {
  return taskAllowedOutcomes(task).filter((o) => o !== ENGINE_ONLY_OUTCOME);
}

export interface NoticeFact {
  label: string;
  value: string;
  /** Set when the value is a run id the card can link to. */
  runId?: string;
}

export interface NoticeView {
  title: string;
  reason?: string;
  facts: NoticeFact[];
}

type Obj = Record<string, unknown>;

const text = (value: unknown): string | undefined => {
  if (typeof value === "string" && value.trim() !== "") return value;
  if (typeof value === "number" && Number.isFinite(value)) return String(value);
  return undefined;
};

/** 86400 → "24h", 90 → "90s", 5400 → "90m". */
export function formatWindowSeconds(value: unknown): string | undefined {
  const n = typeof value === "number" ? value : Number(text(value));
  if (!Number.isFinite(n) || n <= 0) return undefined;
  if (n % 86400 === 0 && n >= 172800) return `${n / 86400}d`;
  if (n % 3600 === 0) return `${n / 3600}h`;
  if (n % 60 === 0) return `${n / 60}m`;
  return `${n}s`;
}

/**
 * What a notice says, readably. Only fields the task actually carries are
 * listed; nothing is inferred.
 */
export function noticeView(task: HumanTask): NoticeView {
  const request = (task.request ?? {}) as Obj;
  const facts: NoticeFact[] = [];
  const add = (label: string, value: string | undefined, runId?: boolean) => {
    if (value) facts.push(runId ? { label, value, runId: value } : { label, value });
  };
  if (task.kind === "trigger_remint_exhausted") {
    add("subject", text(request.subject));
    add("attempts", text(request.attempts));
    const window = formatWindowSeconds(request.window_seconds);
    add("window", window ? `within ${window}` : undefined);
    add("original event", text(request.original_event_id));
  } else if (task.kind === "schedule_failing") {
    add("schedule", text(request.schedule_name) ?? text(request.schedule_id));
    add("event", text(request.event_name));
    add("consecutive failures", text(request.consecutive_failures));
    add("last failed run", text(request.last_failed_run_id), true);
    add("proposed action", text(request.proposed_action));
  }
  return {
    title: NOTICE_TITLES[task.kind] ?? task.kind,
    reason: text(request.reason),
    facts,
  };
}
