import { Link } from "react-router-dom";
import type { NodeRunListItem } from "../api/types";
import { nodeRunStateToExecState } from "../domain/run-state";
import StatusChip from "./StatusChip";

export interface JobsTableProps {
  items: NodeRunListItem[];
  id?: string;
  caption?: string;
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
export function JobsTable({ items, id, caption }: JobsTableProps) {
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
          <th scope="col">node</th>
          <th scope="col">actor / runner</th>
          <th scope="col">state</th>
          <th scope="col">outcome</th>
          <th scope="col">started</th>
          <th scope="col">updated</th>
        </tr>
      </thead>
      <tbody>
        {items.map((item) => (
          <tr
            key={item.id}
            data-node-run-id={item.id}
            data-run-id={item.run_id}
            data-node-run-state={item.state}
          >
            <th scope="row">
              <Link to={`/runs/${encodeURIComponent(item.run_id)}`}>
                {item.run_id}
              </Link>
            </th>
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
              <time dateTime={item.created_at}>{item.created_at}</time>
            </td>
            <td>
              <time dateTime={item.updated_at}>{item.updated_at}</time>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export default JobsTable;
