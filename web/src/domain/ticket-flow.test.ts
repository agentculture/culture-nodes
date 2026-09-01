import { describe, expect, it } from "vitest";
import type { TicketProjection } from "../api/types";
import { TICKET_PROJECTION } from "../fixtures/ticket-fixture";
import { TICKET_STAGES, ticketFlow } from "./ticket-flow";

/**
 * The whole risk in drawing "where is this ticket" is that the picture starts
 * asserting things the API never said. These tests are about the refusals as
 * much as the derivation: a stage with no evidence stays un-evidenced, and
 * nothing anywhere names the reader as the person being waited on.
 */

function projection(overrides: Partial<TicketProjection> = {}): TicketProjection {
  return { ...structuredClone(TICKET_PROJECTION), ...overrides } as TicketProjection;
}

const BARE: TicketProjection = {
  ticket_id: "SCRUM-1",
  runs: [],
  ledger: [],
  human_tasks: [],
  ticket_reports: [],
  replies: [],
};

describe("ticketFlow", () => {
  it("names the five stages of the loop, in order", () => {
    const flow = ticketFlow(BARE);
    expect(flow.stages.map((stage) => stage.id)).toEqual([...TICKET_STAGES]);
    expect(flow.stages.map((stage) => stage.label)).toEqual([
      "Intake",
      "Spec",
      "Build",
      "Review",
      "Done",
    ]);
  });

  it("evidences intake alone for a ticket with nothing on it, and waits on nobody", () => {
    const flow = ticketFlow(BARE);
    expect(flow.current).toBe("intake");
    expect(flow.stages.filter((stage) => stage.reached)).toHaveLength(1);
    expect(flow.waitingOn).toBe("nobody");
    expect(flow.waitingLine).toContain("no run carries this ticket yet");
  });

  it("gives every stage a sentence, including the ones nothing proves", () => {
    for (const stage of ticketFlow(BARE).stages) {
      expect(stage.evidence.length).toBeGreaterThan(0);
    }
    const spec = ticketFlow(BARE).stages.find((stage) => stage.id === "spec");
    expect(spec?.reached).toBe(false);
    expect(spec?.evidence).toContain("no frame has been posted");
  });

  it("does NOT back-fill an un-evidenced stage behind an evidenced one", () => {
    // Runs exist, no frame was ever posted: build is real, spec is not, and
    // the rail must draw it that way rather than assuming the ladder.
    const flow = ticketFlow(
      projection({ latest_frame: undefined, pending_tasks: [], pending_records: [] }),
    );
    const byId = Object.fromEntries(flow.stages.map((stage) => [stage.id, stage]));
    expect(byId.build.reached).toBe(true);
    expect(byId.spec.reached).toBe(false);
    expect(flow.current).toBe("build");
  });

  it("reads the current stage as the LAST evidenced one", () => {
    const flow = ticketFlow(projection());
    expect(flow.current).toBe("review");
    expect(flow.stages.filter((stage) => stage.current)).toHaveLength(1);
  });

  it("says a person is waited on — never the reader — when something is pending", () => {
    const flow = ticketFlow(projection());
    expect(flow.waitingOn).toBe("person");
    expect(flow.waitingLine).toMatch(/^Waiting on a person/);
    expect(flow.waitingLine).not.toMatch(/\byou\b/);
    expect(flow.pendingCount).toBeGreaterThan(0);
  });

  it("waits on the engine while a run is still going and nothing is pending", () => {
    const flow = ticketFlow(
      projection({
        pending_tasks: [],
        pending_records: [],
        runs: [
          {
            id: "run-1",
            workflow_digest: "sha256:abc",
            state: "running",
            created_at: "2026-08-15T09:00:00Z",
            updated_at: "2026-08-15T09:00:00Z",
          },
        ],
      }),
    );
    expect(flow.waitingOn).toBe("engine");
    expect(flow.waitingLine).toContain("1 run is");
  });

  it("reads a frozen ticket as Done, waiting on nobody, quoting the API's own banner", () => {
    const flow = ticketFlow(
      projection({
        frozen: true,
        freeze: {
          reason: "ticket_frozen",
          cancelled_runs: 2,
          parked_runs: 0,
          banner: "Ticket status Done: 2 runs cancelled and 0 parked.",
        },
      }),
    );
    expect(flow.current).toBe("done");
    expect(flow.waitingOn).toBe("nobody");
    expect(
      flow.stages.find((stage) => stage.id === "done")?.evidence,
    ).toBe("Ticket status Done: 2 runs cancelled and 0 parked.");
  });

  it("tolerates a control plane older than t14/t18, which serves neither pending list", () => {
    const older = projection();
    delete older.pending_tasks;
    delete older.pending_records;
    const flow = ticketFlow(older);
    expect(flow.pendingCount).toBe(0);
    expect(flow.stages.find((stage) => stage.id === "review")?.reached).toBe(false);
  });
});
