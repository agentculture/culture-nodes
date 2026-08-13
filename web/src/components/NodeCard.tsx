import type { KeyboardEvent } from "react";
import type { GraphNode } from "../domain/graph";
import { accentFor } from "../domain/graph";
import { NODE_STATE_LABEL, type NodeExecution } from "../domain/run-state";
import StatusChip from "./StatusChip";

/**
 * Zoom bands, PRD §8.5. Which facts a card shows is a function of how much
 * room the viewport is giving it — not a user preference and not a toggle.
 */
export type DetailBand = "far" | "medium" | "close";

/**
 * The `far` threshold sits below the zoom a fit-to-view of a first-slice
 * workflow settles at, so opening a run lands in `medium` — name, kind,
 * state, owner — rather than in topology-only. Zooming out past it is a
 * deliberate move to the wider read.
 */
export function bandForZoom(zoom: number): DetailBand {
  if (zoom < 0.45) return "far";
  if (zoom < 1.05) return "medium";
  return "close";
}

export interface NodeCardProps {
  node: GraphNode;
  execution: NodeExecution;
  band: DetailBand;
  selected: boolean;
  reducedMotion: boolean;
  onOpen: (nodeId: string) => void;
  /**
   * Whether ANY of this node's node runs has an evidence-type ledger record
   * attached (task t11 acceptance #3: the run view must make "open node →
   * see evidence" discoverable without opening every node first). Computed
   * by RunView from data it already has fetched — the run's ledger,
   * joined back to this node's node runs — never a separate API call, so
   * absence of the flag (`undefined`/`false`) is never rendered as a claim
   * that no evidence exists, only that none is known from here.
   */
  hasEvidence?: boolean;
}

function shortRef(ref: string | undefined): string | undefined {
  if (!ref) return undefined;
  // actor://company/intake@sha256:111111 -> company/intake@sha256:1111…
  const at = ref.indexOf("@");
  if (at < 0) return ref.replace(/^[a-z]+:\/\//, "");
  const head = ref.slice(0, at).replace(/^[a-z]+:\/\//, "");
  const digest = ref.slice(at + 1);
  return `${head}@${digest.slice(0, 14)}…`;
}

/**
 * The workflow node card — the shared Culture node frame (surface, --line,
 * --radius, --shadow from culture-design/tokens.css) plus kind-specific
 * content (PRD §8.3), overlaid with live execution state (§8.4).
 *
 * This component is deliberately free of React Flow: it takes a band and a
 * plain execution record, so it can be rendered and asserted on without a
 * canvas. WorkflowNode.tsx is the thin React Flow adapter around it.
 */
export function NodeCard({
  node,
  execution,
  band,
  selected,
  reducedMotion,
  onOpen,
  hasEvidence = false,
}: NodeCardProps) {
  const accent = accentFor(node.kind);
  const state = execution.state;
  const pulsing = state === "active" && !reducedMotion;

  const classes = [
    "node-card",
    `node-card--${state}`,
    `node-card--band-${band}`,
    selected ? "is-selected" : "",
    pulsing ? "is-pulse" : "",
  ]
    .filter(Boolean)
    .join(" ");

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      event.stopPropagation();
      onOpen(node.id);
    }
  };

  const label = `node ${node.id}, ${node.kind}, ${NODE_STATE_LABEL[state]}`;
  const lastAttempt = execution.attempts[execution.attempts.length - 1];

  return (
    <div
      className={classes}
      data-node-id={node.id}
      data-node-state={state}
      data-band={band}
      data-node-evidence={hasEvidence ? "true" : "false"}
      role="button"
      tabIndex={0}
      aria-label={label}
      aria-pressed={selected}
      style={{ ["--node-accent" as string]: accent }}
      onClick={() => onOpen(node.id)}
      onKeyDown={onKeyDown}
    >
      <span className="node-card__rail" aria-hidden="true" />
      <div className="node-card__head">
        <span className="node-card__dot" aria-hidden="true" />
        <span className="node-card__name">{node.id}</span>
      </div>

      {band === "far" ? (
        // Distant zoom shows topology, flow, failure and ownership only.
        state === "failed" || state === "policy_denied" ? (
          <StatusChip state={state} className="status-chip--compact" />
        ) : null
      ) : (
        <>
          <div className="node-card__meta">
            <span className="node-card__kind">{node.kind}</span>
            {node.ownerRef ? (
              <span className="node-card__owner" title={node.ownerRef}>
                {node.ownerRef}
              </span>
            ) : null}
          </div>
          <div className="node-card__status">
            <StatusChip state={state} />
            {execution.visits > 1 ? (
              <span className="node-card__visits">visit {execution.visits}</span>
            ) : null}
            {state === "waiting" ? (
              <span className="node-card__badge">awaiting signal</span>
            ) : null}
            {state === "active" && reducedMotion ? (
              // The pulse carries "an attempt is in flight". With motion off
              // that information has to be text, not an absent animation.
              <span className="node-card__badge node-card__badge--live">
                attempt in flight
              </span>
            ) : null}
            {hasEvidence ? (
              // Discoverability for the ship-review pause (task t11 #3): a
              // reader scanning the graph sees which nodes carry measured
              // workspace evidence before opening any of them.
              <span
                className="node-card__badge node-card__badge--evidence"
                title="this node has measured workspace evidence"
              >
                <span aria-hidden="true">◈</span> evidence
              </span>
            ) : null}
          </div>
        </>
      )}

      {band === "close" ? (
        <dl className="node-card__detail">
          <div>
            <dt>attempts</dt>
            <dd>
              {execution.attempts.length}
              {lastAttempt ? ` (${lastAttempt.status})` : ""}
            </dd>
          </div>
          {execution.actorId ?? node.uses ? (
            <div>
              <dt>{node.kind === "code" ? "runner" : "actor"}</dt>
              <dd>{execution.actorId ?? shortRef(node.uses)}</dd>
            </div>
          ) : null}
          {node.raw.operation?.image ? (
            <div>
              <dt>image</dt>
              <dd>{shortRef(node.raw.operation.image)}</dd>
            </div>
          ) : null}
          {execution.outcome ? (
            <div>
              <dt>outcome</dt>
              <dd>{execution.outcome}</dd>
            </div>
          ) : null}
        </dl>
      ) : null}
    </div>
  );
}

export default NodeCard;
