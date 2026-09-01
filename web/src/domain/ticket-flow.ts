import type { TicketProjection } from "../api/types";
import { ACTIVE_RUN_STATES } from "./active-presence";

/**
 * Where a ticket is in the loop, derived from its projection (task t17,
 * spec c25, issue #270).
 *
 * The operator's read of the site was "intimidating: blocks of text and
 * lists". The first thing a person on a Jira link needs is not a list, it is
 * an orientation: *where is this, and who is it waiting on*. Nothing in the
 * API answers that — there is no stage column on a ticket — so this module
 * derives it, and the whole design of the derivation is about not inventing
 * the answer:
 *
 *   - Each stage is `reached` only when a FIELD OF THE PROJECTION proves it,
 *     and every stage carries the sentence naming that proof. A stage with
 *     no proof renders as not evidenced, never as "probably done".
 *   - The stages are therefore independent, not a monotonic ladder. A ticket
 *     whose runs exist but whose frame was never posted really does have an
 *     un-evidenced `spec` behind an evidenced `build`, and the rail draws it
 *     that way rather than back-filling it.
 *   - `current` is the LAST evidenced stage, so it is a statement about
 *     evidence, not a guess about intent.
 *
 * Who it waits on is the same discipline. The control plane does not route a
 * human task to a named person — `human_tasks` carries `assigned_owner_id`,
 * a role/team reference, and no binding to a signed-in principal — so this
 * can honestly say "a person" and never "you specifically". What it can say
 * about the reader is whether they are able to act, which the page adds from
 * whoami.
 */

export const TICKET_STAGES = [
  "intake",
  "spec",
  "build",
  "review",
  "done",
] as const;

export type TicketStageId = (typeof TICKET_STAGES)[number];

export interface TicketStage {
  id: TicketStageId;
  /** The word on the diagram. */
  label: string;
  /** True only when a field of the projection proves this stage happened. */
  reached: boolean;
  /** The proof, or the plain statement that there is none. */
  evidence: string;
  /** The last reached stage — where the ticket is now. */
  current: boolean;
}

/** Who the ticket is waiting on, as far as the projection can say. */
export type TicketWaitingOn = "person" | "engine" | "nobody";

export interface TicketFlow {
  stages: TicketStage[];
  current: TicketStageId;
  waitingOn: TicketWaitingOn;
  /** One sentence: who it waits on, and the fact that establishes it. */
  waitingLine: string;
  /** Pending human tasks plus runs with undecided claims — what a person can act on. */
  pendingCount: number;
}

const LABELS: Record<TicketStageId, string> = {
  intake: "Intake",
  spec: "Spec",
  build: "Build",
  review: "Review",
  done: "Done",
};

/** Report phases that evidence a stage, matched case-insensitively. */
const PHASES: Record<TicketStageId, readonly string[]> = {
  intake: ["intake", "triage"],
  spec: ["spec", "scope", "plan", "frame"],
  build: ["build", "implement", "develop"],
  review: ["review", "gate", "merge-gate"],
  done: ["done", "delivered"],
};

function reportPhases(projection: TicketProjection): string[] {
  return projection.ticket_reports.map((report) =>
    (report.phase ?? "").trim().toLowerCase(),
  );
}

function hasPhase(projection: TicketProjection, stage: TicketStageId): boolean {
  const wanted = PHASES[stage];
  return reportPhases(projection).some((phase) => wanted.includes(phase));
}

function plural(count: number, one: string, many: string): string {
  return `${count} ${count === 1 ? one : many}`;
}

/** The projection's own view of where the ticket is. */
export function ticketFlow(projection: TicketProjection): TicketFlow {
  const runs = projection.runs ?? [];
  const pendingTasks = projection.pending_tasks ?? [];
  const pendingRecords = projection.pending_records ?? [];
  const frame = projection.latest_frame;
  const frozen = projection.frozen === true;
  const activeRuns = runs.filter((run) => ACTIVE_RUN_STATES.has(run.state));
  const pendingCount = pendingTasks.length + pendingRecords.length;

  const reached: Record<TicketStageId, { reached: boolean; evidence: string }> = {
    // The projection answered at all, which is the only thing intake means:
    // this ticket is known to the control plane.
    intake: {
      reached: true,
      evidence: `the control plane holds a projection for ${projection.ticket_id}`,
    },
    spec: frame
      ? {
          reached: true,
          evidence: `frame v${frame.version} was posted by ${frame.posted_by}`,
        }
      : hasPhase(projection, "spec")
        ? { reached: true, evidence: "a spec-phase report was filed" }
        : {
            reached: false,
            evidence: "no frame has been posted and no spec report filed",
          },
    build:
      runs.length > 0
        ? {
            reached: true,
            evidence: `${plural(runs.length, "run", "runs")} carry this ticket`,
          }
        : hasPhase(projection, "build")
          ? { reached: true, evidence: "a build-phase report was filed" }
          : { reached: false, evidence: "no run carries this ticket yet" },
    review:
      pendingCount > 0
        ? {
            reached: true,
            evidence: `${plural(pendingTasks.length, "decision", "decisions")} and ${plural(
              pendingRecords.length,
              "run",
              "runs",
            )} of claims are awaiting a person`,
          }
        : hasPhase(projection, "review")
          ? { reached: true, evidence: "a review-phase report was filed" }
          : {
              reached: false,
              evidence: "nothing on this ticket is awaiting a person",
            },
    done: frozen
      ? {
          reached: true,
          evidence:
            projection.freeze?.banner ?? "the ticket is frozen; its runs are ended",
        }
      : { reached: false, evidence: "the ticket is not frozen" },
  };

  const evidenced = TICKET_STAGES.filter((id) => reached[id].reached);
  const current = evidenced[evidenced.length - 1] ?? "intake";

  const stages: TicketStage[] = TICKET_STAGES.map((id) => ({
    id,
    label: LABELS[id],
    reached: reached[id].reached,
    evidence: reached[id].evidence,
    current: id === current,
  }));

  return { stages, current, waitingOn: waitingOn(), waitingLine: waitingLine(), pendingCount };

  function waitingOn(): TicketWaitingOn {
    if (frozen) return "nobody";
    if (pendingCount > 0) return "person";
    if (activeRuns.length > 0) return "engine";
    return "nobody";
  }

  function waitingLine(): string {
    if (frozen) {
      return "Waiting on nobody — this ticket is frozen and its runs have ended.";
    }
    if (pendingCount > 0) {
      const parts: string[] = [];
      if (pendingTasks.length > 0) {
        parts.push(plural(pendingTasks.length, "decision", "decisions"));
      }
      if (pendingRecords.length > 0) {
        parts.push(
          `${plural(pendingRecords.length, "run", "runs")} of claims to review`,
        );
      }
      // Deliberately "a person", not "you": no human task on this ticket is
      // routed to a named principal, so naming the reader would be invented.
      return `Waiting on a person — ${parts.join(" and ")}.`;
    }
    if (activeRuns.length > 0) {
      return `Waiting on the engine — ${plural(
        activeRuns.length,
        "run is",
        "runs are",
      )} still going.`;
    }
    if (runs.length > 0) {
      return "Waiting on nobody — every run has ended and nothing is pending.";
    }
    return "Waiting on nobody — no run carries this ticket yet.";
  }
}
