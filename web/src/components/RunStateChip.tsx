import { NODE_STATE_ICON } from "../domain/run-state";
import type { RunState } from "../api/types";

/**
 * `Run.state` glyphs, borrowed straight from `NODE_STATE_ICON` (run-state.ts)
 * rather than a second icon set: `waiting`, `completed`, `failed` and
 * `cancelled` are literally the same words in both vocabularies, and
 * `created`/`running` read onto the closest node-execution concept
 * (`ready`/`active`) the icon set already has a glyph for. No new glyph is
 * invented here.
 */
const RUN_STATE_ICON: Record<RunState, string> = {
  created: NODE_STATE_ICON.ready,
  running: NODE_STATE_ICON.active,
  waiting: NODE_STATE_ICON.waiting,
  completed: NODE_STATE_ICON.completed,
  failed: NODE_STATE_ICON.failed,
  cancelled: NODE_STATE_ICON.cancelled,
};

export interface RunStateChipProps {
  state: RunState;
  className?: string;
}

/**
 * A `Run.state` chip — the run-level sibling of `StatusChip` (which is keyed
 * to `NodeExecState`, a different, node-scoped vocabulary). Same markup, same
 * `status-chip`/`status-chip__icon`/`status-chip__label` classes, same "icon
 * + word, never colour alone" rule (PRD §8.8); styles/app.css extends the
 * existing `status-chip--*` colour rules to cover `running`/`created` as
 * additional selectors on the `active`/`ready` rules, so no new colour is
 * declared. The label is always the literal RunState word — the board must
 * read the vocabulary the API actually reports (openapi.yaml's RunState
 * enum), never a paraphrase.
 */
export function RunStateChip({ state, className }: RunStateChipProps) {
  return (
    <span
      className={`status-chip status-chip--${state}${className ? ` ${className}` : ""}`}
      data-run-state={state}
    >
      <span className="status-chip__icon" aria-hidden="true">
        {RUN_STATE_ICON[state]}
      </span>
      <span className="status-chip__label">{state}</span>
    </span>
  );
}

export default RunStateChip;
