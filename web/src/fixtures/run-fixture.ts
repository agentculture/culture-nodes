/**
 * The PRD §8.7 first-slice run, as fixture data.
 *
 * The workflow is `deliver-change` — the same document that lives at
 * schemas/examples/deliver-change.workflow.json, trimmed to the fields the
 * Run view actually reads. The run walks intake → plan → build → test →
 * verify, then takes the `verify.changes_required` loop back to build, where
 * a second attempt is in flight. That is exactly the slice §8.7 names: an
 * active path, a visible loop, a headspace code node with observed test
 * evidence, and a node still running.
 *
 * Used by the component tests and by the Playwright e2e run, which serves it
 * through request interception so the browser walk needs no Go server.
 *
 * Plain TypeScript on purpose: no JSX, no DOM types, so both the app's
 * tsconfig and the Playwright/node one can compile it.
 */

import type {
  LedgerRecord,
  LedgerRecords,
  NodeRunListItem,
  RunEvent,
  RunView,
  Usage,
  WorkflowIR,
  WorkflowVersion,
} from "../api/types";

export const RUN_ID = "run-01J8XKQ3P0FIRSTSLICE";
export const WORKFLOW_DIGEST =
  "sha256:2c1e0a9b6d4f8e37a5b0c9d2e1f4a7b6c3d8e5f2a9b4c7d0e3f6a1b8c5d2e9f4";

export const WORKFLOW_IR: WorkflowIR = {
  apiVersion: "nodes.culture.dev/v1alpha1",
  kind: "Workflow",
  metadata: {
    name: "deliver-change",
    version: "1.0.0",
    ownerRef: "team/platform-ai",
  },
  spec: {
    entry: "intake",
    nodes: {
      intake: {
        kind: "agent",
        ownerRef: "team/platform-ai",
        uses: "actor://company/intake@sha256:111111",
        outcomes: ["completed"],
        contract: {
          input: { schemaRef: "./contracts/change-request.schema.json" },
          outcomes: {
            completed: {
              schemaRef: "./contracts/intake-result.schema.json",
              digest: "sha256:aaa1",
            },
          },
        },
        policy: { timeout: "5m", retry: { maxAttempts: 2 } },
      },
      plan: {
        kind: "agent",
        ownerRef: "team/architecture",
        uses: "actor://company/planner@sha256:222222",
        outcomes: ["completed"],
        policy: { timeout: "15m", retry: { maxAttempts: 2 } },
      },
      build: {
        kind: "agent",
        ownerRef: "team/developer-experience",
        uses: "actor://company/developer@sha256:333333",
        outcomes: ["completed"],
        policy: { timeout: "45m", retry: { maxAttempts: 2 } },
      },
      test: {
        kind: "code",
        ownerRef: "team/developer-experience",
        uses: "runner://headspace/docker@sha256:555555",
        outcomes: ["passed", "failed"],
        operation: {
          image: "registry.example/python-test@sha256:666666",
          argv: ["python", "-m", "pytest", "-q"],
          network: "none",
          workspaceRef: "/nodes/build/artifacts/workspace",
        },
        policy: { timeout: "15m", retry: { maxAttempts: 1 } },
      },
      verify: {
        kind: "agent",
        ownerRef: "team/quality-platform",
        uses: "actor://company/verifier@sha256:444444",
        outcomes: ["passed", "changes_required", "blocked"],
        policy: { timeout: "20m", retry: { maxAttempts: 2 } },
      },
      "human-review": {
        kind: "approval",
        ownerRef: "team/platform-ai",
        approverRef: "group/platform-ai-approvers",
        deadline: "24h",
        outcomes: ["approved", "rejected"],
      },
      finish: {
        kind: "end",
        ownerRef: "team/platform-ai",
        outcomes: [],
      },
    },
    edges: [
      { from: "intake.completed", to: "plan" },
      { from: "plan.completed", to: "build" },
      { from: "build.completed", to: "test" },
      { from: "test.passed", to: "verify" },
      { from: "test.failed", to: "build" },
      { from: "verify.passed", to: "finish" },
      { from: "verify.changes_required", to: "build" },
      { from: "verify.blocked", to: "human-review" },
      { from: "human-review.approved", to: "build" },
      { from: "human-review.rejected", to: "finish" },
    ],
  },
};

export const WORKFLOW_VERSION: WorkflowVersion = {
  id: "wfv-01J8XKQ3P0",
  workflow_key: "deliver-change",
  version: 1,
  source_format: "yaml",
  source: "# see schemas/examples/deliver-change.workflow.json\n",
  normalized_ir: WORKFLOW_IR,
  digest: WORKFLOW_DIGEST,
  created_at: "2026-08-09T09:00:00Z",
};

const t = (minute: number, second = 0) =>
  `2026-08-09T09:${String(minute).padStart(2, "0")}:${String(second).padStart(2, "0")}Z`;

/**
 * The run-wide §13.2 usage rollup (task t2/t5): the sum of the five
 * *completed* attempts' reported usage (intake, plan, build-1, test,
 * verify) — the sixth, still-`dispatched` attempt (build-2) has not
 * completed and so is neither reported nor not-reported yet, matching the
 * NODE_RUN_USAGE per-attempt figures below.
 */
export const RUN_USAGE: Usage = {
  input_tokens: 14900,
  output_tokens: 4600,
  cost: 0.93,
  currency: "USD",
  attempts_reported: 5,
  attempts_not_reported: 0,
};

export const RUN_VIEW: RunView = {
  run: {
    id: RUN_ID,
    workflow_digest: WORKFLOW_DIGEST,
    state: "running",
    input: { title: "add the ledger projection endpoint" },
    created_at: t(0),
    updated_at: t(31),
    name: "deliver the ledger projection endpoint",
    category: "delivery",
    usage: RUN_USAGE,
  },
  tokens: [
    { id: "tok-1", node_id: "intake", state: "consumed", created_at: t(0), consumed_at: t(3) },
    { id: "tok-2", node_id: "plan", state: "consumed", created_at: t(3), consumed_at: t(9) },
    { id: "tok-3", node_id: "build", state: "consumed", created_at: t(9), consumed_at: t(18) },
    { id: "tok-4", node_id: "test", state: "consumed", created_at: t(18), consumed_at: t(24) },
    { id: "tok-5", node_id: "verify", state: "consumed", created_at: t(24), consumed_at: t(30) },
    { id: "tok-6", node_id: "build", state: "active", created_at: t(30) },
  ],
  node_runs: [
    {
      id: "nr-intake",
      token_id: "tok-1",
      node_id: "intake",
      state: "completed",
      outcome: "completed",
      visit_count: 1,
      created_at: t(0),
      completed_at: t(3),
      attempts: [
        {
          id: "att-intake-1",
          node_run_id: "nr-intake",
          attempt_number: 1,
          actor_id: "actor://company/intake",
          status: "succeeded",
          started_at: t(0, 5),
          completed_at: t(2, 50),
        },
      ],
    },
    {
      id: "nr-plan",
      token_id: "tok-2",
      node_id: "plan",
      state: "completed",
      outcome: "completed",
      visit_count: 1,
      created_at: t(3),
      completed_at: t(9),
      attempts: [
        {
          id: "att-plan-1",
          node_run_id: "nr-plan",
          attempt_number: 1,
          actor_id: "actor://company/planner",
          status: "succeeded",
          started_at: t(3, 2),
          completed_at: t(8, 40),
        },
      ],
    },
    {
      id: "nr-build-1",
      token_id: "tok-3",
      node_id: "build",
      state: "completed",
      outcome: "completed",
      visit_count: 1,
      created_at: t(9),
      completed_at: t(18),
      attempts: [
        {
          id: "att-build-1",
          node_run_id: "nr-build-1",
          attempt_number: 1,
          actor_id: "actor://company/developer",
          status: "succeeded",
          started_at: t(9, 1),
          completed_at: t(17, 55),
        },
      ],
    },
    {
      id: "nr-test",
      token_id: "tok-4",
      node_id: "test",
      state: "completed",
      outcome: "passed",
      visit_count: 1,
      created_at: t(18),
      completed_at: t(24),
      attempts: [
        {
          id: "att-test-1",
          node_run_id: "nr-test",
          attempt_number: 1,
          actor_id: "runner://headspace/docker",
          status: "succeeded",
          started_at: t(18, 2),
          completed_at: t(23, 30),
        },
      ],
    },
    {
      id: "nr-verify",
      token_id: "tok-5",
      node_id: "verify",
      state: "completed",
      outcome: "changes_required",
      visit_count: 1,
      created_at: t(24),
      completed_at: t(30),
      attempts: [
        {
          id: "att-verify-1",
          node_run_id: "nr-verify",
          attempt_number: 1,
          actor_id: "actor://company/verifier",
          status: "succeeded",
          started_at: t(24, 4),
          completed_at: t(29, 48),
        },
      ],
    },
    {
      id: "nr-build-2",
      token_id: "tok-6",
      node_id: "build",
      state: "running",
      visit_count: 2,
      created_at: t(30),
      updated_at: t(31),
      attempts: [
        {
          id: "att-build-2",
          node_run_id: "nr-build-2",
          attempt_number: 1,
          actor_id: "actor://company/developer",
          // In flight: the attempts table's default status, no completed_at.
          status: "dispatched",
          started_at: t(30, 10),
        },
      ],
    },
  ],
};

/**
 * `GET /v1alpha1/node-runs` (task t11/t2), the flat listing useRunData.ts
 * best-effort joins back onto RUN_VIEW's node runs by id to recover
 * per-node-run usage (NodeRun, nested under RunView, carries no `usage`
 * field of its own — see openapi.yaml). One entry per RUN_VIEW.node_runs
 * row; `nr-build-2` (the still-`dispatched` attempt) is deliberately
 * `attempts_reported: 0` so the RunView e2e spec can assert the
 * not-reported state renders honestly, merged alongside `nr-build-1`'s
 * reported figures, for the "build" node's detail panel.
 */
export const NODE_RUN_USAGE_ITEMS: NodeRunListItem[] = [
  {
    id: "nr-intake",
    run_id: RUN_ID,
    node_id: "intake",
    actor_id: "actor://company/intake",
    state: "completed",
    outcome: "completed",
    created_at: t(0),
    updated_at: t(3),
    completed_at: t(3),
    usage: {
      input_tokens: 1200,
      output_tokens: 300,
      attempts_reported: 1,
      attempts_not_reported: 0,
    },
  },
  {
    id: "nr-plan",
    run_id: RUN_ID,
    node_id: "plan",
    actor_id: "actor://company/planner",
    state: "completed",
    outcome: "completed",
    created_at: t(3),
    updated_at: t(9),
    completed_at: t(9),
    usage: {
      input_tokens: 3600,
      output_tokens: 900,
      attempts_reported: 1,
      attempts_not_reported: 0,
    },
  },
  {
    id: "nr-build-1",
    run_id: RUN_ID,
    node_id: "build",
    actor_id: "actor://company/developer",
    state: "completed",
    outcome: "completed",
    created_at: t(9),
    updated_at: t(18),
    completed_at: t(18),
    usage: {
      input_tokens: 5200,
      output_tokens: 1800,
      cost: 0.52,
      currency: "USD",
      attempts_reported: 1,
      attempts_not_reported: 0,
    },
  },
  {
    id: "nr-test",
    run_id: RUN_ID,
    node_id: "test",
    actor_id: "runner://headspace/docker",
    state: "completed",
    outcome: "passed",
    created_at: t(18),
    updated_at: t(24),
    completed_at: t(24),
    usage: {
      input_tokens: 800,
      output_tokens: 200,
      attempts_reported: 1,
      attempts_not_reported: 0,
    },
  },
  {
    id: "nr-verify",
    run_id: RUN_ID,
    node_id: "verify",
    actor_id: "actor://company/verifier",
    state: "completed",
    outcome: "changes_required",
    created_at: t(24),
    updated_at: t(30),
    completed_at: t(30),
    usage: {
      input_tokens: 4100,
      output_tokens: 1400,
      cost: 0.41,
      currency: "USD",
      attempts_reported: 1,
      attempts_not_reported: 0,
    },
  },
  {
    id: "nr-build-2",
    run_id: RUN_ID,
    node_id: "build",
    actor_id: "actor://company/developer",
    state: "running",
    created_at: t(30),
    updated_at: t(31),
    // Still in flight: no usage block has been reported yet.
    usage: {
      input_tokens: 0,
      output_tokens: 0,
      attempts_reported: 0,
      attempts_not_reported: 1,
    },
  },
];

let sequence = 0;
function event(
  type: string,
  data: Record<string, unknown>,
  time: string,
): RunEvent {
  sequence += 1;
  return {
    sequence: String(sequence),
    envelope: {
      id: `evt-${String(sequence).padStart(3, "0")}`,
      source: "nodes",
      specversion: "1.0",
      type: `dev.culture.nodes.${type}`,
      subject: RUN_ID,
      time,
      datacontenttype: "application/json",
      data: { run_id: RUN_ID, ...data },
    },
  };
}

/**
 * The committed event history for RUN_VIEW, in per-run sequence order — the
 * exact frames `GET /v1alpha1/runs/{id}/events` replays from sequence 1.
 */
export const RUN_EVENTS: RunEvent[] = [
  event("run.created", { workflow_digest: WORKFLOW_DIGEST }, t(0)),
  event("node-run.ready", { node_id: "intake", node_run_id: "nr-intake", visit: 1 }, t(0)),
  event("attempt.started", { node_id: "intake", node_run_id: "nr-intake", attempt_id: "att-intake-1", actor_id: "actor://company/intake" }, t(0, 5)),
  event("attempt.completed", { node_id: "intake", node_run_id: "nr-intake", attempt_id: "att-intake-1", tech_status: "succeeded", outcome: "completed" }, t(2, 50)),
  event("token.transitioned", { from_node: "intake", to_node: "plan", outcome: "completed", edge: "intake.completed", visit: 1 }, t(3)),
  event("attempt.started", { node_id: "plan", node_run_id: "nr-plan", attempt_id: "att-plan-1", actor_id: "actor://company/planner" }, t(3, 2)),
  event("attempt.completed", { node_id: "plan", node_run_id: "nr-plan", attempt_id: "att-plan-1", tech_status: "succeeded", outcome: "completed" }, t(8, 40)),
  event("token.transitioned", { from_node: "plan", to_node: "build", outcome: "completed", edge: "plan.completed", visit: 1 }, t(9)),
  event("attempt.started", { node_id: "build", node_run_id: "nr-build-1", attempt_id: "att-build-1", actor_id: "actor://company/developer" }, t(9, 1)),
  event("attempt.completed", { node_id: "build", node_run_id: "nr-build-1", attempt_id: "att-build-1", tech_status: "succeeded", outcome: "completed" }, t(17, 55)),
  event("token.transitioned", { from_node: "build", to_node: "test", outcome: "completed", edge: "build.completed", visit: 1 }, t(18)),
  event("attempt.started", { node_id: "test", node_run_id: "nr-test", attempt_id: "att-test-1", actor_id: "runner://headspace/docker" }, t(18, 2)),
  event("runner.operation-completed", { node_id: "test", node_run_id: "nr-test", image: "registry.example/python-test@sha256:666666", exit_code: 0 }, t(23, 20)),
  event("attempt.completed", { node_id: "test", node_run_id: "nr-test", attempt_id: "att-test-1", tech_status: "succeeded", outcome: "passed" }, t(23, 30)),
  event("token.transitioned", { from_node: "test", to_node: "verify", outcome: "passed", edge: "test.passed", visit: 1 }, t(24)),
  event("attempt.started", { node_id: "verify", node_run_id: "nr-verify", attempt_id: "att-verify-1", actor_id: "actor://company/verifier" }, t(24, 4)),
  event("attempt.completed", { node_id: "verify", node_run_id: "nr-verify", attempt_id: "att-verify-1", tech_status: "succeeded", outcome: "changes_required" }, t(29, 48)),
  // The loop: a domain outcome that follows a graph edge, never a failure.
  event("token.transitioned", { from_node: "verify", to_node: "build", outcome: "changes_required", edge: "verify.changes_required", visit: 2 }, t(30)),
  event("attempt.started", { node_id: "build", node_run_id: "nr-build-2", attempt_id: "att-build-2", actor_id: "actor://company/developer" }, t(30, 10)),
];

export const LEDGER_RECORDS: LedgerRecord[] = [
  {
    id: "lr-001",
    schema_version: "nodes.culture.dev/ledger/v1alpha1",
    record_type: "announcement",
    run_id: RUN_ID,
    node_run_id: "nr-intake",
    attempt_id: "att-intake-1",
    origin: { kind: "agent", actor_id: "actor://company/intake" },
    authority: "proposed",
    data: { title: "add the ledger projection endpoint" },
    provenance_refs: [],
    created_at: t(2, 51),
    content_digest: "sha256:1111111111111111111111111111111111111111",
  },
  {
    id: "lr-002",
    schema_version: "nodes.culture.dev/ledger/v1alpha1",
    record_type: "claim",
    run_id: RUN_ID,
    node_run_id: "nr-intake",
    attempt_id: "att-intake-1",
    origin: { kind: "agent", actor_id: "actor://company/intake" },
    authority: "proposed",
    data: { statement: "the endpoint needs a digest in its response" },
    provenance_refs: [],
    created_at: t(2, 52),
    content_digest: "sha256:2222222222222222222222222222222222222222",
  },
  {
    id: "lr-003",
    schema_version: "nodes.culture.dev/ledger/v1alpha1",
    record_type: "claim",
    run_id: RUN_ID,
    node_run_id: "nr-intake",
    origin: { kind: "human", actor_id: "human://ori" },
    authority: "confirmed",
    subject_ref: "lr-002",
    data: { statement: "the endpoint needs a digest in its response" },
    provenance_refs: ["lr-002"],
    supersedes: "lr-002",
    created_at: t(4, 10),
    content_digest: "sha256:3333333333333333333333333333333333333333",
  },
  {
    id: "lr-004",
    schema_version: "nodes.culture.dev/ledger/v1alpha1",
    record_type: "evidence",
    run_id: RUN_ID,
    node_run_id: "nr-test",
    attempt_id: "att-test-1",
    origin: { kind: "runner", actor_id: "runner://headspace/docker" },
    authority: "observed",
    subject_ref: "artifact://workspace/pytest-report.xml",
    data: { process_exit: 0, workspace_diff: true },
    provenance_refs: ["att-test-1"],
    created_at: t(23, 25),
    content_digest: "sha256:4444444444444444444444444444444444444444",
  },
  {
    id: "lr-005",
    schema_version: "nodes.culture.dev/ledger/v1alpha1",
    record_type: "result",
    run_id: RUN_ID,
    node_run_id: "nr-verify",
    attempt_id: "att-verify-1",
    origin: { kind: "validator", actor_id: "validator://contracts" },
    authority: "derived",
    data: { outcome: "changes_required" },
    provenance_refs: ["lr-004"],
    created_at: t(29, 50),
    content_digest: "sha256:5555555555555555555555555555555555555555",
  },
];

export const LEDGER: LedgerRecords = {
  items: LEDGER_RECORDS,
  ledger_version: 5,
};

/** Serialize the fixture events as an SSE body, exactly as the API frames them. */
export function eventsAsSse(events: RunEvent[] = RUN_EVENTS): string {
  return (
    events
      .map(
        (item) =>
          `id: ${item.sequence}\nevent: ${item.envelope.type}\ndata: ${JSON.stringify(item.envelope)}\n\n`,
      )
      .join("")
  );
}
