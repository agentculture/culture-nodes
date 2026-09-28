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
 *   - `expired` is never a button (issue #265). The compiler implies it in
 *     every approval node's allowed_outcomes, but it is the outcome the
 *     control plane records when it READS a fact — a merged PR, a passed
 *     deadline — and `DecideHumanTask` now refuses it from a decider
 *     (internal/engine, DecidableOutcomes). A button for it was an offer to
 *     hand-produce an engine observation.
 *   - Every button is disabled unless a decision could actually be
 *     recorded. Since task t9 that means: the signed-in principal is bound
 *     to an actor (`useWhoami` says `bound`) and, where the caller has to
 *     read it, the ledger version is known. Nothing is typed to enable them
 *     — the token panels and the free-text decider are gone; the decider is
 *     whoever Cloudflare Access verified, and the control plane stamps that
 *     on the record regardless of what the body names. A decision with no
 *     accountable decider is still not a decision (PRD §10.4) — it is just
 *     no longer the page's job to ask for the name.
 *   - The busy task's buttons are disabled while its POST is in flight, so
 *     a double click cannot become two decisions.
 */
/**
 * The outcome only the engine reaches, mirroring engine.OutcomeExpired. It is
 * filtered here rather than server-side because `allowed_outcomes` is the
 * verbatim record of what the task declared and must stay that way — the
 * expiry path validates against it.
 */
const ENGINE_ONLY_OUTCOME = "expired";

/**
 * A button reads as the verb a person performs. Only outcomes whose raw name
 * reads badly as a button are relabelled; the POSTed outcome is always the
 * raw name. `acknowledged` is the one answer a notice takes (task t45);
 * `retry` / `abandon` are what a blocked-agent task offers (task t46, for
 * t47's declarations). Any other outcome in `allowed_outcomes` still gets a
 * button, under its raw name — the set is the server's, never this map's.
 */
export const OUTCOME_LABELS: Readonly<Record<string, string>> = {
  acknowledged: "Acknowledge",
  retry: "Retry",
  abandon: "Abandon",
};

/**
 * One line on what choosing an outcome does, where this page knows it. An
 * outcome with no entry gets no hint rather than a guessed one: the edge it
 * routes is the workflow's, and only these three mean the same thing
 * wherever they appear.
 */
export const OUTCOME_HINTS: Readonly<Record<string, string>> = {
  acknowledged: "Record that you read this; nothing else moves.",
  retry: "Send the blocked step back to the agent to try again.",
  abandon: "Stop here; the blocked step is not tried again.",
};

export function OutcomeButtons({
  taskId,
  outcomes,
  disabled,
  busy,
  onChoose,
}: {
  taskId: string;
  outcomes: string[];
  /** True when no decision can be recorded at all (whoami not bound, or no ledger version yet). */
  disabled: boolean;
  /** True while THIS task's decision is in flight. */
  busy: boolean;
  onChoose: (outcome: string) => void;
}) {
  const decidable = outcomes.filter((outcome) => outcome !== ENGINE_ONLY_OUTCOME);
  if (outcomes.length === 0) {
    return <p className="muted">needs an outcome set</p>;
  }
  if (decidable.length === 0) {
    return <p className="muted">no outcome a person may select</p>;
  }
  return (
    <div className="inbox-card__outcomes" aria-label={`Outcomes for ${taskId}`}>
      {decidable.map((outcome) => {
        const hint = OUTCOME_HINTS[outcome];
        const hintId = `outcome-hint-${taskId}-${outcome}`;
        return (
          <span key={outcome} className="inbox-card__outcome-choice">
            <button
              type="button"
              className="author-workflow__button"
              disabled={disabled || busy}
              aria-describedby={hint ? hintId : undefined}
              onClick={() => onChoose(outcome)}
            >
              {OUTCOME_LABELS[outcome] ?? outcome}
            </button>
            {hint ? (
              <span id={hintId} className="muted inbox-card__outcome-hint">
                {hint}
              </span>
            ) : null}
          </span>
        );
      })}
    </div>
  );
}

export default OutcomeButtons;
