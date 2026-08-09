/**
 * The jobs timeline's time-range filter vocabulary (task t15): three fixed
 * lookback presets plus a custom since/until pair.
 *
 * A preset resolves to a concrete `since` instant the moment it is chosen —
 * it is not a live "recompute every render" window. That is what makes the
 * URL a preset click produces (JobsTimeline's `/jobs` route, via
 * `updated_since`/`updated_until` mapped onto `since`/`until` search
 * params) actually shareable and bookmarkable: opening the link later
 * replays the exact instant the click captured, rather than silently
 * sliding the window forward to a new "now".
 */

export const TIME_RANGE_PRESETS = ["1h", "24h", "7d"] as const;

export type TimeRangePresetKey = (typeof TIME_RANGE_PRESETS)[number];

/** Human label per preset, in the order the filter control renders them. */
export const PRESET_LABEL: Record<TimeRangePresetKey, string> = {
  "1h": "Last hour",
  "24h": "Last 24h",
  "7d": "Last 7 days",
};

const PRESET_MS: Record<TimeRangePresetKey, number> = {
  "1h": 60 * 60 * 1000,
  "24h": 24 * 60 * 60 * 1000,
  "7d": 7 * 24 * 60 * 60 * 1000,
};

/**
 * The `since` instant a preset resolves to, anchored to `now`. `now`
 * defaults to the real clock and is a parameter purely so callers and tests
 * can pin it — the same seam `formatRelativeTime` (domain/run-board.ts)
 * uses for the same reason.
 */
export function presetSince(
  preset: TimeRangePresetKey,
  now: Date = new Date(),
): string {
  return new Date(now.getTime() - PRESET_MS[preset]).toISOString();
}
