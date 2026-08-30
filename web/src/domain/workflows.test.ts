import { describe, expect, it } from "vitest";
import type { Run, WorkflowIR, WorkflowVersion } from "../api/types";
import {
  groupWorkflowVersions,
  RECENT_RUNS_LIMIT,
  withRunsByWorkflowKey,
} from "./workflows";

function ir(ownerRef?: string): WorkflowIR {
  return {
    metadata: ownerRef ? { ownerRef } : undefined,
    spec: { entry: "start", nodes: {}, edges: [] },
  };
}

function version(
  workflowKey: string,
  versionNumber: number,
  digest: string,
  ownerRef?: string,
  createdAt = "2026-08-09T09:00:00Z",
): WorkflowVersion {
  return {
    id: `wfv-${workflowKey}-${versionNumber}`,
    workflow_key: workflowKey,
    version: versionNumber,
    source_format: "yaml",
    source: "# fixture\n",
    normalized_ir: ir(ownerRef),
    digest,
    created_at: createdAt,
  };
}

function run(id: string, workflowDigest: string, updatedAt: string): Run {
  return {
    id,
    workflow_digest: workflowDigest,
    state: "completed",
    created_at: updatedAt,
    updated_at: updatedAt,
  };
}

describe("groupWorkflowVersions", () => {
  it("groups versions by workflow_key, newest version first", () => {
    const versions = [
      version("deliver-change", 1, "sha256:v1", "team/a"),
      version("deliver-change", 2, "sha256:v2", "team/a"),
      version("hello-world", 1, "sha256:h1", "team/b"),
    ];
    const groups = groupWorkflowVersions(versions);
    expect(groups.map((g) => g.workflowKey)).toEqual([
      "deliver-change",
      "hello-world",
    ]);
    const deliver = groups.find((g) => g.workflowKey === "deliver-change")!;
    expect(deliver.versions.map((v) => v.version)).toEqual([2, 1]);
  });

  it("orders groups alphabetically by workflow_key", () => {
    const versions = [
      version("zeta", 1, "sha256:z1"),
      version("alpha", 1, "sha256:a1"),
    ];
    const groups = groupWorkflowVersions(versions);
    expect(groups.map((g) => g.workflowKey)).toEqual(["alpha", "zeta"]);
  });

  it("derives owner from the latest version's normalized_ir.metadata.ownerRef", () => {
    const versions = [
      version("deliver-change", 1, "sha256:v1", "team/old-owner"),
      version("deliver-change", 2, "sha256:v2", "team/new-owner"),
    ];
    const groups = groupWorkflowVersions(versions);
    expect(groups[0].owner).toBe("team/new-owner");
  });

  it("reports an undefined owner rather than inventing one when metadata carries none", () => {
    const groups = groupWorkflowVersions([version("no-owner", 1, "sha256:n1")]);
    expect(groups[0].owner).toBeUndefined();
  });

  it("starts every group with no recent runs — that is a separate, explicit step", () => {
    const groups = groupWorkflowVersions([version("deliver-change", 1, "sha256:v1")]);
    expect(groups[0].recentRuns).toEqual([]);
  });

  it("returns an empty list for no versions", () => {
    expect(groupWorkflowVersions([])).toEqual([]);
  });
});

describe("withRunsByWorkflowKey", () => {
  it("attaches the runs the server answered for that group's own workflow_key", () => {
    const groups = groupWorkflowVersions([
      version("deliver-change", 1, "sha256:v1"),
      version("deliver-change", 2, "sha256:v2"),
      version("hello-world", 1, "sha256:h1"),
    ]);
    const withRuns = withRunsByWorkflowKey(
      groups,
      new Map([
        [
          "deliver-change",
          [
            run("run-v2", "sha256:v2", "2026-08-09T09:02:00Z"),
            run("run-v1", "sha256:v1", "2026-08-09T09:01:00Z"),
          ],
        ],
        ["hello-world", [run("run-h1", "sha256:h1", "2026-08-09T09:03:00Z")]],
      ]),
    );
    const deliver = withRuns.find((g) => g.workflowKey === "deliver-change")!;
    expect(deliver.recentRuns.map((r) => r.id)).toEqual(["run-v2", "run-v1"]);
    const hello = withRuns.find((g) => g.workflowKey === "hello-world")!;
    expect(hello.recentRuns.map((r) => r.id)).toEqual(["run-h1"]);
  });

  // The task t8 defect, at the domain layer: the previous implementation
  // filtered ONE global run list by digest, so a run of this workflow that
  // fell outside that list's page simply vanished and the card claimed the
  // workflow had never run. A per-key answer can only ever be empty when
  // the workflow genuinely has no runs.
  it("never empties a group whose key the server answered with runs, whatever else is running", () => {
    const groups = groupWorkflowVersions([version("deliver-change", 1, "sha256:v1")]);
    const withRuns = withRunsByWorkflowKey(
      groups,
      new Map([["deliver-change", [run("run-v1", "sha256:v1", "2026-08-09T09:01:00Z")]]]),
    );
    expect(withRuns[0].recentRuns).toHaveLength(1);
  });

  it("leaves a group with no runs empty — the honest 'No runs yet' case", () => {
    const groups = groupWorkflowVersions([
      version("deliver-change", 1, "sha256:v1"),
      version("never-run", 1, "sha256:n1"),
    ]);
    const withRuns = withRunsByWorkflowKey(
      groups,
      new Map([
        ["deliver-change", [run("run-v1", "sha256:v1", "2026-08-09T09:01:00Z")]],
        ["never-run", []],
      ]),
    );
    expect(withRuns.find((g) => g.workflowKey === "never-run")!.recentRuns).toEqual([]);
  });

  it("leaves a group the map has no entry for empty rather than inventing runs", () => {
    const groups = groupWorkflowVersions([version("deliver-change", 1, "sha256:v1")]);
    expect(withRunsByWorkflowKey(groups, new Map())[0].recentRuns).toEqual([]);
  });

  it("preserves the server's run ordering (it already sorts newest-first)", () => {
    const groups = groupWorkflowVersions([version("deliver-change", 1, "sha256:v1")]);
    const withRuns = withRunsByWorkflowKey(
      groups,
      new Map([
        [
          "deliver-change",
          [
            run("run-newer", "sha256:v1", "2026-08-09T09:05:00Z"),
            run("run-older", "sha256:v1", "2026-08-09T09:01:00Z"),
          ],
        ],
      ]),
    );
    expect(withRuns[0].recentRuns.map((r) => r.id)).toEqual([
      "run-newer",
      "run-older",
    ]);
  });

  it("caps recent runs at the given limit, default RECENT_RUNS_LIMIT", () => {
    const groups = groupWorkflowVersions([version("deliver-change", 1, "sha256:v1")]);
    const runs = Array.from({ length: RECENT_RUNS_LIMIT + 3 }, (_, i) =>
      run(`run-${i}`, "sha256:v1", `2026-08-09T09:${String(i).padStart(2, "0")}:00Z`),
    );
    const byKey = new Map([["deliver-change", runs]]);
    expect(withRunsByWorkflowKey(groups, byKey)[0].recentRuns).toHaveLength(
      RECENT_RUNS_LIMIT,
    );
    expect(withRunsByWorkflowKey(groups, byKey, 2)[0].recentRuns).toHaveLength(2);
  });

  it("does not mutate the groups it was given", () => {
    const groups = groupWorkflowVersions([version("deliver-change", 1, "sha256:v1")]);
    withRunsByWorkflowKey(
      groups,
      new Map([["deliver-change", [run("run-v1", "sha256:v1", "2026-08-09T09:01:00Z")]]]),
    );
    expect(groups[0].recentRuns).toEqual([]);
  });
});
