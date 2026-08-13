import { describe, expect, it } from "vitest";
import { parseWorkspaceEvidence } from "./evidence";

describe("parseWorkspaceEvidence", () => {
  it("reads changed_paths, snapshot_digest, and artifact_refs — the exact shape task t12's buildEvidence appends", () => {
    const data = {
      producer_id: "runner://headspace/hooks",
      collection_method: "workspace_snapshot_diff",
      completeness: "complete",
      changed_paths: ["internal/worker/hooks.go", "internal/runners/dispatch.go"],
      snapshot_digest: `sha256:${"c".repeat(64)}`,
      artifact_refs: ["artifact://diff/att_workspace_snapshot"],
    };
    expect(parseWorkspaceEvidence(data)).toEqual({
      changedPaths: ["internal/worker/hooks.go", "internal/runners/dispatch.go"],
      snapshotDigest: `sha256:${"c".repeat(64)}`,
      artifactRefs: ["artifact://diff/att_workspace_snapshot"],
      diffstat: undefined,
    });
  });

  it("recognizes a top-level diffstat string defensively, though no shipped producer sets it yet", () => {
    const data = { changed_paths: ["a.go"], diffstat: " 1 file changed, 2 insertions(+)" };
    expect(parseWorkspaceEvidence(data)?.diffstat).toBe(
      " 1 file changed, 2 insertions(+)",
    );
  });

  it("also recognizes diffstat nested under the evidence schema's own measurements object", () => {
    const data = { measurements: { diffstat: "1 file changed" } };
    expect(parseWorkspaceEvidence(data)?.diffstat).toBe("1 file changed");
  });

  it("returns null for evidence data with none of the workspace-snapshot fields — e.g. the test node's exit-status evidence", () => {
    expect(parseWorkspaceEvidence({ process_exit: 0, workspace_diff: true })).toBeNull();
  });

  it("returns null for non-object data without throwing", () => {
    expect(parseWorkspaceEvidence(undefined)).toBeNull();
    expect(parseWorkspaceEvidence(null)).toBeNull();
    expect(parseWorkspaceEvidence("a string")).toBeNull();
    expect(parseWorkspaceEvidence(42)).toBeNull();
  });

  it("ignores a changed_paths that isn't actually an array of strings", () => {
    expect(parseWorkspaceEvidence({ changed_paths: "not-an-array" })).toBeNull();
    expect(parseWorkspaceEvidence({ changed_paths: [1, 2] })).toBeNull();
  });
});
