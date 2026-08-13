// Types mirroring api/openapi/openapi.yaml (v1alpha1). Hand-maintained
// rather than generated: this is a read-only view over a small, stable
// slice of the API, and a generator would pull a build dependency into a
// package that deliberately has none beyond React/Vite.

export type RunState =
  | "created"
  | "running"
  | "waiting"
  | "completed"
  | "failed"
  | "cancelled";

/**
 * The §13.2 usage/cost rollup (task t2), summed over every attempt in scope
 * regardless of technical outcome ("retry burn" — a failed/cancelled attempt
 * that still burned tokens still counts). `attempts_reported === 0` is a
 * distinct, honest state from a reported sum that happens to equal zero
 * (`attempts_reported > 0`, `input_tokens === 0`) — a renderer MUST tell
 * these apart and never present the former as "0 tokens" (honesty condition
 * h27's UI half: absent usage is never rendered as a zero).
 *
 * `cost`/`currency` are set together only when every cost-reporting attempt
 * in scope agreed on one currency; `cost_by_currency` is set instead, as a
 * list, whenever more than one currency was seen. Never derive a currency
 * that was not reported — that is c35/h27's other half, and applies to every
 * renderer of this type.
 *
 * `cached_input_tokens`/`reasoning_tokens` (task t2, ADR 0009) sum the same
 * way `input_tokens`/`output_tokens` do: an attempt that reported tokens
 * but no cache telemetry at all contributes nothing, never a fabricated
 * zero. `cache_ratio` (`cached_input_tokens / input_tokens`) is present
 * only when `input_tokens > 0` — omitted, not a fabricated 0, when nothing
 * in scope reported any input tokens.
 */
export interface Usage {
  input_tokens: number;
  output_tokens: number;
  cached_input_tokens: number;
  reasoning_tokens: number;
  cache_ratio?: number;
  cost?: number;
  currency?: string;
  cost_by_currency?: CurrencyCost[];
  attempts_reported: number;
  attempts_not_reported: number;
}

/** One `Usage.cost_by_currency` entry. */
export interface CurrencyCost {
  /** Absent when the summarized attempt(s) reported a cost with no currency at all. */
  currency?: string;
  cost: number;
}

export interface Run {
  id: string;
  workflow_digest: string;
  state: RunState;
  input?: unknown;
  output?: unknown;
  created_at: string;
  updated_at: string;
  completed_at?: string;
  /**
   * The run-wide usage/cost rollup (task t2). Always present on
   * `GET /v1alpha1/runs/{id}`, `POST /v1alpha1/runs`, `PATCH
   * /v1alpha1/runs/{id}`, and `POST /v1alpha1/runs/{id}/cancel` — even a
   * freshly created run carries a present-but-empty rollup. Deliberately
   * NOT computed (and therefore absent here) on `GET /v1alpha1/runs`
   * (listRuns), which would otherwise need one extra query per listed row.
   */
  usage?: Usage;
  /** Operator-given display name (task t3). Absent when created without one. Immutable. */
  name?: string;
  /** Operator-given free-text description (task t3). Immutable. */
  description?: string;
  /** The run's flat category tag (task t3), retaggable via PATCH. */
  category?: string;
  /**
   * A truncated, best-effort guess at what this run is about, derived AT
   * READ TIME from the run's own input — never persisted, and present only
   * when `name` is absent. This is a guess, not something an operator
   * actually said: a renderer MUST mark it as derived and never present it
   * as if it were the given name.
   */
  display_hint?: string;
}

export interface RunList {
  items: Run[];
}

export interface Token {
  id: string;
  node_id: string;
  state: "active" | "consumed";
  parent_token_id?: string;
  created_at: string;
  consumed_at?: string;
}

/**
 * `dispatched` is not in openapi.yaml's Attempt.status enum, but it is the
 * `attempts.status` column default (migrations/0002_runtime_execution.sql),
 * so an attempt that is still in flight really does serialize with it. The
 * UI must render what the API can actually send, not only what the document
 * enumerates — the divergence is the spec's to reconcile, not this view's to
 * hide. `string & {}` keeps any other value the server adds renderable
 * rather than a type error.
 */
export type AttemptStatus =
  | "dispatched"
  | "succeeded"
  | "failed"
  | "timed_out"
  | "cancelled"
  | "policy_denied"
  | "contract_rejected"
  // eslint-disable-next-line @typescript-eslint/ban-types
  | (string & {});

export interface Attempt {
  id: string;
  node_run_id: string;
  attempt_number: number;
  actor_id?: string;
  status: AttemptStatus;
  fencing_token?: number;
  result?: unknown;
  started_at: string;
  completed_at?: string;
}

export type NodeRunState =
  | "ready"
  | "leased"
  | "running"
  | "waiting_external"
  | "completed"
  | "failed"
  | "cancelled";

export interface NodeRun {
  id: string;
  token_id?: string;
  node_id: string;
  state: NodeRunState;
  outcome?: string;
  visit_count: number;
  created_at: string;
  updated_at?: string;
  completed_at?: string;
  attempts?: Attempt[];
}

/**
 * One row of `GET /v1alpha1/node-runs` (task t11) — the cross-run "jobs
 * timeline" listing. The same `node_runs` row `NodeRun` above documents,
 * but listed across every run in the namespace rather than nested under
 * one: `run_id` is added because the parent run is no longer implied by a
 * URL path, and `actor_id` is the most recent attempt's actor/runner
 * reference (empty until the node run has been dispatched at least once).
 */
export interface NodeRunListItem {
  id: string;
  run_id: string;
  node_id: string;
  actor_id?: string;
  state: NodeRunState;
  outcome?: string;
  created_at: string;
  updated_at: string;
  completed_at?: string;
  /**
   * This node run's own usage/cost rollup (task t2), over its own attempts
   * only. Required — always present, even a present-but-empty rollup for a
   * node run with zero attempts (openapi.yaml's NodeRunListItem.usage).
   */
  usage: Usage;
}

export interface NodeRunList {
  items: NodeRunListItem[];
  /** Present only when a further page exists; pass back as `cursor`. */
  next_cursor?: string;
}

export interface RunView {
  run: Run;
  tokens: Token[];
  node_runs: NodeRun[];
}

export interface WorkflowVersion {
  id: string;
  workflow_key: string;
  version: number;
  source_format: "yaml" | "json";
  source: string;
  normalized_ir: WorkflowIR;
  digest: string;
  created_at: string;
}

/** `GET /v1alpha1/workflows` (task t8). */
export interface WorkflowVersionList {
  items: WorkflowVersion[];
}

/**
 * The request body `POST /v1alpha1/workflows/validate` and
 * `POST /v1alpha1/workflows` both take (task t9): the workflow document, in
 * either format, exactly as authored. `format` defaults to `yaml` server-side
 * when omitted, matching openapi.yaml's `WorkflowSource.format`.
 */
export interface WorkflowSource {
  format?: "yaml" | "json";
  source: string;
}

/** One compiler diagnostic — a JSON Pointer into the submitted document. */
export interface Diagnostic {
  level: "error" | "warning";
  path: string;
  code: string;
  message: string;
  hint: string;
}

/** `POST /v1alpha1/workflows/validate` response (task t9). */
export interface WorkflowValidation {
  valid: boolean;
  /** The normalized IR's content digest. Empty when there is any error diagnostic. */
  digest: string;
  diagnostics: Diagnostic[];
}

/** The subset of the normalized IR the Run view renders (PRD §11.3). */
export interface WorkflowIR {
  apiVersion?: string;
  kind?: string;
  metadata?: {
    name?: string;
    version?: string;
    ownerRef?: string;
    description?: string;
  };
  spec: {
    entry: string;
    nodes: Record<string, WorkflowIRNode>;
    edges: WorkflowIREdge[];
  };
}

export interface WorkflowIRNode {
  kind: string;
  ownerRef?: string;
  uses?: string;
  outcomes?: string[];
  approverRef?: string;
  deadline?: string;
  contract?: {
    input?: { schemaRef?: string; digest?: string };
    outcomes?: Record<string, { schemaRef?: string; digest?: string }>;
  };
  operation?: {
    image?: string;
    argv?: string[];
    network?: string;
    workspaceRef?: string;
  };
  ledger?: {
    read?: string[];
    propose?: string[];
    observe?: string[];
  };
  policy?: {
    timeout?: string;
    retry?: { maxAttempts?: number; backoff?: string };
  };
}

/** `from` is `"<nodeId>.<outcome>"`; `to` is a bare node id. */
export interface WorkflowIREdge {
  from: string;
  to: string;
  when?: string;
}

export type LedgerAuthorityValue =
  | "proposed"
  | "confirmed"
  | "observed"
  | "derived"
  | "rejected"
  | "superseded";

export interface LedgerRecord {
  id: string;
  schema_version: string;
  record_type: string;
  run_id: string;
  node_run_id?: string;
  attempt_id?: string;
  origin: { kind: string; actor_id: string; actor_revision?: string };
  authority: LedgerAuthorityValue;
  subject_ref?: string;
  data?: unknown;
  provenance_refs: string[];
  supersedes?: string;
  created_at: string;
  content_digest: string;
}

export interface LedgerRecords {
  items: LedgerRecord[];
  ledger_version: number;
}

export interface Projection {
  kind: string;
  subject: string;
  items: LedgerRecord[];
  summary?: unknown;
  digest: string;
}

/**
 * One `actors` table row (task t15, `GET /v1alpha1/actors`). Identity is
 * append-only: a capability or endpoint change is a new row with the same
 * `actor_key` and a higher `revision`, never an update — a consumer showing
 * "the fleet" collapses rows per actor_key to the newest revision.
 * `kind` is free text in the schema (today's registrations write "agent");
 * `capabilities`/`metadata` are the row's own JSON, rendered verbatim
 * server-side because neither ever carries a credential.
 */
export interface Actor {
  id: string;
  actor_key: string;
  revision: number;
  kind: string;
  protocol: string;
  endpoint_ref?: string;
  capabilities?: Record<string, unknown>;
  metadata?: Record<string, unknown>;
  created_at: string;
}

/** `GET /v1alpha1/actors` (task t15). */
export interface ActorList {
  items: Actor[];
}

/** A CloudEvents-1.0 envelope as emitted by internal/events. */
export interface EventEnvelope {
  id: string;
  source: string;
  specversion: string;
  type: string;
  subject?: string;
  time: string;
  datacontenttype: string;
  data: Record<string, unknown>;
}

/** An envelope plus the SSE `id:` field (the per-run sequence number). */
export interface RunEvent {
  sequence: string;
  envelope: EventEnvelope;
}

export interface ApiErrorBody {
  code: number;
  message: string;
  remediation: string;
}

/**
 * `human_tasks.request` as `POST` dispatch stored it (PRD §9.9, task t6's
 * `buildHumanTaskRequest` in internal/engine/humantask.go). Carried
 * verbatim by the API — this is a record of what the human was actually
 * shown, so every field is optional except the audit's node id, and a
 * renderer must not fabricate what is absent (e.g. never invent a deadline).
 */
export interface HumanTaskContextRefs {
  /** A single input pointer, exactly as authored. */
  from?: string;
  /** Named input bindings, exactly as authored. */
  bindings?: Record<string, string>;
}

/** PRD §9.9 "audit identity" not already on the task row's own columns. */
export interface HumanTaskAudit {
  node_id?: string;
  token_id?: string;
  workflow_digest?: string;
  from_node?: string;
  from_outcome?: string;
}

export interface HumanTaskRequestPayload {
  /** Unresolved schema reference, like every schemaRef the engine carries. */
  decision_schema_ref?: string;
  /** The requested approver role/group, exactly as authored. */
  approver_ref?: string;
  /** RFC3339. Absent when the node declared no deadline — never "now". */
  deadline?: string;
  /** The node's resolved outcome set — what the decider may choose from. */
  allowed_outcomes?: string[];
  context_refs?: HumanTaskContextRefs;
  audit?: HumanTaskAudit;
}

/** One `human_tasks` row (components.schemas.HumanTask, task t6/t12). */
export interface HumanTask {
  id: string;
  run_id: string;
  node_run_id?: string;
  kind: string;
  assigned_owner_id?: string;
  status: "pending" | "decided";
  request: HumanTaskRequestPayload;
  /** The decision payload, present once decided. */
  response?: unknown;
  created_at: string;
  resolved_at?: string;
}

/** `GET /v1alpha1/human-tasks` (components.schemas.HumanTaskList). */
export interface HumanTaskList {
  items: HumanTask[];
}

/** `POST /v1alpha1/human-tasks/{id}/decision` request body. */
export interface HumanTaskDecisionRequest {
  outcome: string;
  decider_actor_id: string;
  response?: unknown;
  /**
   * The run's ledger version the decider last read (`GET
   * /runs/{id}/ledger`'s `ledger_version`). A stale expectation is refused
   * atomically with nothing written — same guard as PRD §10.8 review
   * commits.
   */
  expected_ledger_version: number;
  record_ids?: string[];
}

/** `POST .../decision`'s result (components.schemas.HumanTaskDecisionResult). */
export interface HumanTaskDecisionResult {
  human_task_id: string;
  run_id: string;
  node_run_id: string;
  outcome: string;
  ledger_records: LedgerRecord[];
  next_node_id?: string;
  next_node_run_id?: string;
  next_human_task_id?: string;
  run_state: string;
  run_output?: unknown;
}
