import { describe, expect, it } from "vitest";
import { preserveBranchInfo } from "./preserve";

describe("preserveBranchInfo", () => {
  it("returns null when the attempt reported no preserve branch", () => {
    expect(preserveBranchInfo({})).toBeNull();
    expect(
      preserveBranchInfo({ preserve_branch: undefined, preserve_pushed: true }),
    ).toBeNull();
  });

  it("returns null for an empty branch string, treating it the same as absent", () => {
    expect(preserveBranchInfo({ preserve_branch: "" })).toBeNull();
  });

  it("surfaces a pushed branch with pushed:true and no href when no forge template is configured", () => {
    const info = preserveBranchInfo({
      preserve_branch: "preserve/run-01J-att-01K",
      preserve_pushed: true,
      preserve_remote: "origin",
    });
    expect(info).toEqual({
      branch: "preserve/run-01J-att-01K",
      pushed: true,
      remote: "origin",
    });
    expect(info?.href).toBeUndefined();
  });

  it("builds a link ONLY when pushed is true AND a forge template is configured", () => {
    const info = preserveBranchInfo(
      {
        preserve_branch: "preserve/run-01J-att-01K",
        preserve_pushed: true,
        preserve_remote: "origin",
      },
      "https://forge.example/org/repo/tree/{branch}",
    );
    // encodeURIComponent encodes "/" too — the branch name is opaque data
    // substituted into one path segment, not a path of its own.
    expect(info?.href).toBe(
      "https://forge.example/org/repo/tree/preserve%2Frun-01J-att-01K",
    );
  });

  it("never builds a link for a local-only branch, even with a forge template configured", () => {
    const info = preserveBranchInfo(
      {
        preserve_branch: "preserve/run-01J-att-01L",
        preserve_pushed: false,
        preserve_remote: "origin",
      },
      "https://forge.example/org/repo/tree/{branch}",
    );
    expect(info?.pushed).toBe(false);
    expect(info?.href).toBeUndefined();
  });

  it("treats preserve_pushed left undefined as local-only (never inferred as pushed)", () => {
    const info = preserveBranchInfo(
      { preserve_branch: "preserve/run-01J-att-01M" },
      "https://forge.example/org/repo/tree/{branch}",
    );
    expect(info?.pushed).toBe(false);
    expect(info?.href).toBeUndefined();
  });

  it("percent-encodes the branch name when building a link", () => {
    const info = preserveBranchInfo(
      {
        preserve_branch: "preserve/weird branch#name",
        preserve_pushed: true,
      },
      "https://forge.example/org/repo/tree/{branch}",
    );
    expect(info?.href).toBe(
      "https://forge.example/org/repo/tree/preserve%2Fweird%20branch%23name",
    );
  });

  it("omits remote from the result when the attempt reported none", () => {
    const info = preserveBranchInfo({
      preserve_branch: "preserve/run-01J-att-01N",
      preserve_pushed: true,
    });
    expect(info?.remote).toBeUndefined();
  });
});
