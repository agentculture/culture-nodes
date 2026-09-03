import { useSearchParams } from "react-router-dom";
import JobsTimeline from "./JobsTimeline";
import RunsBoard from "./RunsBoard";
import RunsList from "./RunsList";

/**
 * The three projections of one dataset, and their URL names (task t9).
 *
 * List, board and jobs were three nav destinations reading the same runs —
 * three tabs for one question ("what has the engine been doing"), which is
 * three of the twelve destinations the PRD §8.6 spine consolidates. They are
 * one page now, and which projection is on screen rides the URL as
 * `?view=list|board|jobs` so it stays bookmarkable, shareable and reachable
 * by the redirects `/board` and `/jobs` still answer.
 *
 * Jobs is a projection of node runs rather than runs, which is the same
 * question one level down — every node run belongs to a run in this list.
 */
export const RUNS_VIEWS = [
  { key: "list", label: "List" },
  { key: "board", label: "Board" },
  { key: "jobs", label: "Jobs" },
] as const;

export type RunsView = (typeof RUNS_VIEWS)[number]["key"];

/** The projection an unqualified `/runs` shows — the table it always was. */
export const DEFAULT_RUNS_VIEW: RunsView = "list";

/**
 * `?view=` → a projection. An unknown or absent value is the default rather
 * than an error state: a URL is something people edit and truncate, and the
 * run table is a better answer than a diagnostic.
 */
export function parseRunsView(raw: string | null): RunsView {
  return RUNS_VIEWS.some((view) => view.key === raw)
    ? (raw as RunsView)
    : DEFAULT_RUNS_VIEW;
}

/**
 * The Runs page: one heading, one projection toggle, one of three bodies.
 *
 * Switching projection preserves every other search param, because the
 * time-range filter each projection renders writes `since`/`until` there
 * (issue #23) — changing the lens must not silently widen the range you were
 * looking through. The default projection is written as the *absence* of
 * `view`, so `/runs` and `/runs?view=list` are the same URL rather than two.
 */
export function Runs() {
  const [searchParams, setSearchParams] = useSearchParams();
  const view = parseRunsView(searchParams.get("view"));

  const setView = (next: RunsView) => {
    setSearchParams((prev) => {
      const params = new URLSearchParams(prev);
      if (next === DEFAULT_RUNS_VIEW) params.delete("view");
      else params.set("view", next);
      return params;
    });
  };

  return (
    <section className="view-rail runs-page">
      <h1>Runs</h1>

      <div
        id="runs-toggle"
        className="view-toggle"
        role="group"
        aria-label="Runs projection"
      >
        {RUNS_VIEWS.map(({ key, label }) => (
          <button
            key={key}
            type="button"
            id={`runs-toggle-${key}`}
            aria-pressed={view === key}
            onClick={() => setView(key)}
          >
            {label}
          </button>
        ))}
      </div>

      {view === "list" ? <RunsList /> : null}
      {view === "board" ? <RunsBoard /> : null}
      {view === "jobs" ? <JobsTimeline /> : null}
    </section>
  );
}

export default Runs;
