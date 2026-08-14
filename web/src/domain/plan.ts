import type {
  LedgerAuthorityValue,
  PlanImportOriginKind,
  PlanImportTask,
} from "../api/types";

/**
 * One wave of a plan import (task t23, issue #45): every task whose
 * `wave` equals this bucket's index, in `task_ref` order for a stable
 * render. Waves are already computed server-side (internal/devague's
 * `planTaskWaves`, over the plan's REAL per-task `depends_on` edges — never
 * the lossy `plan waves --json` reading, per spec claim c15) — this module
 * only buckets the already-computed field, it does not lay out a graph.
 */
export interface PlanWave {
  wave: number;
  tasks: PlanImportTask[];
}

/**
 * Buckets a plan import's tasks by their (server-computed) `wave`, ascending,
 * plus a separate `unscheduled` bucket for tasks with no wave at all — which
 * `internal/devague.planTaskWaves` only ever leaves nil for a REJECTED task
 * (see PlanTask.Wave's doc comment): a rejected task never ships, so it
 * occupies no wave, and this function renders that as an honest third
 * category rather than folding it into wave 0 or hiding it.
 */
export function groupTasksByWave(tasks: PlanImportTask[]): {
  waves: PlanWave[];
  unscheduled: PlanImportTask[];
} {
  const byWave = new Map<number, PlanImportTask[]>();
  const unscheduled: PlanImportTask[] = [];
  for (const task of tasks) {
    if (task.wave === undefined) {
      unscheduled.push(task);
      continue;
    }
    const bucket = byWave.get(task.wave);
    if (bucket) bucket.push(task);
    else byWave.set(task.wave, [task]);
  }

  const waves = [...byWave.entries()]
    .sort(([a], [b]) => a - b)
    .map(([wave, waveTasks]) => ({
      wave,
      tasks: [...waveTasks].sort((a, b) => a.task_ref.localeCompare(b.task_ref)),
    }));

  return {
    waves,
    unscheduled: [...unscheduled].sort((a, b) =>
      a.task_ref.localeCompare(b.task_ref),
    ),
  };
}

/**
 * A task's/deviation's own `source_status` -> `AuthorityChip`'s vocabulary
 * (task t23). devague spells a TASK's ratified state exactly the way the
 * ledger authority model does (`proposed` | `confirmed` | `rejected`), so
 * this is a direct pass-through, not a translation — see
 * `authorityForDeviationStatus` below for the one place devague's own
 * vocabulary actually diverges (a DEVIATION's ratified state is spelled
 * `approved`, not `confirmed` — internal/devague/deviations.go's
 * `reviewForDeviationStatus` doc comment).
 */
export function authorityForTaskStatus(
  sourceStatus: string,
): LedgerAuthorityValue {
  if (
    sourceStatus === "proposed" ||
    sourceStatus === "confirmed" ||
    sourceStatus === "rejected"
  ) {
    return sourceStatus;
  }
  // An unrecognised status is a data problem, not a rendering decision this
  // module should paper over — falling back to "proposed" (the weakest,
  // most-visually-flagged authority) is the honest default: better to look
  // unconfirmed than to falsely look confirmed.
  return "proposed";
}

/**
 * A DEVIATION's own `source_status` -> `AuthorityChip`'s vocabulary.
 * devague spells a deviation's ratified state `approved`, not `confirmed`
 * (internal/devague/deviations.go's `reviewForDeviationStatus`) — the same
 * fact, different word, so this maps it rather than passing it through.
 */
export function authorityForDeviationStatus(
  sourceStatus: string,
): LedgerAuthorityValue {
  if (sourceStatus === "approved") return "confirmed";
  if (sourceStatus === "proposed" || sourceStatus === "rejected") {
    return sourceStatus;
  }
  return "proposed";
}

/**
 * A deviation's `origin_kind` -> `AuthorityChip`'s vocabulary — the
 * honesty-condition-bearing mapping this whole task exists for (spec c15/
 * h11: "deviations render with origin user vs llm visibly distinguished
 * using the AuthorityChip vocabulary").
 *
 * This is not a second, invented meaning for `AuthorityChip` — it restates
 * the PRD's own ledger authority model in chip form: "agents may only
 * create `proposed` records ... no actor promotes its own proposal"
 * (CLAUDE.md's ledger authority model, PRD §10.4) means an agent/`llm`
 * origin can only ever be an unconfirmed claim — DASHED, `proposed` — no
 * matter what devague's own `source_status` says about it later. A
 * `human`/`user` origin is the one origin the ledger model lets stand on
 * its own authority — SOLID, `confirmed` — matching devague's own default
 * behaviour (`internal/devague/deviations.go`'s doc comment: a user-origin
 * deviation auto-approves; an llm-origin one needs an explicit human
 * `--confirm`).
 *
 * This is deliberately a DIFFERENT axis from `authorityForDeviationStatus`
 * above (which reads the deviation's actual ratification state) — a
 * llm-origin deviation that a human later approved still renders as
 * "system-derived" here (origin never changes), while its OWN status chip
 * (rendered alongside, never instead) shows `confirmed`. Conflating the two
 * into one chip would silently lose whichever fact lost the argument; the
 * plan view renders both, clearly labelled, so neither is.
 */
export function authorityForOrigin(
  origin: PlanImportOriginKind,
): LedgerAuthorityValue {
  return origin === "human" ? "confirmed" : "proposed";
}

/** The words a deviation's origin renders as, alongside its chip. */
export const ORIGIN_LABEL: Record<PlanImportOriginKind, string> = {
  human: "user reports",
  agent: "system knows",
};
