import { describe, expect, it } from "vitest";
import { PRESET_LABEL, presetSince, TIME_RANGE_PRESETS } from "./time-range";

describe("presetSince", () => {
  it("resolves 1h to exactly one hour before the given instant", () => {
    const now = new Date("2026-08-09T12:00:00.000Z");
    expect(presetSince("1h", now)).toBe("2026-08-09T11:00:00.000Z");
  });

  it("resolves 24h to exactly one day before the given instant", () => {
    const now = new Date("2026-08-09T12:00:00.000Z");
    expect(presetSince("24h", now)).toBe("2026-08-08T12:00:00.000Z");
  });

  it("resolves 7d to exactly seven days before the given instant", () => {
    const now = new Date("2026-08-09T12:00:00.000Z");
    expect(presetSince("7d", now)).toBe("2026-08-02T12:00:00.000Z");
  });

  it("defaults to the real clock when now is omitted", () => {
    const before = Date.now();
    const resolved = new Date(presetSince("1h")).getTime();
    // resolved should be ~1h before "before" (within a second of slack).
    expect(before - resolved).toBeGreaterThanOrEqual(60 * 60 * 1000 - 1000);
    expect(before - resolved).toBeLessThan(60 * 60 * 1000 + 5000);
  });
});

describe("TIME_RANGE_PRESETS / PRESET_LABEL", () => {
  it("declares 1h, 24h, 7d in that order", () => {
    expect(TIME_RANGE_PRESETS).toEqual(["1h", "24h", "7d"]);
  });

  it("has a non-empty label for every declared preset", () => {
    for (const preset of TIME_RANGE_PRESETS) {
      expect(PRESET_LABEL[preset]).toBeTruthy();
    }
  });
});
