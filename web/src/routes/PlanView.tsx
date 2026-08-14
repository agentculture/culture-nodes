import { useEffect, useState, type FormEvent } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { setAgentState } from "../agent-state/store";
import { ApiError, getPlanImport, listPlanImports } from "../api/client";
import type { PlanImport, PlanImportTask } from "../api/types";
import AuthorityChip from "../components/AuthorityChip";
import ErrorNotice from "../components/ErrorNotice";
import {
  ORIGIN_LABEL,
  authorityForDeviationStatus,
  authorityForOrigin,
  authorityForTaskStatus,
  groupTasksByWave,
} from "../domain/plan";

function toApiError(cause: unknown): ApiError {
  return cause instanceof ApiError
    ? cause
    : new ApiError(0, String(cause), "check the browser console");
}

/** One task row: its status chip, summary, and its REAL dependency edges. */
function TaskRow({ task }: { task: PlanImportTask }) {
  return (
    <li className="plan-task" data-task-ref={task.task_ref}>
      <div className="plan-task__head">
        <code className="plan-task__ref">{task.task_ref}</code>
        <AuthorityChip authority={authorityForTaskStatus(task.source_status)} />
        <span className="plan-task__origin muted">{task.origin_kind}</span>
      </div>
      <p className="plan-task__summary">{task.summary}</p>
      {task.depends_on.length > 0 ? (
        <p className="plan-task__deps muted">
          depends on{" "}
          {task.depends_on.map((ref, i) => (
            <span key={ref}>
              {i > 0 ? ", " : ""}
              <code>{ref}</code>
            </span>
          ))}
        </p>
      ) : (
        <p className="plan-task__deps muted">no prerequisites</p>
      )}
    </li>
  );
}

/**
 * The Plan view (task t23, issue #45): plan, waves, and task status over
 * `GET /v1alpha1/plan-imports` — the durable projection task t22 built,
 * read here for the first time by any surface (internal/devague had zero
 * production callers before t22/t23).
 *
 * Waves and per-task status/dependency edges come straight off the plan
 * import snapshot's REAL per-task fields (spec c15: never the lossy
 * `plan waves` reading). Deviations render with their origin (`user` vs
 * `llm` — "the user reports" vs "the system knows") visibly distinguished
 * using the existing `AuthorityChip` vocabulary (`domain/plan.ts`'s
 * `authorityForOrigin`) — the entire point of this task (h11).
 *
 * `slug` is the URL param (`/plan/:slug`); `/plan` alone renders a slug
 * entry field, mirroring LedgerView's projection-picker discipline for
 * optional state. There is no "every imported plan" listing endpoint (by
 * design — openapi.yaml's listPlanImports), so a slug must be named, either
 * by URL or by typing it in.
 */
export function PlanView() {
  const { slug } = useParams<{ slug?: string }>();
  const navigate = useNavigate();
  const [slugInput, setSlugInput] = useState(slug ?? "");
  const [snapshotCount, setSnapshotCount] = useState<number | null>(null);
  const [plan, setPlan] = useState<PlanImport | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [notFound, setNotFound] = useState(false);

  useEffect(() => {
    setSlugInput(slug ?? "");
  }, [slug]);

  useEffect(() => {
    if (!slug) {
      setPlan(null);
      setSnapshotCount(null);
      setNotFound(false);
      return;
    }
    const controller = new AbortController();
    setAgentState({ status: "loading" });
    setError(null);
    setPlan(null);
    setSnapshotCount(null);
    setNotFound(false);

    listPlanImports(slug, controller.signal)
      .then((list) => {
        if (controller.signal.aborted) return;
        setSnapshotCount(list.items.length);
        if (list.items.length === 0) {
          setNotFound(true);
          setAgentState({ status: "ready" });
          return null;
        }
        // Most recent first (openapi.yaml's listPlanImports contract) —
        // items[0] is "the current one".
        return getPlanImport(list.items[0].id, controller.signal);
      })
      .then((detail) => {
        if (controller.signal.aborted || !detail) return;
        setPlan(detail);
        setAgentState({ status: "ready" });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setError(toApiError(cause));
        setAgentState({ status: "ready" });
      });
    return () => controller.abort();
  }, [slug]);

  const submitSlug = (event: FormEvent) => {
    event.preventDefault();
    const trimmed = slugInput.trim();
    if (trimmed) navigate(`/plan/${encodeURIComponent(trimmed)}`);
  };

  const { waves, unscheduled } = plan
    ? groupTasksByWave(plan.tasks)
    : { waves: [], unscheduled: [] };

  return (
    <section className="view-rail plan-view">
      <h1>Plan</h1>
      <p className="muted">
        A devague plan's real per-task status and dependency edges, plus its
        deviations — origin distinguished, using the same chip vocabulary the
        Ledger view does.
      </p>

      <form className="plan-view__slug-form" onSubmit={submitSlug}>
        <label htmlFor="plan-slug-input">Plan slug</label>
        <input
          id="plan-slug-input"
          type="text"
          value={slugInput}
          onChange={(event) => setSlugInput(event.target.value)}
          placeholder="economy-discord-graphs"
        />
        <button type="submit">Go</button>
      </form>

      {error ? <ErrorNotice error={error} /> : null}

      {!slug ? (
        <p className="muted" id="plan-view-empty">
          Enter a plan slug above to view its imported state.
        </p>
      ) : notFound ? (
        <p className="muted" id="plan-view-not-found">
          No plan named <code>{slug}</code> has been imported yet. Import one
          with <code>nodes plan-import --plan &lt;plan-show.json&gt;</code>.
        </p>
      ) : plan === null ? (
        <p className="muted" id="plan-view-loading">
          Loading plan…
        </p>
      ) : (
        <div id="plan-view-content">
          <div className="plan-view__head">
            <div>
              <h2>{plan.title || plan.slug}</h2>
              <p className="muted">
                <code>{plan.slug}</code> · source status{" "}
                <span className="status-chip" data-plan-source-status={plan.source_status}>
                  {plan.source_status}
                </span>{" "}
                · imported{" "}
                <time dateTime={plan.imported_at}>{plan.imported_at}</time>
                {snapshotCount !== null && snapshotCount > 1 ? (
                  <>
                    {" "}
                    ·{" "}
                    <span id="plan-snapshot-count">
                      {snapshotCount} snapshots on record, showing the most recent
                    </span>
                  </>
                ) : null}
              </p>
            </div>
          </div>

          <h3>Waves</h3>
          {waves.length === 0 && unscheduled.length === 0 ? (
            <p className="muted">No tasks in this plan.</p>
          ) : (
            <div className="plan-waves" id="plan-waves">
              {waves.map((wave) => (
                <section
                  key={wave.wave}
                  className="workflow-card plan-wave"
                  data-wave={wave.wave}
                >
                  <h4>Wave {wave.wave}</h4>
                  <ul className="plan-task-list">
                    {wave.tasks.map((task) => (
                      <TaskRow key={task.task_ref} task={task} />
                    ))}
                  </ul>
                </section>
              ))}
              {unscheduled.length > 0 ? (
                <section
                  className="workflow-card plan-wave plan-wave--unscheduled"
                  id="plan-unscheduled"
                >
                  <h4>Not scheduled (rejected)</h4>
                  <ul className="plan-task-list">
                    {unscheduled.map((task) => (
                      <TaskRow key={task.task_ref} task={task} />
                    ))}
                  </ul>
                </section>
              ) : null}
            </div>
          )}

          <h3>Deviations</h3>
          {plan.deviations.length === 0 ? (
            <p className="muted" id="plan-deviations-empty">
              No deviations recorded for this plan.
            </p>
          ) : (
            <div className="table-scroll">
              <table className="ledger-table plan-deviations-table" id="plan-deviations-table">
                <caption>{plan.deviations.length} deviation(s)</caption>
                <thead>
                  <tr>
                    <th scope="col">id</th>
                    <th scope="col">what</th>
                    <th scope="col">task</th>
                    <th scope="col">origin</th>
                    <th scope="col">status</th>
                  </tr>
                </thead>
                <tbody>
                  {plan.deviations.map((deviation) => (
                    <tr
                      key={deviation.deviation_ref}
                      data-deviation-ref={deviation.deviation_ref}
                      data-origin={deviation.origin_kind}
                    >
                      <th scope="row">
                        <code>{deviation.deviation_ref}</code>
                      </th>
                      <td>
                        <p className="plan-deviation__what">{deviation.what}</p>
                        <p className="plan-deviation__reason muted">
                          {deviation.reason}
                        </p>
                      </td>
                      <td>
                        <code>{deviation.task_ref}</code>
                      </td>
                      <td>
                        <span className="plan-origin">
                          <AuthorityChip
                            authority={authorityForOrigin(deviation.origin_kind)}
                          />
                          <span className="plan-origin__label muted">
                            {ORIGIN_LABEL[deviation.origin_kind]}
                          </span>
                        </span>
                      </td>
                      <td>
                        <AuthorityChip
                          authority={authorityForDeviationStatus(
                            deviation.source_status,
                          )}
                        />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}
    </section>
  );
}

export default PlanView;
