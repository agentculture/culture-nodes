import { afterEach, describe, expect, it } from "vitest";
import type { AgentMeshState } from "./store";
import {
  getAgentState,
  resetAgentState,
  serializeAgentState,
  setAgentState,
  subscribeAgentState,
} from "./store";

/**
 * The mesh block of the agent-state mirror (task t18): it must serialize
 * completely (a canvas is untestable otherwise), stay absent from every
 * other view's payload, and not churn the `<script>` node when nothing
 * actually changed.
 */

const MESH: AgentMeshState = {
  machine_count: 2,
  actor_count: 3,
  run_count: 2,
  edge_count: 5,
  probe_failures: 1,
  unattributed_actors: 1,
  connection: "live",
  last_event_id: "01ULID",
  events_total: 7,
  pulses_total: 4,
  reduced_motion: false,
};

afterEach(() => {
  resetAgentState();
});

describe("agent-state mesh block", () => {
  it("is absent from the serialized state until a mesh view sets it", () => {
    const parsed = JSON.parse(serializeAgentState(getAgentState()));
    expect("mesh" in parsed).toBe(false);
  });

  it("round-trips every field through serialization", () => {
    setAgentState({ mesh: MESH });
    const parsed = JSON.parse(serializeAgentState(getAgentState()));
    expect(parsed.mesh).toEqual(MESH);
  });

  it("does not notify listeners for an identical mesh patch", () => {
    setAgentState({ mesh: MESH });
    let notified = 0;
    const unsubscribe = subscribeAgentState(() => {
      notified += 1;
    });
    setAgentState({ mesh: { ...MESH } });
    expect(notified).toBe(0);
    setAgentState({ mesh: { ...MESH, pulses_total: 5 } });
    expect(notified).toBe(1);
    unsubscribe();
  });

  it("drops out of the payload when cleared with undefined", () => {
    setAgentState({ mesh: MESH });
    setAgentState({ mesh: undefined });
    const parsed = JSON.parse(serializeAgentState(getAgentState()));
    expect("mesh" in parsed).toBe(false);
  });
});
