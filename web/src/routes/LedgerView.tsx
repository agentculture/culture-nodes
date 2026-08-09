import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import {
  ApiError,
  PROJECTION_NAMES,
  getLedger,
  getProjection,
} from "../api/client";
import type { LedgerRecord, Projection } from "../api/types";
import { setAgentState } from "../agent-state/store";
import ErrorNotice from "../components/ErrorNotice";
import LedgerTable from "../components/LedgerTable";

/**
 * The Ledger view (PRD §8.6): the run's raw record feed, plus any of the
 * §10.9 standard projections computed over it.
 *
 * Authority is rendered with the same dashed/solid vocabulary the canvas
 * uses for edges — see components/AuthorityChip.tsx and
 * culture-design/edges.ts. A `proposed` record is an agent's own claim that
 * nobody has confirmed; the chip says so before its text is read.
 */
export function LedgerView() {
  const { id: runId } = useParams<{ id: string }>();
  const [records, setRecords] = useState<LedgerRecord[] | null>(null);
  const [ledgerVersion, setLedgerVersion] = useState<number | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [projectionName, setProjectionName] = useState<string>("");
  const [projection, setProjection] = useState<Projection | null>(null);
  const [projectionError, setProjectionError] = useState<ApiError | null>(null);

  useEffect(() => {
    if (!runId) return;
    const controller = new AbortController();
    setAgentState({ status: "loading" });
    getLedger(runId, controller.signal)
      .then((payload) => {
        if (controller.signal.aborted) return;
        setRecords(payload.items);
        setLedgerVersion(payload.ledger_version);
        setAgentState({ status: "ready" });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setRecords([]);
        setError(
          cause instanceof ApiError
            ? cause
            : new ApiError(0, String(cause), "check the browser console"),
        );
        setAgentState({ status: "ready" });
      });
    return () => controller.abort();
  }, [runId]);

  useEffect(() => {
    if (!runId || !projectionName) {
      setProjection(null);
      setProjectionError(null);
      return;
    }
    const controller = new AbortController();
    getProjection(runId, projectionName, controller.signal)
      .then((payload) => {
        if (controller.signal.aborted) return;
        setProjection(payload);
        setProjectionError(null);
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setProjection(null);
        setProjectionError(
          cause instanceof ApiError
            ? cause
            : new ApiError(0, String(cause), "check the browser console"),
        );
      });
    return () => controller.abort();
  }, [runId, projectionName]);

  return (
    <section className="container ledger-view">
      <h1>Ledger</h1>
      <p className="muted">
        Run <code>{runId}</code>
        {ledgerVersion !== null ? (
          <>
            {" "}
            · ledger version <code id="ledger-version">{ledgerVersion}</code>
          </>
        ) : null}{" "}
        · <Link to={`/runs/${runId}`}>Back to the run</Link>
      </p>

      {error ? <ErrorNotice error={error} /> : null}

      <div className="ledger-view__projection">
        <label htmlFor="projection-select">Projection</label>
        <select
          id="projection-select"
          value={projectionName}
          onChange={(event) => setProjectionName(event.target.value)}
        >
          <option value="">— raw record feed —</option>
          {PROJECTION_NAMES.map((name) => (
            <option key={name} value={name}>
              {name}
            </option>
          ))}
        </select>
      </div>

      {projectionError ? <ErrorNotice error={projectionError} /> : null}

      {projection ? (
        <>
          <p className="muted">
            Projection <code>{projection.kind}</code> over{" "}
            <code>{projection.subject}</code> · digest{" "}
            <code id="projection-digest">
              {projection.digest.slice(0, 24)}…
            </code>
          </p>
          <LedgerTable
            id="projection-table"
            records={projection.items}
            caption={`${projection.kind} — ${projection.items.length} record(s)`}
          />
        </>
      ) : (
        <LedgerTable
          id="ledger-table"
          records={records ?? []}
          caption={
            records === null
              ? "Loading…"
              : `${records.length} record(s), append order`
          }
        />
      )}
    </section>
  );
}

export default LedgerView;
