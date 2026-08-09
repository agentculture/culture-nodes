import { useSyncExternalStore } from "react";
import {
  getAgentState,
  serializeAgentState,
  subscribeAgentState,
} from "./store";

/**
 * Renders the agent-state node. Mounted once, from the root component, so
 * there is exactly one `#agent-state` element on the page — webglass's
 * `--selector '#agent-state'` must match one element, never a list.
 *
 * `type="application/json"` is a data block: the browser never executes it,
 * and React 18 renders it inline rather than hoisting it to <head>.
 */
export function AgentStateScript() {
  const state = useSyncExternalStore(
    subscribeAgentState,
    getAgentState,
    getAgentState,
  );

  return (
    <script
      type="application/json"
      id="agent-state"
      data-testid="agent-state"
      dangerouslySetInnerHTML={{ __html: serializeAgentState(state) }}
    />
  );
}

export default AgentStateScript;
