import type {
  LedgerRecords,
  NodeRunList,
  Projection,
  RunList,
  RunState,
  RunView,
  WorkflowVersion,
} from "./types";

/**
 * Same-origin API root. In dev, vite.config.ts proxies it to the Go control
 * plane; in production the Go binary serves this bundle and `/v1alpha1`
 * from one origin. Phase 1 has no auth by design (PRD §26) — no token is
 * attached here, and none should be added without the auth story landing
 * first.
 */
export const API_ROOT = "/v1alpha1";

export class ApiError extends Error {
  readonly status: number;
  readonly remediation: string;

  constructor(status: number, message: string, remediation: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.remediation = remediation;
  }
}

async function getJson<T>(path: string, signal?: AbortSignal): Promise<T> {
  let response: Response;
  try {
    response = await fetch(`${API_ROOT}${path}`, {
      signal,
      headers: { accept: "application/json" },
    });
  } catch (cause) {
    throw new ApiError(
      0,
      `cannot reach the control plane at ${API_ROOT}`,
      "start the API (`nodes serve`) or point NODES_API at a running one",
    );
  }

  const body = await response.text();
  if (!response.ok) {
    let message = `${response.status} ${response.statusText}`;
    let remediation = "check the run id and the API logs";
    try {
      const parsed = JSON.parse(body) as {
        message?: string;
        remediation?: string;
      };
      if (parsed.message) message = parsed.message;
      if (parsed.remediation) remediation = parsed.remediation;
    } catch {
      /* a non-JSON error body stays as the status line */
    }
    throw new ApiError(response.status, message, remediation);
  }

  try {
    return JSON.parse(body) as T;
  } catch {
    throw new ApiError(
      response.status,
      `${path} did not return JSON`,
      "the request probably hit the dev server's SPA fallback — check the API proxy",
    );
  }
}

/** GET /v1alpha1/runs query parameters (task t11). */
export interface ListRunsParams {
  state?: RunState;
  /** RFC3339. Only runs updated at or after this instant. */
  updated_since?: string;
  /** RFC3339. Only runs updated at or before this instant. */
  updated_until?: string;
  /** Defaults to `created_at`, or `updated_at` once either bound is set. */
  sort?: "created_at" | "updated_at";
  limit?: number;
}

function toQueryString(
  params: Record<string, string | number | undefined> | undefined,
): string {
  if (!params) return "";
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined) continue;
    search.set(key, String(value));
  }
  const query = search.toString();
  return query ? `?${query}` : "";
}

export const listRuns = (signal?: AbortSignal, params?: ListRunsParams) =>
  getJson<RunList>(
    `/runs${toQueryString(params as Record<string, string | number | undefined> | undefined)}`,
    signal,
  );

export const getRun = (id: string, signal?: AbortSignal) =>
  getJson<RunView>(`/runs/${encodeURIComponent(id)}`, signal);

/**
 * GET /v1alpha1/node-runs query parameters (task t11): the cross-run "jobs
 * timeline" listing, keyset-paginated by `cursor`/`next_cursor` rather than
 * an offset (see openapi.yaml's listNodeRuns for why — `updated_at` moves
 * under an OFFSET page as other rows in the namespace transition).
 */
export interface ListNodeRunsParams {
  /** RFC3339. Only node runs updated at or after this instant. */
  updated_since?: string;
  /** RFC3339. Only node runs updated at or before this instant. */
  updated_until?: string;
  /** An opaque `next_cursor` from a previous page; omit for the first page. */
  cursor?: string;
  limit?: number;
}

export const listNodeRuns = (
  signal?: AbortSignal,
  params?: ListNodeRunsParams,
) =>
  getJson<NodeRunList>(
    `/node-runs${toQueryString(params as Record<string, string | number | undefined> | undefined)}`,
    signal,
  );

export const getWorkflow = (digest: string, signal?: AbortSignal) =>
  getJson<WorkflowVersion>(
    `/workflows/${encodeURIComponent(digest)}`,
    signal,
  );

export const getLedger = (runId: string, signal?: AbortSignal) =>
  getJson<LedgerRecords>(
    `/runs/${encodeURIComponent(runId)}/ledger`,
    signal,
  );

export const getProjection = (
  runId: string,
  name: string,
  signal?: AbortSignal,
) =>
  getJson<Projection>(
    `/runs/${encodeURIComponent(runId)}/ledger/projections/${encodeURIComponent(name)}`,
    signal,
  );

/** The SSE endpoint's URL, with the resume point applied. */
export function runEventsUrl(runId: string, from?: string): string {
  const base = `${API_ROOT}/runs/${encodeURIComponent(runId)}/events`;
  // EventSource cannot set a Last-Event-ID header on its *first* connection,
  // so the API accepts the same resume point as a `from` query parameter
  // (openapi.yaml, streamRunEvents). On a browser-driven reconnect the
  // header is sent automatically as well; both name the same sequence.
  return from ? `${base}?from=${encodeURIComponent(from)}` : base;
}

/** The PRD §10.9 standard projections, in the order the picker lists them. */
export const PROJECTION_NAMES = [
  "current_scope",
  "confirmed_claims",
  "open_assumptions_and_questions",
  "ready_tasks",
  "active_tasks",
  "verification_queue",
  "decision_history",
  "delivery_summary",
] as const;
