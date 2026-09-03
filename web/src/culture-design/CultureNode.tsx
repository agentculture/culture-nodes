import type { KeyboardEvent } from "react";
import type { GraphNode } from "../domain/graph";
import { NODE_STATE_LABEL, type NodeExecution } from "../domain/run-state";
import StatusChip from "../components/StatusChip";
import "./node.css";

export type DetailBand = "far" | "medium" | "close";

export function bandForZoom(zoom: number): DetailBand {
  if (zoom < 0.45) return "far";
  if (zoom < 1.05) return "medium";
  return "close";
}

export interface CultureNodeProps {
  node: GraphNode;
  band?: DetailBand;
  execution?: NodeExecution;
  selected?: boolean;
  live?: boolean;
  pulseCount?: number;
  motion?: "animated" | "paused" | "static";
  /** Compatibility input for callers being migrated to the motion band. */
  reducedMotion?: boolean;
  onOpen?: (nodeId: string) => void;
  hasEvidence?: boolean;
}

function shortRef(ref: string | undefined): string | undefined {
  if (!ref) return undefined;
  const at = ref.indexOf("@");
  if (at < 0) return ref.replace(/^[a-z]+:\/\//, "");
  return `${ref.slice(0, at).replace(/^[a-z]+:\/\//, "")}@${ref.slice(at + 1, at + 15)}…`;
}

/** The single token-driven Culture node, independent of React Flow. */
export function CultureNode({
  node,
  band = "medium",
  execution,
  selected = false,
  live = execution?.state === "active",
  pulseCount = execution?.state === "active" ? execution.attempts.length : 0,
  motion = "animated",
  reducedMotion,
  onOpen,
  hasEvidence = false,
}: CultureNodeProps) {
  const effectiveMotion = reducedMotion ? "static" : motion;
  const state = execution?.state;
  const interactive = Boolean(onOpen);
  const subLine = band === "far" ? undefined : node.ownerRef ?? node.kind;
  const classes = [
    "culture-node",
    "node-card active-node",
    state ? `node-card--${state}` : "",
    `node-card--band-${band}`,
    live ? "is-live" : "",
    selected ? "is-selected is-inspected" : "",
  ].filter(Boolean).join(" ");
  const label = state
    ? `node ${node.id}, ${node.kind}, ${NODE_STATE_LABEL[state]}`
    : `node ${node.id}, ${node.kind}${live ? ", active" : ""}`;
  const lastAttempt = execution?.attempts[execution.attempts.length - 1];
  const activate = () => onOpen?.(node.id);
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (!interactive || (event.key !== "Enter" && event.key !== " ")) return;
    event.preventDefault();
    event.stopPropagation();
    activate();
  };

  return (
    <div
      className={classes}
      data-kind={node.kind}
      data-node-id={node.id}
      data-node-live={live ? "true" : "false"}
      data-node-state={state}
      data-node-evidence={hasEvidence ? "true" : "false"}
      data-band={band}
      data-motion={effectiveMotion}
      role={interactive ? "button" : undefined}
      tabIndex={interactive ? 0 : undefined}
      aria-label={label}
      aria-pressed={interactive ? selected : undefined}
      onClick={interactive ? activate : undefined}
      onKeyDown={onKeyDown}
    >
      <span className="culture-node__halo" aria-hidden="true" />
      <span className="culture-node__core node-card__dot" aria-hidden="true" />
      <div className="culture-node__label node-card__name">{node.id}</div>
      {subLine ? <div className="culture-node__sub node-card__meta"><span>{node.kind}</span>{node.ownerRef ? <span className="node-card__owner" title={node.ownerRef}>{node.ownerRef}</span> : null}</div> : null}

      {execution && band !== "far" ? (
        <div className="culture-node__state-row node-card__status">
          <StatusChip state={execution.state} />
          {execution.visits > 1 ? <span className="node-card__visits">visit {execution.visits}</span> : null}
          {state === "waiting" ? <span className="node-card__badge">awaiting signal</span> : null}
          {live && effectiveMotion === "static" ? <span className="node-card__badge node-card__badge--live">attempt in flight</span> : null}
          {hasEvidence ? <span className="node-card__badge node-card__badge--evidence" title="this node has measured workspace evidence"><span aria-hidden="true">◈</span> evidence</span> : null}
        </div>
      ) : band === "far" && (state === "failed" || state === "policy_denied") ? <StatusChip state={state} className="status-chip--compact" /> : live && band !== "far" ? <span className="culture-node__state active-node__badge"><span aria-hidden="true">●</span> active</span> : null}

      {execution && band === "close" ? (
        <dl className="node-card__detail">
          <div><dt>attempts</dt><dd>{execution.attempts.length}{lastAttempt ? ` (${lastAttempt.status})` : ""}</dd></div>
          {execution.actorId ?? node.uses ? <div><dt>{node.kind === "code" ? "runner" : "actor"}</dt><dd>{execution.actorId ?? shortRef(node.uses)}</dd></div> : null}
          {node.raw.operation?.image ? <div><dt>image</dt><dd>{shortRef(node.raw.operation.image)}</dd></div> : null}
          {execution.outcome ? <div><dt>outcome</dt><dd>{execution.outcome}</dd></div> : null}
        </dl>
      ) : null}
      {pulseCount > 0 && effectiveMotion !== "static" ? <span key={pulseCount} className="culture-node__pulse active-node__pulse" data-pulse-count={pulseCount} aria-hidden="true" /> : null}
    </div>
  );
}

export default CultureNode;
