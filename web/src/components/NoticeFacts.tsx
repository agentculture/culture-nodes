import { Link } from "react-router-dom";
import type { HumanTask } from "../api/types";
import { noticeView } from "../domain/human-task-notice";

/**
 * The readable half of a notice card (task t45): what happened, and the
 * facts the control plane recorded about it — in place of the raw request
 * JSON, which is still behind the card's audit disclosure.
 */
export function NoticeFacts({ task }: { task: HumanTask }) {
  const view = noticeView(task);
  return (
    <>
      <h3 className="inbox-card__question">{view.title}</h3>
      <p className="muted inbox-card__notice-hint">
        A notice — it asks nothing. Acknowledging records that you read it;
        the run it names is not resumed or re-minted.
      </p>
      <dl className="inbox-card__request inbox-card__facts inbox-card__notice">
        {view.reason ? (
          <div>
            <dt>reason</dt>
            <dd>{view.reason}</dd>
          </div>
        ) : null}
        {view.facts.map((fact) => (
          <div key={fact.label}>
            <dt>{fact.label}</dt>
            <dd>
              {fact.runId ? (
                <Link to={`/runs/${fact.runId}`}>{fact.value}</Link>
              ) : (
                fact.value
              )}
            </dd>
          </div>
        ))}
      </dl>
    </>
  );
}

export default NoticeFacts;
