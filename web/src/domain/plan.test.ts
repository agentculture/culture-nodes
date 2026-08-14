import { describe, expect, it } from "vitest";
import type { PlanImportTask } from "../api/types";
import {
  authorityForDeviationStatus,
  authorityForOrigin,
  authorityForTaskStatus,
  groupTasksByWave,
} from "./plan";

function task(overrides: Partial<PlanImportTask> & { task_ref: string }): PlanImportTask {
  return {
    summary: "",
    origin_kind: "agent",
    source_status: "proposed",
    depends_on: [],
    acceptance_criteria: [],
    covers: [],
    ...overrides,
  };
}

describe("groupTasksByWave", () => {
  it("buckets tasks by their computed wave, ascending, task_ref-sorted within a wave", () => {
    const tasks = [
      task({ task_ref: "t4", wave: 2 }),
      task({ task_ref: "t1", wave: 1 }),
      task({ task_ref: "t3", wave: 2 }),
      task({ task_ref: "t2", wave: 1 }),
    ];
    const { waves, unscheduled } = groupTasksByWave(tasks);
    expect(waves).toHaveLength(2);
    expect(waves[0]).toEqual({
      wave: 1,
      tasks: [tasks[1], tasks[3]],
    });
    expect(waves[1]).toEqual({
      wave: 2,
      tasks: [tasks[2], tasks[0]],
    });
    expect(unscheduled).toHaveLength(0);
  });

  it("puts a wave-less (rejected) task in the unscheduled bucket, never wave 0", () => {
    const rejected = task({ task_ref: "t5", source_status: "rejected", wave: undefined });
    const { waves, unscheduled } = groupTasksByWave([
      task({ task_ref: "t1", wave: 1 }),
      rejected,
    ]);
    expect(waves).toHaveLength(1);
    expect(unscheduled).toEqual([rejected]);
  });
});

describe("authorityForTaskStatus", () => {
  it("passes devague's task status straight through — the vocabularies already match", () => {
    expect(authorityForTaskStatus("proposed")).toBe("proposed");
    expect(authorityForTaskStatus("confirmed")).toBe("confirmed");
    expect(authorityForTaskStatus("rejected")).toBe("rejected");
  });

  it("falls back to proposed (never a false confirmed) for an unrecognised status", () => {
    expect(authorityForTaskStatus("weird")).toBe("proposed");
  });
});

describe("authorityForDeviationStatus", () => {
  it("maps devague's approved to the ledger's confirmed", () => {
    expect(authorityForDeviationStatus("approved")).toBe("confirmed");
  });

  it("passes proposed/rejected straight through", () => {
    expect(authorityForDeviationStatus("proposed")).toBe("proposed");
    expect(authorityForDeviationStatus("rejected")).toBe("rejected");
  });
});

describe("authorityForOrigin — the origin distinction this task exists for", () => {
  it("renders a human/user origin as confirmed (solid) — the user's own assertion", () => {
    expect(authorityForOrigin("human")).toBe("confirmed");
  });

  it("renders an agent/llm origin as proposed (dashed) — an unconfirmed system claim", () => {
    expect(authorityForOrigin("agent")).toBe("proposed");
  });

  it("origin and ratification status are independent axes: an approved llm-origin deviation still reads system-derived by origin", () => {
    // Same deviation, two different chips: origin never changes even once
    // a human ratifies the deviation's *status* — that is exactly the
    // point (see authorityForOrigin's doc comment).
    expect(authorityForOrigin("agent")).toBe("proposed");
    expect(authorityForDeviationStatus("approved")).toBe("confirmed");
  });
});
