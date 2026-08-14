import { useCallback, useEffect, useRef, useState } from "react";
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
import { useSharedEvents, type SharedEventType } from "../hooks/useSharedEvents";

/**
 * The events that can append to (or land a review over) this run's ledger —
 * a stable module-level reference, as useSharedEvents requires (issue #46).
 * The shared stream is cross-run, so the subscriber below still filters
 * each event down to this view's own `runId` before scheduling a reload.
 */
const LEDGER_EVENT_TYPES = [
  "dev.culture.nodes.ledger.record-appended",
  "dev.culture.nodes.ledger.review-committed",
] as const satisfies readonly SharedEventType[];

/** Mirrors the Mesh view's attribution-refresh discipline (Mesh.tsx). */
const REFRESH_DEBOUNCE_MS = 4000;

/**
 * The Ledger view (PRD §8.6): the run's raw record feed, plus any of the
 * §10.9 standard projections computed over it.
 *
 * Authority is rendered with the same dashed/solid vocabulary the canvas
 * uses for edges — see components/AuthorityChip.tsx and
 * culture-design/edges.ts. A `proposed` record is an agent's own claim that
 * nobody has confirmed; the chip says so before its text is read.
 *
 * Auto-refresh (issue #46, task t30): the shared cross-run stream is the
 * only event source in the app — this view narrows it to its own run by
 * checking each event's `run_id` before scheduling a debounced reload of
 * the ledger (and the active projection, if one is selected). Never nulls
 * `records`/`projection`, never regresses agent-state to "loading".
 */
export function LedgerView() {
  const { id: runId } = useParams<{ id: string }>();
  const [records, setRecords] = useState<LedgerRecord[] | null>(null);
  const [ledgerVersion, setLedgerVersion] = useState<number | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [projectionName, setProjectionName] = useState<string>("");
  const [projection, setProjection] = useState<Projection | null>(null);
  const [projectionError, setProjectionError] = useState<ApiError | null>(null);
  const [reloadKey, setReloadKey] = useState(0);
  const reloadTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const lastReload = useRef(0);

  const scheduleReload = useCallback(() => {
    if (reloadTimer.current) return;
    const elapsed = Date.now() - lastReload.current;
    const wait = Math.max(0, REFRESH_DEBOUNCE_MS - elapsed);
    reloadTimer.current = setTimeout(() => {
      reloadTimer.current = undefined;
      lastReload.current = Date.now();
      setReloadKey((key) => key + 1);
    }, wait);
  }, []);

  useEffect(
    () => () => {
      if (reloadTimer.current) clearTimeout(reloadTimer.current);
    },
    [],
  );

  useSharedEvents(LEDGER_EVENT_TYPES, (event) => {
    const eventRunId = event.envelope.data?.run_id ?? event.envelope.subject;
    if (eventRunId === runId) scheduleReload();
  });

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

  // The SSE-triggered background refresh (issue #46): skips the very first
  // render (reloadKey === 0, already handled above). Refreshes the raw
  // ledger and, when one is selected, the active projection alongside it —
  // never nulls `records`/`projection`, never touches agent-state.
  useEffect(() => {
    if (reloadKey === 0 || !runId) return;
    const controller = new AbortController();
    const toApiError = (cause: unknown): ApiError =>
      cause instanceof ApiError
        ? cause
        : new ApiError(0, String(cause), "check the browser console");

    getLedger(runId, controller.signal)
      .then((payload) => {
        if (controller.signal.aborted) return;
        setRecords(payload.items);
        setLedgerVersion(payload.ledger_version);
        setError(null);
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setError(toApiError(cause));
      });

    if (projectionName) {
      getProjection(runId, projectionName, controller.signal)
        .then((payload) => {
          if (controller.signal.aborted) return;
          setProjection(payload);
          setProjectionError(null);
        })
        .catch((cause: unknown) => {
          if (controller.signal.aborted) return;
          setProjectionError(toApiError(cause));
        });
    }
    return () => controller.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reloadKey]);

  return (
    <section className="view-rail ledger-view">
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
          <div className="table-scroll">
            <LedgerTable
              id="projection-table"
              records={projection.items}
              caption={`${projection.kind} — ${projection.items.length} record(s)`}
            />
          </div>
        </>
      ) : (
        <div className="table-scroll">
          <LedgerTable
            id="ledger-table"
            records={records ?? []}
            caption={
              records === null
                ? "Loading…"
                : `${records.length} record(s), append order`
            }
          />
        </div>
      )}
    </section>
  );
}

export default LedgerView;
