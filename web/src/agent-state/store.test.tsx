import { afterEach, describe, expect, it } from "vitest";
import { act, render } from "@testing-library/react";
import AgentStateScript from "./AgentStateScript";
import {
  getAgentState,
  resetAgentState,
  serializeAgentState,
  setAgentState,
  subscribeAgentState,
  type AgentState,
} from "./store";

afterEach(() => {
  // act(), because a mounted AgentStateScript is subscribed to this store.
  act(() => resetAgentState());
});

const readScript = (container: HTMLElement): AgentState => {
  const script = container.querySelector<HTMLScriptElement>("#agent-state");
  if (!script) throw new Error("no #agent-state element rendered");
  return JSON.parse(script.textContent ?? "") as AgentState;
};

describe("agent-state store", () => {
  it("starts in loading with no run", () => {
    expect(getAgentState()).toEqual({
      status: "loading",
      route: "/",
      run: null,
    });
  });

  it("merges patches and notifies subscribers", () => {
    let notifications = 0;
    const unsubscribe = subscribeAgentState(() => {
      notifications += 1;
    });
    setAgentState({ status: "ready" });
    setAgentState({ route: "/runs/abc" });
    expect(getAgentState()).toEqual({
      status: "ready",
      route: "/runs/abc",
      run: null,
    });
    expect(notifications).toBe(2);
    unsubscribe();
  });

  it("does not notify when nothing actually changed", () => {
    setAgentState({ status: "ready" });
    let notifications = 0;
    const unsubscribe = subscribeAgentState(() => {
      notifications += 1;
    });
    setAgentState({ status: "ready" });
    setAgentState({
      run: null,
    });
    expect(notifications).toBe(0);
    unsubscribe();
  });

  it("compares node_states by value, not by reference", () => {
    setAgentState({
      run: { id: "r1", state: "running", node_states: { a: "ready" }, selected: null },
    });
    let notifications = 0;
    const unsubscribe = subscribeAgentState(() => {
      notifications += 1;
    });
    setAgentState({
      run: { id: "r1", state: "running", node_states: { a: "ready" }, selected: null },
    });
    expect(notifications).toBe(0);
    setAgentState({
      run: { id: "r1", state: "running", node_states: { a: "active" }, selected: null },
    });
    expect(notifications).toBe(1);
    unsubscribe();
  });

  it("carries the run's name, category, and usage summary (task t5)", () => {
    setAgentState({
      run: {
        id: "r1",
        state: "running",
        node_states: {},
        selected: null,
        name: "nightly regression sweep",
        display_hint: null,
        category: "ci",
        usage: {
          input_tokens: 12300,
          output_tokens: 4100,
          cost: 0.42,
          currency: "USD",
          reported: true,
        },
      },
    });
    expect(getAgentState().run).toEqual({
      id: "r1",
      state: "running",
      node_states: {},
      selected: null,
      name: "nightly regression sweep",
      display_hint: null,
      category: "ci",
      usage: {
        input_tokens: 12300,
        output_tokens: 4100,
        cost: 0.42,
        currency: "USD",
        reported: true,
      },
    });
  });

  it("notifies when only the usage summary changes, not just node_states", () => {
    setAgentState({
      run: {
        id: "r1",
        state: "running",
        node_states: {},
        selected: null,
        usage: {
          input_tokens: 100,
          output_tokens: 50,
          cost: null,
          currency: null,
          reported: true,
        },
      },
    });
    let notifications = 0;
    const unsubscribe = subscribeAgentState(() => {
      notifications += 1;
    });
    setAgentState({
      run: {
        id: "r1",
        state: "running",
        node_states: {},
        selected: null,
        usage: {
          input_tokens: 200,
          output_tokens: 50,
          cost: null,
          currency: null,
          reported: true,
        },
      },
    });
    expect(notifications).toBe(1);
    unsubscribe();
  });

  it("distinguishes reported: false from a genuinely reported zero in equality checks", () => {
    setAgentState({
      run: {
        id: "r1",
        state: "running",
        node_states: {},
        selected: null,
        usage: {
          input_tokens: 0,
          output_tokens: 0,
          cost: null,
          currency: null,
          reported: false,
        },
      },
    });
    let notifications = 0;
    const unsubscribe = subscribeAgentState(() => {
      notifications += 1;
    });
    setAgentState({
      run: {
        id: "r1",
        state: "running",
        node_states: {},
        selected: null,
        usage: {
          input_tokens: 0,
          output_tokens: 0,
          cost: null,
          currency: null,
          reported: true,
        },
      },
    });
    expect(notifications).toBe(1);
    unsubscribe();
  });

  it("escapes < so a value can never close the script element early", () => {
    const serialized = serializeAgentState({
      status: "ready",
      route: "/runs/</script><script>alert(1)</script>",
      run: null,
    });
    expect(serialized).not.toContain("</script>");
    expect(serialized).toContain("\\u003c");
    expect(JSON.parse(serialized).route).toBe(
      "/runs/</script><script>alert(1)</script>",
    );
  });
});

describe("AgentStateScript", () => {
  it("renders exactly one #agent-state application/json node", () => {
    const { container } = render(<AgentStateScript />);
    const scripts = container.querySelectorAll("#agent-state");
    expect(scripts).toHaveLength(1);
    expect(scripts[0]).toHaveAttribute("type", "application/json");
  });

  it("serializes the current state as parseable JSON", () => {
    const { container } = render(<AgentStateScript />);
    expect(readScript(container)).toEqual({
      status: "loading",
      route: "/",
      run: null,
    });
  });

  it("tracks store updates, including the selected node", () => {
    const { container } = render(<AgentStateScript />);
    act(() => {
      setAgentState({
        status: "ready",
        route: "/runs/run-1",
        run: {
          id: "run-1",
          state: "running",
          node_states: { intake: "completed", build: "active" },
          selected: "build",
        },
      });
    });
    expect(readScript(container)).toEqual({
      status: "ready",
      route: "/runs/run-1",
      run: {
        id: "run-1",
        state: "running",
        node_states: { intake: "completed", build: "active" },
        selected: "build",
      },
    });
  });
});
