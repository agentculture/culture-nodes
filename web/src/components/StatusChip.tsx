import {
  NODE_STATE_ICON,
  NODE_STATE_LABEL,
  type NodeExecState,
} from "../domain/run-state";

export interface StatusChipProps {
  state: NodeExecState;
  /** Optional stable id, for the one chip an agent asserts against. */
  id?: string;
  className?: string;
}

/**
 * A node/run state chip.
 *
 * PRD §8.8 requires "status labels and icons in addition to color", so every
 * chip renders a glyph *and* the word — the color is the third signal, never
 * the only one. The glyph is marked aria-hidden because the label beside it
 * already says the same thing to a screen reader.
 */
export function StatusChip({ state, id, className }: StatusChipProps) {
  return (
    <span
      id={id}
      className={`status-chip status-chip--${state}${className ? ` ${className}` : ""}`}
      data-state={state}
    >
      <span className="status-chip__icon" aria-hidden="true">
        {NODE_STATE_ICON[state]}
      </span>
      <span className="status-chip__label">{NODE_STATE_LABEL[state]}</span>
    </span>
  );
}

export default StatusChip;
