import { Link } from "react-router-dom";
import type { NodeRunListItem, Run } from "../api/types";
import { nodeRunStateToExecState } from "../domain/run-state";
import { runDisplayName } from "../domain/usage";
import CategoryChip from "./CategoryChip";
import StatusChip from "./StatusChip";
import UsageSummary from "./UsageSummary";

export interface JobsTableProps {
  items: NodeRunListItem[];
  id?: string;
  caption?: string;
  /**
   * Run name/category lookup, keyed by `run_id` (task t5). `GET
   * /v1alpha1/node-runs` carries no run metadata of its own — a node run
   * row only has `run_id` — so a name/category next to the run link is
   * only ever shown when the caller (JobsTimeline) has separately fetched
   * `GET /v1alpha1/runs` for the same window and passes the result here.
   * Absent or missing entries fall back to the bare run id, same as today.
   */
  runsById?: Record<string, Pick<Run, "id" | "name" | "display_hint" | "category">>;
}

/**
 * The jobs timeline's table (PRD "timeline of jobs" — task t15): one row
 * per node run, cross-run, newest first by `updated_at` — the same
 * `ledger-table` styling LedgerTable.tsx/RunsList.tsx already use, so this
 * is a sibling of those tables rather than a new aesthetic.
 *
 * `state` renders through the same `StatusChip` every other view uses,
 * mapped from the raw `NodeRunState` the API sends
 * (`nodeRunStateToExecState`, domain/run-state.ts) — never a new chip
 * vocabulary invented for this one table.
 */
export function JobsTable({ items, id, caption, runsById }: JobsTableProps) {
  if (items.length === 0) {
    return (
      <p className="muted" id={id ? `${id}-empty` : undefined}>
        No node runs in this range.
      </p>
    );
  }

  return (
    <table className="ledger-table" id={id}>
      {caption ? <caption>{caption}</caption> : null}
      <thead>
        <tr>
          <th scope="col">run</th>
          <th scope="col">category</th>
          <th scope="col">node</th>
          <th scope="col">actor / runner</th>
          <th scope="col">state</th>
          <th scope="col">outcome</th>
          <th scope="col">usage</th>
          <th scope="col">started</th>
          <th scope="col">updated</th>
        </tr>
      </thead>
      <tbody>
        {items.map((item) => {
          const run = runsById?.[item.run_id];
          const display = run ? runDisplayName(run) : null;
          return (
            <tr
              key={item.id}
              data-node-run-id={item.id}
              data-run-id={item.run_id}
              data-node-run-state={item.state}
            >
              <th scope="row">
                <Link to={`/runs/${encodeURIComponent(item.run_id)}`}>
                  {display && display.text !== item.run_id ? (
                    <span
                      className={`run-name${display.derived ? " run-name--derived" : ""}`}
                      data-derived={display.derived ? "true" : "false"}
                      title={
                        display.derived
                          ? `derived guess, not a given name: "${display.text}"`
                          : display.text
                      }
                    >
                      {display.text}
                    </span>
                  ) : (
                    item.run_id
                  )}
                </Link>
              </th>
              <td>
                {run?.category ? (
                  <CategoryChip category={run.category} />
                ) : (
                  <span className="muted">—</span>
                )}
              </td>
              <td>
                <code>{item.node_id}</code>
              </td>
              <td>
                {item.actor_id ? (
                  <code>{item.actor_id}</code>
                ) : (
                  <span className="muted">—</span>
                )}
              </td>
              <td>
                <StatusChip state={nodeRunStateToExecState(item.state)} />
              </td>
              <td>{item.outcome ?? <span className="muted">—</span>}</td>
              <td>
                <UsageSummary usage={item.usage} compact />
              </td>
              <td>
                <time dateTime={item.created_at}>{item.created_at}</time>
              </td>
              <td>
                <time dateTime={item.updated_at}>{item.updated_at}</time>
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

export default JobsTable;
