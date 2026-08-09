import type { LedgerRecord } from "../api/types";
import AuthorityChip from "./AuthorityChip";

export interface LedgerTableProps {
  records: LedgerRecord[];
  id?: string;
  caption?: string;
}

function shortDigest(digest: string): string {
  return digest.length > 20 ? `${digest.slice(0, 20)}…` : digest;
}

/**
 * The ledger view's record table (PRD §8.6 Ledger view): type, authority,
 * origin, time — plus the content digest, because a record's identity in
 * this system *is* its digest.
 */
export function LedgerTable({ records, id, caption }: LedgerTableProps) {
  if (records.length === 0) {
    return (
      <p className="muted" id={id ? `${id}-empty` : undefined}>
        No ledger records.
      </p>
    );
  }

  return (
    <table className="ledger-table" id={id}>
      {caption ? <caption>{caption}</caption> : null}
      <thead>
        <tr>
          <th scope="col">record type</th>
          <th scope="col">authority</th>
          <th scope="col">origin</th>
          <th scope="col">time</th>
          <th scope="col">digest</th>
        </tr>
      </thead>
      <tbody>
        {records.map((record) => (
          <tr key={record.id} data-record-id={record.id}>
            <th scope="row" className="ledger-table__type">
              {record.record_type}
            </th>
            <td>
              <AuthorityChip authority={record.authority} />
            </td>
            <td className="ledger-table__origin">
              <span className="ledger-table__origin-kind">
                {record.origin.kind}
              </span>{" "}
              <code>{record.origin.actor_id}</code>
            </td>
            <td>
              <time dateTime={record.created_at}>{record.created_at}</time>
            </td>
            <td>
              <code title={record.content_digest}>
                {shortDigest(record.content_digest)}
              </code>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export default LedgerTable;
