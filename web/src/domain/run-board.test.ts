import { describe, expect, it } from "vitest";
import type { Run } from "../api/types";
import {
  formatRelativeTime,
  groupRunsByState,
  RUN_STATE_ACCENT_VAR,
  RUN_STATE_COLUMNS,
} from "./run-board";

function run(id: string, state: Run["state"], updated_at: string): Run {
  return {
    id,
    workflow_digest: "sha256:deadbeef",
    state,
    created_at: updated_at,
    updated_at,
  };
}

describe("RUN_STATE_COLUMNS", () => {
  it("is exactly the RunState enum from api/openapi/openapi.yaml, in its declared order", () => {
    // openapi.yaml: RunState enum: [created, running, waiting, completed, failed, cancelled]
    expect(RUN_STATE_COLUMNS).toEqual([
      "created",
      "running",
      "waiting",
      "completed",
      "failed",
      "cancelled",
    ]);
  });

  it("gives every column an accent token pulled from culture-design (no invented colour)", () => {
    for (const state of RUN_STATE_COLUMNS) {
      expect(RUN_STATE_ACCENT_VAR[state]).toMatch(/^var\(--/);
    }
  });
});

describe("groupRunsByState", () => {
  it("buckets runs under their own committed state, one bucket per RunState column", () => {
    const runs = [
      run("r-created", "created", "2026-08-09T09:00:00Z"),
      run("r-running", "running", "2026-08-09T09:01:00Z"),
      run("r-waiting", "waiting", "2026-08-09T09:02:00Z"),
      run("r-completed", "completed", "2026-08-09T09:03:00Z"),
      run("r-failed", "failed", "2026-08-09T09:04:00Z"),
      run("r-cancelled", "cancelled", "2026-08-09T09:05:00Z"),
    ];
    const grouped = groupRunsByState(runs);
    expect(grouped.created.map((r) => r.id)).toEqual(["r-created"]);
    expect(grouped.running.map((r) => r.id)).toEqual(["r-running"]);
    expect(grouped.waiting.map((r) => r.id)).toEqual(["r-waiting"]);
    expect(grouped.completed.map((r) => r.id)).toEqual(["r-completed"]);
    expect(grouped.failed.map((r) => r.id)).toEqual(["r-failed"]);
    expect(grouped.cancelled.map((r) => r.id)).toEqual(["r-cancelled"]);
  });

  it("puts a run waiting on an approval node under the same waiting column as any other wait", () => {
    // The list endpoint reports only run.state; it carries no node-run detail,
    // so an approval-paused run is indistinguishable from any other external
    // wait at this layer — and must not be invented a column of its own.
    const approvalPaused = run(
      "r-approval",
      "waiting",
      "2026-08-09T09:06:00Z",
    );
    const grouped = groupRunsByState([approvalPaused]);
    expect(grouped.waiting.map((r) => r.id)).toEqual(["r-approval"]);
    for (const state of RUN_STATE_COLUMNS) {
      if (state === "waiting") continue;
      expect(grouped[state]).toEqual([]);
    }
  });

  it("returns every column, even ones with no runs", () => {
    const grouped = groupRunsByState([]);
    for (const state of RUN_STATE_COLUMNS) {
      expect(grouped[state]).toEqual([]);
    }
  });

  it("preserves the caller's ordering within a column (the API already sorts newest-first)", () => {
    const runs = [
      run("r-2", "running", "2026-08-09T09:02:00Z"),
      run("r-1", "running", "2026-08-09T09:01:00Z"),
    ];
    const grouped = groupRunsByState(runs);
    expect(grouped.running.map((r) => r.id)).toEqual(["r-2", "r-1"]);
  });
});

describe("formatRelativeTime", () => {
  const now = new Date("2026-08-09T09:10:00Z");

  it("renders sub-minute gaps as just now", () => {
    expect(formatRelativeTime("2026-08-09T09:09:58Z", now)).toBe("just now");
  });

  it("renders minutes, hours and days ago", () => {
    expect(formatRelativeTime("2026-08-09T09:05:00Z", now)).toBe(
      "5 minutes ago",
    );
    expect(formatRelativeTime("2026-08-09T07:10:00Z", now)).toBe(
      "2 hours ago",
    );
    expect(formatRelativeTime("2026-08-06T09:10:00Z", now)).toBe(
      "3 days ago",
    );
  });

  it("falls back to the raw string for an unparseable time rather than inventing one", () => {
    expect(formatRelativeTime("not-a-date", now)).toBe("not-a-date");
  });
});
