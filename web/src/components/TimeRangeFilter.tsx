import { useEffect, useState, type FormEvent } from "react";
import {
  PRESET_LABEL,
  presetSince,
  TIME_RANGE_PRESETS,
  type TimeRangePresetKey,
} from "../domain/time-range";

export interface TimeRangeValue {
  /** RFC3339, or undefined for no lower bound. */
  since?: string;
  /** RFC3339, or undefined for no upper bound. */
  until?: string;
}

export interface TimeRangeFilterProps {
  since?: string;
  until?: string;
  /** Called with the new range whenever a preset, Clear, or Apply fires. */
  onApply: (range: TimeRangeValue) => void;
}

/**
 * `<input type="datetime-local">` wants local wall-clock time with no
 * offset/seconds (`YYYY-MM-DDTHH:mm`); the API deals in RFC3339 instants.
 * These two functions are the only place that conversion happens.
 */
function toLocalInputValue(iso?: string): string {
  if (!iso) return "";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

function fromLocalInputValue(value: string): string | undefined {
  if (!value) return undefined;
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return undefined;
  return date.toISOString();
}

/**
 * The jobs timeline's time-range control (task t15): three fixed lookback
 * presets, a "Custom" since/until pair, and a "Clear range" escape hatch —
 * every one of them calls back with a concrete `{ since, until }`, never a
 * client-side filter. `JobsTimeline` is the one place that range actually
 * reaches the API (as `updated_since`/`updated_until`) and the URL (as
 * `since`/`until` search params); this component only proposes a range.
 *
 * Keyboard access is native throughout: the presets and Custom toggle are
 * plain `<button>`s (Tab order + Enter/Space activation for free), and the
 * custom range is a real `<form>` so Enter inside either input submits it
 * without a mouse.
 */
export function TimeRangeFilter({
  since,
  until,
  onApply,
}: TimeRangeFilterProps) {
  const [activePreset, setActivePreset] = useState<TimeRangePresetKey | null>(
    null,
  );
  const [customOpen, setCustomOpen] = useState(false);
  const [sinceInput, setSinceInput] = useState(toLocalInputValue(since));
  const [untilInput, setUntilInput] = useState(toLocalInputValue(until));

  // The parent is the source of truth (it mirrors the URL); keep the custom
  // inputs in step whenever the applied range changes out from under us —
  // e.g. a bookmarked /jobs?since=...&until=... URL loading fresh.
  useEffect(() => {
    setSinceInput(toLocalInputValue(since));
  }, [since]);
  useEffect(() => {
    setUntilInput(toLocalInputValue(until));
  }, [until]);

  const hasRange = Boolean(since || until);

  const selectPreset = (preset: TimeRangePresetKey) => {
    setActivePreset(preset);
    setCustomOpen(false);
    onApply({ since: presetSince(preset), until: undefined });
  };

  const openCustom = () => {
    setActivePreset(null);
    setCustomOpen((open) => !open);
  };

  const applyCustom = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setActivePreset(null);
    onApply({
      since: fromLocalInputValue(sinceInput),
      until: fromLocalInputValue(untilInput),
    });
  };

  const clearRange = () => {
    setActivePreset(null);
    setCustomOpen(false);
    setSinceInput("");
    setUntilInput("");
    onApply({ since: undefined, until: undefined });
  };

  return (
    <div className="time-range-filter" id="time-range-filter">
      <div
        className="time-range-filter__presets"
        role="group"
        aria-label="Time range"
      >
        {TIME_RANGE_PRESETS.map((preset) => (
          <button
            key={preset}
            type="button"
            id={`time-range-preset-${preset}`}
            aria-pressed={activePreset === preset}
            onClick={() => selectPreset(preset)}
          >
            {PRESET_LABEL[preset]}
          </button>
        ))}
        <button
          type="button"
          id="time-range-preset-custom"
          aria-pressed={customOpen}
          aria-expanded={customOpen}
          onClick={openCustom}
        >
          Custom
        </button>
      </div>

      {hasRange ? (
        <button
          type="button"
          id="time-range-clear"
          className="link-button time-range-filter__clear"
          onClick={clearRange}
        >
          Clear range
        </button>
      ) : null}

      {customOpen ? (
        <form
          className="time-range-filter__custom"
          aria-label="Custom time range"
          onSubmit={applyCustom}
        >
          <label htmlFor="time-range-since">Since</label>
          <input
            id="time-range-since"
            type="datetime-local"
            value={sinceInput}
            onChange={(event) => setSinceInput(event.target.value)}
          />
          <label htmlFor="time-range-until">Until</label>
          <input
            id="time-range-until"
            type="datetime-local"
            value={untilInput}
            onChange={(event) => setUntilInput(event.target.value)}
          />
          <button type="submit" id="time-range-apply">
            Apply
          </button>
        </form>
      ) : null}
    </div>
  );
}

export default TimeRangeFilter;
