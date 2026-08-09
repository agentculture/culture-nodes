import { Handle, Position, useStore, type Node, type NodeProps } from "@xyflow/react";
import type { GraphNode } from "../domain/graph";
import type { NodeExecution } from "../domain/run-state";
import NodeCard, { bandForZoom } from "./NodeCard";

export interface WorkflowNodeData extends Record<string, unknown> {
  node: GraphNode;
  execution: NodeExecution;
  isSelected: boolean;
  reducedMotion: boolean;
  onOpen: (nodeId: string) => void;
}

export type WorkflowFlowNode = Node<WorkflowNodeData, "workflow">;

/**
 * Loop edges attach to the card's underside rather than its sides, so a
 * return path routes *below* the row instead of cutting back through it.
 * RunView names these handles on any edge domain/graph.ts flagged as a loop.
 */
export const LOOP_SOURCE_HANDLE = "loop-out";
export const LOOP_TARGET_HANDLE = "loop-in";

/**
 * React Flow adapter around NodeCard.
 *
 * The only thing this layer adds is the viewport: `useStore` reads the live
 * zoom out of React Flow's transform, which selects the §8.5 detail band.
 * React Flow's own node focus/keyboard handling is switched off on the
 * canvas (`nodesFocusable={false}`) so there is exactly one tab stop per
 * node — the card's own `role="button"` — rather than two.
 */
export function WorkflowNode({ data }: NodeProps<WorkflowFlowNode>) {
  const zoom = useStore((state) => state.transform[2]);
  const band = bandForZoom(zoom);

  return (
    <>
      <Handle type="target" position={Position.Left} isConnectable={false} />
      <NodeCard
        node={data.node}
        execution={data.execution}
        band={band}
        selected={data.isSelected}
        reducedMotion={data.reducedMotion}
        onOpen={data.onOpen}
      />
      <Handle type="source" position={Position.Right} isConnectable={false} />
      <Handle
        type="source"
        id={LOOP_SOURCE_HANDLE}
        position={Position.Bottom}
        isConnectable={false}
      />
      <Handle
        type="target"
        id={LOOP_TARGET_HANDLE}
        position={Position.Bottom}
        isConnectable={false}
      />
    </>
  );
}

export default WorkflowNode;
