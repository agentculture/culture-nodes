import { describe, expect, it } from "vitest";
import { isMachineCheckableSignal } from "./success-signal";

describe("isMachineCheckableSignal", () => {
  it("recognizes an explicit mechanical:true signal", () => {
    expect(
      isMachineCheckableSignal({
        statement: "the test process exits 0",
        check: { kind: "process_exit", equals: 0 },
        mechanical: true,
      }),
    ).toBe(true);
  });

  it("treats mechanical:false as not machine-checkable, whatever the check block says", () => {
    expect(
      isMachineCheckableSignal({
        statement: "the change reads well to a reviewer",
        check: { kind: "process_exit", equals: 0 },
        mechanical: false,
      }),
    ).toBe(false);
  });

  it("defaults to not machine-checkable when the flag is absent or mistyped", () => {
    // The Phase-0 payload is loose; a signal that never said a machine can
    // check it has not earned the machine-checkable rendering.
    expect(isMachineCheckableSignal({ statement: "vague" })).toBe(false);
    expect(isMachineCheckableSignal({ mechanical: "true" })).toBe(false);
    expect(isMachineCheckableSignal(undefined)).toBe(false);
    expect(isMachineCheckableSignal(null)).toBe(false);
    expect(isMachineCheckableSignal("mechanical")).toBe(false);
  });
});
