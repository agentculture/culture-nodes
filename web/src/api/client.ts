import type {
  ActorList,
  CommitReviewRequest,
  CreateReviewRequest,
  HumanTaskDecisionRequest,
  HumanTaskDecisionResult,
  HumanTaskList,
  LedgerRecords,
  NodeRunList,
  PendingDecisionList,
  PlanImport,
  PlanImportSummaryList,
  Projection,
  ReviewCommitResult,
  ReviewRequest,
  RunList,
  RunState,
  RunView,
  TicketProjection,
  TicketReply,
  TicketReplyRequest,
  Version,
  WorkflowSource,
  WorkflowValidation,
  WorkflowVersion,
  WorkflowVersionList,
} from "./types";

/**
 * Same-origin API root. In dev, vite.config.ts proxies it to the Go control
 * plane; in production the Go binary serves this bundle and `/v1alpha1`
 * from one origin. Phase 1 is authless by design (PRD §26) with exactly one
 * class of exceptions: calls that write human-authority records — human-task
 * decisions, the two review calls, and ticket replies — require the decision
 * token the user presents per call (retention policy in
 * ./decision-token.ts). No other request attaches a credential.
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
  workflow_key?: string;
  cursor?: string;
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

export const getTicket = (id: string, signal?: AbortSignal) =>
  getJson<TicketProjection>(`/tickets/${encodeURIComponent(id)}`, signal);

export const postTicketReply = (
  id: string,
  request: TicketReplyRequest,
  token: string,
  signal?: AbortSignal,
) =>
  postJson<TicketReply>(
    `/tickets/${encodeURIComponent(id)}/replies`,
    request,
    signal,
    { authorization: `Bearer ${token}` },
  );

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

async function postJson<T>(
  path: string,
  body: unknown,
  signal?: AbortSignal,
  headers?: Record<string, string>,
): Promise<T> {
  let response: Response;
  try {
    response = await fetch(`${API_ROOT}${path}`, {
      method: "POST",
      signal,
      headers: {
        accept: "application/json",
        "content-type": "application/json",
        ...headers,
      },
      body: JSON.stringify(body),
    });
  } catch (cause) {
    throw new ApiError(
      0,
      `cannot reach the control plane at ${API_ROOT}`,
      "start the API (`nodes serve`) or point NODES_API at a running one",
    );
  }

  const text = await response.text();
  if (!response.ok) {
    let message = `${response.status} ${response.statusText}`;
    let remediation = "check the submitted document and try again";
    try {
      const parsed = JSON.parse(text) as {
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
    return JSON.parse(text) as T;
  } catch {
    throw new ApiError(
      response.status,
      `${path} did not return JSON`,
      "the request probably hit the dev server's SPA fallback — check the API proxy",
    );
  }
}

export const getWorkflow = (digest: string, signal?: AbortSignal) =>
  getJson<WorkflowVersion>(
    `/workflows/${encodeURIComponent(digest)}`,
    signal,
  );

/**
 * `POST /v1alpha1/workflows/validate` (task t9): compiles `source` and
 * returns every diagnostic. A document with error diagnostics is a
 * documented domain outcome (`valid: false`, HTTP 200) — never a technical
 * failure (PRD §3.4) — so this never throws for invalid *content*; it only
 * throws (via `ApiError`) when the request itself is malformed or the
 * control plane cannot be reached.
 */
export const validateWorkflow = (
  source: WorkflowSource,
  signal?: AbortSignal,
) => postJson<WorkflowValidation>("/workflows/validate", source, signal);

export const createWorkflowGeneration = (
  request: import("./types").CreateWorkflowGeneration,
  signal?: AbortSignal,
) => postJson<import("./types").WorkflowGeneration>("/workflow-generations", request, signal);

export const getWorkflowGeneration = (runID: string, signal?: AbortSignal) =>
  getJson<import("./types").WorkflowGeneration>(
    `/workflow-generations/${encodeURIComponent(runID)}`,
    signal,
  );

/**
 * `POST /v1alpha1/workflows` (task t9): publishes `source` as an immutable
 * workflow version, exactly as submitted — the caller must pass the operator's
 * source bytes unmodified so the resulting digest is byte-identical to
 * publishing the same document via `nodes workflow publish`. Idempotent:
 * republishing the same content returns the existing version (HTTP 200)
 * rather than a conflict.
 */
export const publishWorkflow = (
  source: WorkflowSource,
  signal?: AbortSignal,
) => postJson<WorkflowVersion>("/workflows", source, signal);

/** GET /v1alpha1/workflows query parameters (task t8). */
export interface ListWorkflowsParams {
  /** Filter to versions of one workflow key. */
  workflow_key?: string;
  limit?: number;
}

export const listWorkflows = (
  signal?: AbortSignal,
  params?: ListWorkflowsParams,
) =>
  getJson<WorkflowVersionList>(
    `/workflows${toQueryString(params as Record<string, string | number | undefined> | undefined)}`,
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

/** GET /v1alpha1/pending-decisions query parameters (task t30). */
export interface ListPendingDecisionsParams {
  /** Only this run's records. */
  run_id?: string;
  /** Only this ledger record type (e.g. `claim`). */
  record_type?: string;
  /** Only records this actor produced. */
  actor_id?: string;
  limit?: number;
  /** An opaque `next_cursor` from a previous page. */
  cursor?: string;
}

/**
 * `GET /v1alpha1/pending-decisions` (task t30, issue #99): every proposed
 * record no review has decided, grouped by run.
 *
 * This is deliberately not something the browser derives from the ledger
 * feed: "decided" means a review record names it, so the question is a join,
 * and a client-side approximation of it would drift from the gate scripts
 * and the API's own answer.
 */
export const listPendingDecisions = (
  signal?: AbortSignal,
  params?: ListPendingDecisionsParams,
) =>
  getJson<PendingDecisionList>(
    `/pending-decisions${toQueryString(params as Record<string, string | number | undefined> | undefined)}`,
    signal,
  );

/**
 * `POST /v1alpha1/runs/{id}/reviews` (task t30): open a review over the
 * records a human is about to decide, pinned to the ledger version they were
 * read at. Authenticated for the same reason `decideHumanTask` is — it is
 * half of writing a human-authority record.
 */
export const createReview = (
  runId: string,
  request: CreateReviewRequest,
  token: string,
  signal?: AbortSignal,
) =>
  postJson<ReviewRequest>(
    `/runs/${encodeURIComponent(runId)}/reviews`,
    request,
    signal,
    { authorization: `Bearer ${token}` },
  );

/**
 * `POST /v1alpha1/reviews/{id}/commit` (task t30): record the decision. Each
 * verdict becomes its own immutable review record naming the reviewer, the
 * record decided, and the stated rationale. The records under review are not
 * modified — a confirmed claim still reads `proposed`.
 */
export const commitReview = (
  reviewId: string,
  request: CommitReviewRequest,
  token: string,
  signal?: AbortSignal,
) =>
  postJson<ReviewCommitResult>(
    `/reviews/${encodeURIComponent(reviewId)}/commit`,
    request,
    signal,
    { authorization: `Bearer ${token}` },
  );

/** GET /v1alpha1/human-tasks query parameters (task t14). */
export interface ListHumanTasksParams {
  /** Filter to one status; omitted returns every task, newest first. */
  status?: "pending" | "decided";
  limit?: number;
  /** An opaque `next_cursor` from a previous page. */
  cursor?: string;
}

export const listHumanTasks = (
  signal?: AbortSignal,
  params?: ListHumanTasksParams,
) =>
  getJson<HumanTaskList>(
    `/human-tasks${toQueryString(params as Record<string, string | number | undefined> | undefined)}`,
    signal,
  );

/**
 * `POST /v1alpha1/human-tasks/{id}/decision` (task t14): commit a human
 * decision on a pending task. The ONLY authenticated call in this client —
 * the API refuses it without `Authorization: Bearer <token>`
 * (internal/api/humantasks.go's requireDecisionAuth), so `token` is a
 * required argument here, deliberately not optional: there is no
 * unauthenticated code path to a mutation from the browser. Where the token
 * lives (sessionStorage only) and why is ./decision-token.ts's contract.
 */
export const decideHumanTask = (
  id: string,
  decision: HumanTaskDecisionRequest,
  token: string,
  signal?: AbortSignal,
) =>
  postJson<HumanTaskDecisionResult>(
    `/human-tasks/${encodeURIComponent(id)}/decision`,
    decision,
    signal,
    { authorization: `Bearer ${token}` },
  );

/** GET /v1alpha1/actors (task t15): every registered actor row. */
export const listActors = (signal?: AbortSignal) =>
  getJson<ActorList>("/actors", signal);

/**
 * `GET /v1alpha1/plan-imports?slug=` (task t23): every import snapshot of
 * one plan, most recent first — `items[0]` is "the current one". `slug` is
 * required (there is no cross-slug listing; see openapi.yaml's
 * listPlanImports).
 */
export const listPlanImports = (slug: string, signal?: AbortSignal) =>
  getJson<PlanImportSummaryList>(
    `/plan-imports${toQueryString({ slug })}`,
    signal,
  );

/**
 * `GET /v1alpha1/plan-imports/{id}` (task t22): one full plan-import
 * snapshot — every task's real status/dependency edges and every
 * deviation, with its origin.
 */
export const getPlanImport = (id: string, signal?: AbortSignal) =>
  getJson<PlanImport>(`/plan-imports/${encodeURIComponent(id)}`, signal);

/**
 * The cross-run SSE endpoint's URL (task t17, `GET /v1alpha1/events`), with
 * the resume point applied. Unlike the per-run stream the cursor here is
 * the events table's own ULID primary key, not a per-run sequence — see
 * internal/api/events.go's handleStreamEvents ordering note. Same
 * first-connection rule as runEventsUrl: EventSource cannot set a
 * Last-Event-ID header before its first connect, so the API accepts the
 * same cursor as `?from=`.
 *
 * `runs` is the server's optional scope-down filter (task t27, c48/h41's
 * sibling requirement; internal/api/events.go:294-313's `runsFilterParam`):
 * an explicit `?runs=id,id` list narrows the feed to those runs instead of
 * the default active-runs+lifecycle set. Absent or empty means "no explicit
 * filter" on both ends — the query param is omitted entirely rather than
 * sent empty, matching the server's own empty-means-absent parsing.
 */
export function meshEventsUrl(from?: string, runs?: readonly string[]): string {
  const base = `${API_ROOT}/events`;
  const params: string[] = [];
  if (from) params.push(`from=${encodeURIComponent(from)}`);
  if (runs && runs.length > 0) {
    params.push(`runs=${runs.map(encodeURIComponent).join(",")}`);
  }
  return params.length > 0 ? `${base}?${params.join("&")}` : base;
}

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

/**
 * `GET /v1alpha1/version` (task t27): the revision the control plane serving
 * this bundle was built from. Unauthenticated, like healthz and readyz, for
 * the reason internal/api/version.go states — a reader who has to hold a
 * secret just to learn what they are looking at does not look.
 *
 * The header reads it once per page load and renders it verbatim. Nothing
 * here interprets the answer: an unstamped build reports no revision and
 * says why in `staleness`, and that sentence is what the tooltip shows.
 */
export const getVersion = (signal?: AbortSignal) =>
  getJson<Version>("/version", signal);
