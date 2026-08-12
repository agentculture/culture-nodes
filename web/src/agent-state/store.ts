/**
 * The agent-state store — the machine-readable mirror of what this page is
 * currently showing.
 *
 * A UI that only says what it is doing in pixels is untestable by an agent.
 * This store is serialized into a single
 * `<script type="application/json" id="agent-state">` node, which an agent
 * (or the webglass CI job in .github/workflows/web.yml) reads with one
 * selector-scoped extract instead of scraping the DOM:
 *
 *     webglass page extract --selector '#agent-state' --json \
 *       | jq -r '.content.untrusted.matches[0].text' | jq .status
 *
 * webglass selectors are tag / #id / .class / [attr] only, so every
 * assertable element in this app carries a stable `id` or a `data-` attribute
 * that matches exactly one element.
 */

export type AgentStatus = "loading" | "ready";

/**
 * The token-first §13.2 usage rollup, mirrored for the machine-readable
 * state exactly the way UsageSummary.tsx renders it (task t5, honesty
 * condition h27): `reported: false` means "no attempt reported usage",
 * distinct from `cost`/`currency` both being `null` (which only means no
 * cost was reported, alongside real, present token totals). `null` rather
 * than `undefined` throughout, because `undefined` values are dropped by
 * `JSON.stringify` and this state is read back out of serialized JSON.
 */
export interface AgentUsageSummary {
  input_tokens: number;
  output_tokens: number;
  cost: number | null;
  currency: string | null;
  reported: boolean;
}

export interface AgentRunState {
  id: string;
  state: string;
  /** nodeId -> execution state, exactly as the canvas renders it. */
  node_states: Record<string, string>;
  /** The node whose detail panel is open, or null. */
  selected: string | null;
  /**
   * The run's own name/hint/category (task t5), mirroring
   * api/types.ts's `Run` fields rather than pre-resolving them, so a reader
   * of `#agent-state` can tell a real operator-given name apart from a
   * derived `display_hint` the same way the UI visually does (never present
   * a hint as if it were a given name). Both `null` when the run carries
   * neither. Optional and omitted by every route that isn't the Run view,
   * so this adds nothing to `#agent-state` anywhere else (same convention
   * as `authoring`).
   */
  name?: string | null;
  display_hint?: string | null;
  category?: string | null;
  usage?: AgentUsageSummary | null;
}

/**
 * The authoring view's machine-readable mirror (task t9): which step the
 * paste/upload -> validate -> preview -> publish flow is on, right now.
 * Optional and absent from every other view's state (undefined keys are
 * dropped by `JSON.stringify`, so this adds nothing to the `#agent-state`
 * payload anywhere else).
 */
export interface AgentAuthoringState {
  step: "editing" | "validating" | "invalid" | "valid" | "publishing" | "published";
  /** `null` before the current source has ever been validated. */
  valid: boolean | null;
  diagnostics_count: number;
  /** The published version's content digest, once publish succeeds. */
  digest: string | null;
}

export interface AgentState {
  status: AgentStatus;
  route: string;
  run: AgentRunState | null;
  authoring?: AgentAuthoringState | null;
}

const INITIAL: AgentState = { status: "loading", route: "/", run: null };

let current: AgentState = INITIAL;
const listeners = new Set<() => void>();

export function getAgentState(): AgentState {
  return current;
}

export function subscribeAgentState(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function shallowEqualUsage(
  a: AgentUsageSummary | null | undefined,
  b: AgentUsageSummary | null | undefined,
): boolean {
  if (a === b) return true;
  if (!a || !b) return false;
  return (
    a.input_tokens === b.input_tokens &&
    a.output_tokens === b.output_tokens &&
    a.cost === b.cost &&
    a.currency === b.currency &&
    a.reported === b.reported
  );
}

function shallowEqualRun(
  a: AgentRunState | null,
  b: AgentRunState | null,
): boolean {
  if (a === b) return true;
  if (!a || !b) return false;
  if (
    a.id !== b.id ||
    a.state !== b.state ||
    a.selected !== b.selected ||
    (a.name ?? null) !== (b.name ?? null) ||
    (a.display_hint ?? null) !== (b.display_hint ?? null) ||
    (a.category ?? null) !== (b.category ?? null) ||
    !shallowEqualUsage(a.usage, b.usage)
  ) {
    return false;
  }
  const aKeys = Object.keys(a.node_states);
  const bKeys = Object.keys(b.node_states);
  if (aKeys.length !== bKeys.length) return false;
  return aKeys.every((key) => a.node_states[key] === b.node_states[key]);
}

function shallowEqualAuthoring(
  a: AgentAuthoringState | null | undefined,
  b: AgentAuthoringState | null | undefined,
): boolean {
  if (a === b) return true;
  if (!a || !b) return false;
  return (
    a.step === b.step &&
    a.valid === b.valid &&
    a.diagnostics_count === b.diagnostics_count &&
    a.digest === b.digest
  );
}

/**
 * Merge a patch into the agent state. No-ops when nothing actually changed,
 * so a re-render storm cannot make the `<script>` node churn.
 */
export function setAgentState(patch: Partial<AgentState>): void {
  const next: AgentState = { ...current, ...patch };
  if (
    next.status === current.status &&
    next.route === current.route &&
    shallowEqualRun(next.run, current.run) &&
    shallowEqualAuthoring(next.authoring, current.authoring)
  ) {
    return;
  }
  current = next;
  for (const listener of listeners) listener();
}

/** Test seam: drop back to the initial state. */
export function resetAgentState(): void {
  current = INITIAL;
  for (const listener of listeners) listener();
}

/**
 * Serialize for embedding inside a `<script>` element. `<` is escaped so a
 * value containing `</script>` cannot close the element early — the state is
 * built from API data, and API data is never trusted to be inert markup.
 */
export function serializeAgentState(state: AgentState): string {
  return JSON.stringify(state, null, 2).replace(/</g, "\\u003c");
}
