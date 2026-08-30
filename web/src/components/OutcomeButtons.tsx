/**
 * One button per outcome the engine will accept for a pending human task —
 * the shape t10 built inside the Decisions view, extracted here in task t18
 * when the ticket page needed the same affordance (spec c6/c10).
 *
 * The extraction is not cosmetic. The whole point of `allowed_outcomes` is
 * that a surface offers exactly what `DecideHumanTask` accepts and nothing
 * else; two independent renderings of that rule are two chances for one of
 * them to drift into offering an outcome that 400s, or into hiding one that
 * would have worked. There is now one.
 *
 * Three properties it refuses to fudge, all inherited from t10:
 *
 *   - A task that declares NO outcomes renders a stated absence, never an
 *     empty row of buttons. `schedule_failing` is an alert, not a choice,
 *     and a page that shows it with nothing to click should say why.
 *   - Every button is disabled unless a decision could actually be
 *     recorded — a token is held and a decider is named. A decision with no
 *     named decider is not a decision (PRD §10.4).
 *   - The busy task's buttons are disabled while its POST is in flight, so
 *     a double click cannot become two decisions.
 */
export function OutcomeButtons({
  taskId,
  outcomes,
  disabled,
  busy,
  onChoose,
}: {
  taskId: string;
  outcomes: string[];
  /** True when no decision can be recorded at all (no token, no decider, no version). */
  disabled: boolean;
  /** True while THIS task's decision is in flight. */
  busy: boolean;
  onChoose: (outcome: string) => void;
}) {
  if (outcomes.length === 0) {
    return <p className="muted">needs an outcome set</p>;
  }
  return (
    <div className="inbox-card__outcomes" aria-label={`Outcomes for ${taskId}`}>
      {outcomes.map((outcome) => (
        <button
          key={outcome}
          type="button"
          className="author-workflow__button"
          disabled={disabled || busy}
          onClick={() => onChoose(outcome)}
        >
          {outcome}
        </button>
      ))}
    </div>
  );
}

export default OutcomeButtons;
