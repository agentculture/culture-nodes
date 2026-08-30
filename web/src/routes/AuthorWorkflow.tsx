import { useCallback, useEffect, useMemo, useState } from "react";
import type { ChangeEvent } from "react";
import { Link } from "react-router-dom";
import {
  Background,
  Controls,
  MarkerType,
  ReactFlow,
  ReactFlowProvider,
  type BuiltInEdge,
} from "@xyflow/react";
import { DASHED } from "../culture-design/edges";
import { setAgentState } from "../agent-state/store";
import { ApiError, publishWorkflow, validateWorkflow } from "../api/client";
import type { WorkflowValidation, WorkflowVersion } from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import WorkflowNode, { type WorkflowFlowNode } from "../components/WorkflowNode";
import { NODE_HEIGHT, NODE_WIDTH, useElkLayout } from "../hooks/useElkLayout";
import { useReducedMotion } from "../hooks/useReducedMotion";
import { idleExecution } from "../domain/run-state";
import { parseWorkflowGraph } from "../domain/graph";
import {
  formatFromFilename,
  parseWorkflowSourceForPreview,
} from "../domain/workflow-source";

const NODE_TYPES = { workflow: WorkflowNode };

/**
 * The document the "Load a sample" button drops in (task t27): the minimal
 * shape a workflow can have and still compile — one agent node, one end node,
 * one edge between them.
 *
 * It is `deploy/compose/testdata/smoke.workflow.yaml` with its comments
 * stripped and its name changed, deliberately rather than a fresh document
 * invented here: that fixture is the one the compose smoke test validates,
 * publishes and runs on every check, so a sample that stops compiling is a
 * failure somebody else's suite catches first.
 *
 * `uses` names an actor reference that resolves to nothing. That is correct
 * for a sample: this page validates and publishes, it never runs anything,
 * and a sample pinned to a real actor digest would go stale the moment that
 * actor was re-registered.
 */
const SAMPLE_WORKFLOW = `apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow

metadata:
  name: sample-workflow
  version: 1.0.0
  ownerRef: team/platform-ai

spec:
  entry: work

  contract:
    input:
      schema:
        type: object
        required: [subject]
        properties:
          subject:
            type: string
    output:
      schema:
        type: object
        required: [summary]
        properties:
          summary:
            type: string

  limits:
    maxDuration: 1h
    maxTransitions: 8
    maxVisitsPerNode: 4
    maxParallelTokens: 1

  ledger:
    schemaVersion: nodes.culture.dev/ledger/v1alpha1
    maxRecordsPerNode: 10

  nodes:
    work:
      kind: agent
      ownerRef: team/platform-ai
      uses: actor://company/smoke-test@sha256:aaaaaa
      input:
        from: /run/input
      contract:
        outcomes:
          completed:
            schema:
              type: object
              required: [summary]
              properties:
                summary:
                  type: string
      ledger:
        propose: [claim, result]
      policy:
        timeout: 2m
        retry:
          maxAttempts: 1
          backoff: none

    finish:
      kind: end
      ownerRef: team/platform-ai
      output:
        from: /nodes/work/output

  edges:
    - from: work.completed
      to: finish
`;

const noop = () => {
  /* the preview canvas is read-only: nothing opens on click */
};

/**
 * The authoring slice (task t9, ADR 0007): paste or upload workflow YAML,
 * validate it against the compiler, render its diagnostics verbatim, preview
 * the graph read-only once it compiles, and publish.
 *
 * The contract this view holds itself to (ADR 0007's own words): invalid
 * YAML renders the compiler diagnostics and publishes nothing; valid YAML
 * publishes a digest identical to the CLI publish path for the same source.
 * That second half is why `source` — the operator's exact pasted/uploaded
 * string — is what `publishWorkflow` ships, never a re-serialized value: any
 * client-side re-encoding (quote style, key order, trailing newline) would
 * change the byte digest even though the *content* was unchanged.
 */
function AuthorWorkflowInner() {
  const [source, setSource] = useState("");
  const [format, setFormat] = useState<"yaml" | "json">("yaml");
  const [validating, setValidating] = useState(false);
  const [validation, setValidation] = useState<WorkflowValidation | null>(
    null,
  );
  const [validatedSource, setValidatedSource] = useState<string | null>(null);
  const [validatedFormat, setValidatedFormat] = useState<
    "yaml" | "json" | null
  >(null);
  const [publishing, setPublishing] = useState(false);
  const [published, setPublished] = useState<WorkflowVersion | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const reducedMotion = useReducedMotion();

  const toApiError = (cause: unknown): ApiError =>
    cause instanceof ApiError
      ? cause
      : new ApiError(0, String(cause), "check the browser console");

  const sourceMatchesValidation =
    validation !== null &&
    validatedSource === source &&
    validatedFormat === format;

  const step:
    | "editing"
    | "validating"
    | "invalid"
    | "valid"
    | "publishing"
    | "published" = published
    ? "published"
    : publishing
      ? "publishing"
      : validating
        ? "validating"
        : sourceMatchesValidation
          ? validation!.valid
            ? "valid"
            : "invalid"
          : "editing";

  const authoringValid = sourceMatchesValidation ? validation!.valid : null;
  const authoringDiagnosticsCount = sourceMatchesValidation
    ? validation!.diagnostics.length
    : 0;
  const authoringDigest = published?.digest ?? null;

  useEffect(() => {
    setAgentState({
      status: "ready",
      run: null,
      authoring: {
        step,
        valid: authoringValid,
        diagnostics_count: authoringDiagnosticsCount,
        digest: authoringDigest,
      },
    });
  }, [step, authoringValid, authoringDiagnosticsCount, authoringDigest]);

  const resetOutcome = () => {
    setPublished(null);
    setError(null);
  };

  const onSourceChange = (event: ChangeEvent<HTMLTextAreaElement>) => {
    setSource(event.target.value);
    resetOutcome();
  };

  const onFormatChange = (event: ChangeEvent<HTMLSelectElement>) => {
    setFormat(event.target.value as "yaml" | "json");
    resetOutcome();
  };

  const onFileChange = useCallback((event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => {
      setSource(String(reader.result ?? ""));
      setFormat(formatFromFilename(file.name));
      resetOutcome();
    };
    reader.readAsText(file);
    // Allow re-selecting the same file to trigger onChange again.
    event.target.value = "";
  }, []);

  const onLoadSample = () => {
    setSource(SAMPLE_WORKFLOW);
    setFormat("yaml");
    resetOutcome();
  };

  const onValidate = async () => {
    setValidating(true);
    setError(null);
    try {
      const result = await validateWorkflow({ format, source });
      setValidation(result);
      setValidatedSource(source);
      setValidatedFormat(format);
      setPublished(null);
    } catch (cause) {
      setValidation(null);
      setValidatedSource(null);
      setValidatedFormat(null);
      setError(toApiError(cause));
    } finally {
      setValidating(false);
    }
  };

  const onPublish = async () => {
    setPublishing(true);
    setError(null);
    try {
      // `source` — the exact pasted/uploaded string, never re-serialized —
      // is what goes over the wire; see the module docstring above.
      const version = await publishWorkflow({ format, source });
      setPublished(version);
    } catch (cause) {
      setError(toApiError(cause));
    } finally {
      setPublishing(false);
    }
  };

  const previewGraph = useMemo(() => {
    if (!sourceMatchesValidation || !validation!.valid) return null;
    const ir = parseWorkflowSourceForPreview(source, format);
    if (!ir) return null;
    return parseWorkflowGraph(ir);
  }, [sourceMatchesValidation, validation, source, format]);

  const flowNodes: WorkflowFlowNode[] = useMemo(() => {
    if (!previewGraph) return [];
    return previewGraph.nodes.map((node) => ({
      id: node.id,
      type: "workflow" as const,
      position: { x: 0, y: 0 },
      width: NODE_WIDTH,
      height: NODE_HEIGHT,
      draggable: false,
      selectable: false,
      connectable: false,
      data: {
        node,
        execution: idleExecution(node.id),
        isSelected: false,
        reducedMotion,
        onOpen: noop,
      },
    }));
  }, [previewGraph, reducedMotion]);

  const { positions } = useElkLayout(previewGraph);

  const positionedNodes = useMemo(
    () =>
      flowNodes.map((node) => ({
        ...node,
        position: positions[node.id] ?? { x: 0, y: 0 },
      })),
    [flowNodes, positions],
  );

  const flowEdges: BuiltInEdge[] = useMemo(() => {
    if (!previewGraph) return [];
    // Nothing has run yet, so every edge is DASHED (culture-design/edges.ts):
    // a path the document proposes, never one a token has actually walked.
    return previewGraph.edges.map((edge) => ({
      id: edge.id,
      source: edge.source,
      target: edge.target,
      type: "smoothstep",
      pathOptions: { borderRadius: 14, offset: 12 },
      label: edge.outcome,
      className: "flow-edge is-unwalked",
      style: {
        stroke: DASHED.stroke,
        strokeWidth: DASHED.strokeWidth,
        strokeDasharray: DASHED.strokeDasharray,
      },
      markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14 },
      ariaLabel: `edge ${edge.source} ${edge.outcome} to ${edge.target}`,
    }));
  }, [previewGraph]);

  const publishDisabled =
    publishing ||
    !sourceMatchesValidation ||
    !authoringValid ||
    source.trim() === "";

  return (
    <section className="view-rail author-workflow" id="author-workflow-view">
      <h1>New workflow</h1>
      <p className="muted">
        Paste or upload a workflow document, validate it against the
        compiler, preview its graph, then publish it as an immutable version.
      </p>

      {error ? <ErrorNotice error={error} /> : null}

      <div className="author-workflow__source">
        <div className="author-workflow__source-head">
          <label htmlFor="workflow-source-input">Workflow source</label>
          <div className="author-workflow__source-controls">
            <label htmlFor="workflow-format-select">Format</label>
            <select
              id="workflow-format-select"
              value={format}
              onChange={onFormatChange}
            >
              <option value="yaml">YAML</option>
              <option value="json">JSON</option>
            </select>
            <label className="author-workflow__upload" htmlFor="workflow-file-input">
              Upload…
            </label>
            <input
              type="file"
              id="workflow-file-input"
              accept=".yaml,.yml,.json"
              onChange={onFileChange}
            />
            {/* An empty textarea and a Validate button is a door with no
                handle — a reader who has never written this schema has
                nowhere to start (task t27). The sample compiles, so the
                first thing the page does is show them a green validate. */}
            <button
              type="button"
              id="load-sample-workflow-button"
              className="author-workflow__button"
              onClick={onLoadSample}
            >
              Load a sample
            </button>
          </div>
        </div>
        <textarea
          id="workflow-source-input"
          className="author-workflow__textarea"
          spellCheck={false}
          placeholder="apiVersion: nodes.culture.dev/v1alpha1&#10;kind: Workflow&#10;..."
          value={source}
          onChange={onSourceChange}
        />
        <div className="author-workflow__actions">
          <button
            type="button"
            id="validate-workflow-button"
            className="author-workflow__button"
            disabled={validating || source.trim() === ""}
            onClick={onValidate}
          >
            {validating ? "Validating…" : "Validate"}
          </button>
          <button
            type="button"
            id="publish-workflow-button"
            className="author-workflow__button author-workflow__button--primary"
            disabled={publishDisabled}
            onClick={onPublish}
          >
            {publishing ? "Publishing…" : "Publish"}
          </button>
        </div>
      </div>

      {sourceMatchesValidation ? (
        <div
          id="workflow-diagnostics"
          className="author-workflow__diagnostics"
          data-valid={validation!.valid}
        >
          <h2>Diagnostics</h2>
          {validation!.diagnostics.length === 0 ? (
            <p className="muted" id="workflow-diagnostics-empty">
              No diagnostics. The document compiles cleanly.
            </p>
          ) : (
            <ul className="author-workflow__diagnostics-list">
              {validation!.diagnostics.map((diagnostic, index) => (
                <li
                  key={`${diagnostic.path}-${diagnostic.code}-${index}`}
                  className={`author-workflow__diagnostic author-workflow__diagnostic--${diagnostic.level}`}
                  data-diagnostic-level={diagnostic.level}
                  data-diagnostic-code={diagnostic.code}
                  data-diagnostic-path={diagnostic.path}
                >
                  <span className="author-workflow__diagnostic-level">
                    {diagnostic.level}
                  </span>
                  <code className="author-workflow__diagnostic-path">
                    {diagnostic.path || "/"}
                  </code>
                  <p className="author-workflow__diagnostic-message">
                    {diagnostic.message}
                  </p>
                  <p className="author-workflow__diagnostic-hint">
                    <strong>hint:</strong> {diagnostic.hint}
                  </p>
                </li>
              ))}
            </ul>
          )}
        </div>
      ) : null}

      {previewGraph ? (
        <div className="author-workflow__preview">
          <h2>Preview</h2>
          <p className="muted">
            Read-only. No node here has run yet — this is the graph the
            document describes, not a live execution.
          </p>
          <div id="workflow-preview-canvas" className="run-canvas">
            <ReactFlow
              nodes={positionedNodes}
              edges={flowEdges}
              nodeTypes={NODE_TYPES}
              nodesDraggable={false}
              nodesConnectable={false}
              nodesFocusable={false}
              edgesFocusable={false}
              elementsSelectable={false}
              panOnScroll
              minZoom={0.3}
              maxZoom={2}
              fitView
              fitViewOptions={{ padding: 0.12, maxZoom: 1 }}
              proOptions={{ hideAttribution: true }}
            >
              <Background gap={28} size={1} />
              <Controls showInteractive={false} />
            </ReactFlow>
          </div>
        </div>
      ) : null}

      {published ? (
        <div id="workflow-publish-result" className="author-workflow__published">
          <h2>Published</h2>
          <p>
            <strong>{published.workflow_key}</strong> version{" "}
            {published.version}
          </p>
          <p className="author-workflow__digest">
            digest{" "}
            <code data-published-digest={published.digest}>
              {published.digest}
            </code>
          </p>
          <p>
            <Link to="/workflows">Back to workflows</Link>
          </p>
        </div>
      ) : null}
    </section>
  );
}

export function AuthorWorkflow() {
  return (
    <ReactFlowProvider>
      <AuthorWorkflowInner />
    </ReactFlowProvider>
  );
}

export default AuthorWorkflow;
