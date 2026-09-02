import type { TicketFlow } from "../domain/ticket-flow";

/**
 * The ticket's place in the loop, drawn (task t17, spec c25, issue #270).
 *
 * WHY A HAND-DRAWN SVG AND NOT THE XYFLOW/ELK CANVAS. The graph stack in
 * `ActiveGraphCanvas`/`WorkflowNode` is right for a workflow whose shape is
 * unknown until it is read: it lays out with ELK *asynchronously*, mounts a
 * pannable, zoomable `role="application"` surface with its own keyboard
 * model, and refits when the layout lands. Every one of those properties is
 * wrong here. This diagram has five stops in a fixed order, it is the first
 * thing on the first screen, and an async layout means the very first paint
 * a person sees is the fallback one and then a jump. A five-stop rail is
 * ~40 lines of deterministic SVG that paints correctly on the first frame,
 * scales with the column, needs no focus trap, and costs no layout worker.
 * The canvas primitives stay where they belong — the graph pages.
 *
 * What it refuses to fudge:
 *
 *   - Reached and un-reached stops differ in SHAPE (filled disc vs dashed
 *     ring), not only in colour, and the rail segment between two stops is
 *     dashed unless both ends are evidenced.
 *   - Every stop's state is also available as text: the current stage and
 *     its evidence are visible under the rail, and the full stage-by-stage
 *     reading is in a visually-hidden list for a screen reader, because a
 *     picture is the only thing here that could become status-by-colour.
 *   - The breathing halo on the current stop is CSS, and app.css turns it
 *     off under `prefers-reduced-motion` — the ring stays, it stops moving.
 */

const STOP_X = [64, 192, 320, 448, 576];
const RAIL_Y = 46;
const LABEL_Y = 86;

export function TicketFlowRail({
  flow,
  ticketId,
}: {
  flow: TicketFlow;
  ticketId: string;
}) {
  const titleId = "ticket-flow-title";
  const descId = "ticket-flow-desc";
  const currentStage = flow.stages.find((stage) => stage.current);

  return (
    <figure className="ticket-flow" data-testid="ticket-flow-rail">
      <svg
        className="ticket-flow__rail"
        viewBox="0 0 640 104"
        preserveAspectRatio="xMidYMid meet"
        role="img"
        aria-labelledby={`${titleId} ${descId}`}
      >
        <title id={titleId}>
          {`Where ${ticketId} is in the loop: ${currentStage?.label ?? "Intake"}`}
        </title>
        <desc id={descId}>{flow.waitingLine}</desc>

        {flow.stages.slice(0, -1).map((stage, index) => {
          const next = flow.stages[index + 1];
          const walked = stage.reached && next.reached;
          return (
            <line
              key={`${stage.id}-${next.id}`}
              className={`ticket-flow__link${walked ? " is-walked" : ""}`}
              data-from={stage.id}
              data-to={next.id}
              x1={STOP_X[index] + 22}
              y1={RAIL_Y}
              x2={STOP_X[index + 1] - 22}
              y2={RAIL_Y}
              strokeDasharray={walked ? undefined : "5 6"}
            />
          );
        })}

        {flow.stages.map((stage, index) => (
          <g
            key={stage.id}
            className={[
              "ticket-flow__stop",
              stage.reached ? "is-reached" : "is-unevidenced",
              stage.current ? "is-current" : "",
            ]
              .filter(Boolean)
              .join(" ")}
            data-stage={stage.id}
            data-reached={stage.reached ? "true" : "false"}
            data-current={stage.current ? "true" : "false"}
          >
            {stage.current ? (
              <circle
                className="ticket-flow__halo"
                cx={STOP_X[index]}
                cy={RAIL_Y}
                r={18}
              />
            ) : null}
            <circle
              className="ticket-flow__mark"
              cx={STOP_X[index]}
              cy={RAIL_Y}
              r={stage.current ? 11 : 8}
              strokeDasharray={stage.reached ? undefined : "3 4"}
            />
            <text
              className="ticket-flow__label"
              x={STOP_X[index]}
              y={LABEL_Y}
              textAnchor="middle"
            >
              {stage.label}
            </text>
          </g>
        ))}
      </svg>

      <figcaption className="ticket-flow__caption">
        <p className="ticket-flow__now">
          <span className="ticket-flow__now-label">Now</span>
          <strong>{currentStage?.label ?? "Intake"}</strong>
          <span className="ticket-flow__waiting" data-waiting-on={flow.waitingOn}>
            {flow.waitingLine}
          </span>
        </p>
        <p className="ticket-flow__evidence muted">
          Read from this ticket alone: {currentStage?.evidence}.
        </p>
        {/* The picture in words. Each stop says what proved it, or that
            nothing did — the same sentences the rail is drawn from. */}
        <ul className="sr-only">
          {flow.stages.map((stage) => (
            <li key={stage.id}>
              {stage.label}: {stage.reached ? "reached" : "not evidenced"}
              {stage.current ? ", the current stage" : ""} — {stage.evidence}.
            </li>
          ))}
        </ul>
      </figcaption>
    </figure>
  );
}

export default TicketFlowRail;
