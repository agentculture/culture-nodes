import { describe, expect, it } from "vitest";
import { meshEventsUrl } from "./client";

/**
 * The cross-run SSE URL builder (task t27, spec requirement c48's sibling
 * clause): resume cursor via `?from=`, plus the server's `?runs=id,id`
 * scope-down filter (internal/api/events.go:294-313's `runsFilterParam`)
 * that this client previously never exposed.
 */
describe("meshEventsUrl", () => {
  it("opens a new shared stream at the latest committed row", () => {
    expect(meshEventsUrl()).toBe("/v1alpha1/events?from=latest");
  });

  it("carries the resume cursor as ?from=", () => {
    expect(meshEventsUrl("01EVENT1")).toBe("/v1alpha1/events?from=01EVENT1");
  });

  it("carries a runs scope as a comma-joined ?runs=", () => {
    expect(meshEventsUrl(undefined, ["run-1", "run-2"])).toBe(
      "/v1alpha1/events?from=latest&runs=run-1,run-2",
    );
  });

  it("combines from and runs when both are given", () => {
    expect(meshEventsUrl("01EVENT1", ["run-1"])).toBe(
      "/v1alpha1/events?from=01EVENT1&runs=run-1",
    );
  });

  it("treats an empty runs list the same as absent — omitted, not sent empty", () => {
    expect(meshEventsUrl(undefined, [])).toBe("/v1alpha1/events?from=latest");
  });

  it("percent-encodes special characters in both from and each run id", () => {
    expect(meshEventsUrl("id with space", ["run/with/slash", "id&amp"])).toBe(
      "/v1alpha1/events?from=id%20with%20space&runs=run%2Fwith%2Fslash,id%26amp",
    );
  });
});
