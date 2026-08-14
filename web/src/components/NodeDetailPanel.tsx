import { useEffect, useRef, type KeyboardEvent } from "react";
import type { LedgerRecord, Usage } from "../api/types";
import { parseWorkspaceEvidence } from "../domain/evidence";
import {
  isMachineCheckableSignal,
  NOT_MACHINE_CHECKABLE,
} from "../domain/success-signal";
import type { GraphNode } from "../domain/graph";
import { NODE_STATE_LABEL, type NodeExecution } from "../domain/run-state";
import { preserveBranchInfo } from "../domain/preserve";
import AuthorityChip from "./AuthorityChip";
import StatusChip from "./StatusChip";
import UsageSummary from "./UsageSummary";

/**
 * Task t26 (issue #49, spec claim c32 / honesty h21): an operator-set forge
 * URL template for linking a PUSHED preserve branch, e.g.
 * `https://github.com/org/repo/tree/{branch}` — `{branch}` is substituted
 * with the branch name. Read once at module load, the same way vite.config.ts
 * reads `NODES_API` from the environment. Unset by default: with no
 * template configured, a pushed branch still renders (as plain text, plus
 * "pushed to <remote>"), it is simply not a clickable link — see
 * web/README.md. Never derived from `preserve_remote`: a link may only come
 * from configuration the operator actually set, never a guess from a
 * remote's name.
 */
const PRESERVE_BRANCH_URL_TEMPLATE = import.meta.env
  .VITE_PRESERVE_BRANCH_URL_TEMPLATE;

export interface NodeDetailPanelProps {
  node: GraphNode;
  execution: NodeExecution;
  ledger: LedgerRecord[];
  onClose: () => void;
  /**
   * This node's usage/cost rollup (task t2/t5), merged across every one of
   * its node runs (a loop revisits a node more than once). `undefined` —
   * distinct from a `Usage` with `attempts_reported: 0` — means the
   * best-effort node-runs join (useRunData.ts) found no entry at all for
   * this node run, an honest "not available" rather than "not reported".
   */
  usage?: Usage;
}

function clockTime(iso: string): string {
  const match = /T(\d{2}:\d{2}:\d{2})/.exec(iso);
  return match ? match[1] : iso;
}

function duration(startedAt?: string, completedAt?: string): string {
  if (!startedAt || !completedAt) return "—";
  const ms = new Date(completedAt).getTime() - new Date(startedAt).getTime();
  if (Number.isNaN(ms)) return "—";
  if (ms < 1000) return `${ms} ms`;
  return `${(ms / 1000).toFixed(1)} s`;
}

/**
 * The node-detail side panel (PRD §8.7: "node detail showing actor or
 * runner, contract, owner, attempt, and ledger delta").
 *
 * It is a zoom-independent panel — §8.8's "zoom-independent readable detail
 * panels": the canvas may be zoomed all the way out, and this still reads at
 * full size. Focus moves here on open; Escape closes and the caller returns
 * focus to the node that opened it.
 */
export function NodeDetailPanel({
  node,
  execution,
  ledger,
  onClose,
  usage,
}: NodeDetailPanelProps) {
  const panelRef = useRef<HTMLElement>(null);

  useEffect(() => {
    panelRef.current?.focus();
  }, [node.id]);

  const onKeyDown = (event: KeyboardEvent<HTMLElement>) => {
    if (event.key === "Escape") {
      event.stopPropagation();
      onClose();
    }
  };

  const nodeRunIds = new Set(execution.nodeRuns.map((run) => run.id));
  const delta = ledger.filter(
    (record) => record.node_run_id && nodeRunIds.has(record.node_run_id),
  );
  const evidence = delta.filter((record) => record.record_type === "evidence");
  const contract = node.raw.contract;
  const contractDigest =
    contract?.input?.digest ??
    Object.values(contract?.outcomes ?? {})[0]?.digest;

  return (
    <aside
      id="node-detail-panel"
      className="detail-panel"
      ref={panelRef}
      tabIndex={-1}
      role="region"
      aria-label={`Node detail: ${node.id}`}
      data-node-id={node.id}
      onKeyDown={onKeyDown}
    >
      <div className="detail-panel__head">
        <h2 className="detail-panel__title">{node.id}</h2>
        <button
          type="button"
          id="node-detail-close"
          className="detail-panel__close"
          onClick={onClose}
        >
          Close
        </button>
      </div>

      <p className="detail-panel__state">
        <StatusChip state={execution.state} id="node-detail-state-chip" />
        <span className="sr-only">
          {`node ${node.id} is ${NODE_STATE_LABEL[execution.state]}`}
        </span>
      </p>

      <dl className="detail-panel__facts">
        <div>
          <dt>kind</dt>
          <dd>{node.kind}</dd>
        </div>
        <div>
          <dt>owner</dt>
          <dd>{node.ownerRef ?? "—"}</dd>
        </div>
        <div>
          <dt>{node.kind === "code" ? "runner" : "actor"}</dt>
          <dd>
            <code>{execution.actorId ?? node.uses ?? "—"}</code>
          </dd>
        </div>
        <div>
          <dt>contract digest</dt>
          <dd>
            <code id="node-detail-contract-digest">
              {contractDigest ??
                contract?.input?.schemaRef ??
                "not pinned in this IR"}
            </code>
          </dd>
        </div>
        <div>
          <dt>outcomes</dt>
          <dd>{node.outcomes.length > 0 ? node.outcomes.join(", ") : "—"}</dd>
        </div>
        <div>
          <dt>visits</dt>
          <dd>{execution.visits}</dd>
        </div>
        {node.raw.operation?.image ? (
          <div>
            <dt>image</dt>
            <dd>
              <code>{node.raw.operation.image}</code>
            </dd>
          </div>
        ) : null}
        {node.raw.policy?.retry?.maxAttempts ? (
          <div>
            <dt>max attempts</dt>
            <dd>{node.raw.policy.retry.maxAttempts}</dd>
          </div>
        ) : null}
      </dl>

      <h3>Attempts</h3>
      {execution.attempts.length === 0 ? (
        <p className="muted">No attempt has run on this node yet.</p>
      ) : (
        <div className="detail-panel__scroll">
          <table className="detail-panel__attempts" id="node-detail-attempts">
            <thead>
              <tr>
                <th scope="col">#</th>
                <th scope="col">status</th>
                <th scope="col">actor</th>
                <th scope="col">started</th>
                <th scope="col">duration</th>
                <th scope="col">preserve</th>
              </tr>
            </thead>
            <tbody>
              {execution.attempts.map((attempt) => {
                const preserve = preserveBranchInfo(
                  attempt,
                  PRESERVE_BRANCH_URL_TEMPLATE,
                );
                return (
                  <tr key={attempt.id} data-attempt-id={attempt.id}>
                    <th scope="row">{attempt.attempt_number}</th>
                    <td data-attempt-status={attempt.status}>{attempt.status}</td>
                    <td>
                      <code>{attempt.actor_id ?? "—"}</code>
                    </td>
                    <td>
                      {/* Clock time in the cell, the full instant in the title
                          and in dateTime — the panel is narrow, the record is
                          not truncated. */}
                      <time dateTime={attempt.started_at} title={attempt.started_at}>
                        {clockTime(attempt.started_at)}
                      </time>
                    </td>
                    <td>{duration(attempt.started_at, attempt.completed_at)}</td>
                    <td data-preserve-branch={preserve?.branch}>
                      {preserve ? (
                        <span
                          className="detail-panel__preserve"
                          data-preserve-status={
                            preserve.pushed ? "pushed" : "local-only"
                          }
                        >
                          {preserve.href ? (
                            <a
                              href={preserve.href}
                              target="_blank"
                              rel="noreferrer"
                              className="detail-panel__preserve-link"
                            >
                              <code>{preserve.branch}</code>
                            </a>
                          ) : (
                            <code>{preserve.branch}</code>
                          )}
                          <span className="detail-panel__preserve-status">
                            {preserve.pushed
                              ? `↗ pushed${preserve.remote ? ` to ${preserve.remote}` : ""}`
                              : "⌁ local-only"}
                          </span>
                        </span>
                      ) : (
                        "—"
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      <h3>Usage</h3>
      {usage ? (
        <UsageSummary usage={usage} id="node-detail-usage" />
      ) : (
        <p className="muted" id="node-detail-usage-unavailable">
          Usage data is not available for this node run.
        </p>
      )}

      <h3>Ledger delta</h3>
      {delta.length === 0 ? (
        <p className="muted">This node run has appended no ledger records.</p>
      ) : (
        <ul className="detail-panel__ledger" id="node-detail-ledger">
          {delta.map((record) => (
            <li key={record.id} data-record-id={record.id}>
              <AuthorityChip authority={record.authority} />
              <span className="detail-panel__record-type">
                {record.record_type}
              </span>
              {/* A success_signal whose author did not declare it mechanical
                  will never get a machine verdict (task t18) — say so, so it
                  is not mistaken for one whose evaluation just hasn't landed. */}
              {record.record_type === "success_signal" &&
              !isMachineCheckableSignal(record.data) ? (
                <span
                  className="detail-panel__signal-checkability"
                  data-signal-checkability="not-machine-checkable"
                >
                  {NOT_MACHINE_CHECKABLE}
                </span>
              ) : null}
              <code className="detail-panel__digest">
                {record.content_digest.slice(0, 20)}…
              </code>
            </li>
          ))}
        </ul>
      )}

      <h3>Evidence</h3>
      {evidence.length === 0 ? (
        <p className="muted">
          No observed evidence is attached to this node run.
        </p>
      ) : (
        <ul className="detail-panel__evidence" id="node-detail-evidence">
          {evidence.map((record) => {
            // Additive, shape-driven rendering (task t11): every evidence
            // record still gets the generic authority/subject/provenance
            // line above unconditionally; a record whose `data` is shaped
            // like task t12's workspace-snapshot hook evidence (see
            // domain/evidence.ts) ALSO gets the structured block below.
            // Evidence whose data does not match that shape — e.g. the
            // `test` node's runner exit-status evidence — renders exactly
            // as it did before this task, no regression.
            const workspace = parseWorkspaceEvidence(record.data);
            return (
              <li key={record.id} data-record-id={record.id}>
                <AuthorityChip authority={record.authority} />
                <code>{record.subject_ref ?? record.id}</code>
                {record.provenance_refs.length > 0 ? (
                  <span className="detail-panel__provenance">
                    ← {record.provenance_refs.join(", ")}
                  </span>
                ) : null}
                {workspace ? (
                  <div
                    className="detail-panel__workspace-evidence"
                    data-workspace-evidence="true"
                  >
                    {workspace.changedPaths ? (
                      <div
                        className="detail-panel__changed-paths"
                        data-evidence-changed-paths="true"
                      >
                        <h4>Changed files</h4>
                        <ul>
                          {workspace.changedPaths.map((path) => (
                            <li key={path}>
                              <code>{path}</code>
                            </li>
                          ))}
                        </ul>
                      </div>
                    ) : null}
                    {workspace.snapshotDigest ? (
                      <p className="detail-panel__snapshot-digest">
                        snapshot{" "}
                        <code
                          className="evidence-chip"
                          data-evidence-snapshot-digest="true"
                        >
                          {workspace.snapshotDigest}
                        </code>
                      </p>
                    ) : null}
                    {workspace.artifactRefs ? (
                      <ul
                        className="detail-panel__artifact-refs"
                        data-evidence-artifact-refs="true"
                      >
                        {workspace.artifactRefs.map((ref) => (
                          <li key={ref}>
                            <a href={ref} className="detail-panel__artifact-ref">
                              <code>{ref}</code>
                            </a>
                          </li>
                        ))}
                      </ul>
                    ) : null}
                    {workspace.diffstat ? (
                      <pre
                        className="detail-panel__diffstat"
                        data-evidence-diffstat="true"
                      >
                        {workspace.diffstat}
                      </pre>
                    ) : null}
                  </div>
                ) : null}
              </li>
            );
          })}
        </ul>
      )}
    </aside>
  );
}

export default NodeDetailPanel;
