/**
 * Fixture data for the Declarations view (task t22, #328): a small,
 * hand-built `DeclarationFocus` payload shaped like
 * `internal/api/declgraph.go`'s response at distance 2, `direction=both`,
 * `link=both` -- `build` at the center (distance 0), `test` a direct
 * neighbour (distance 1, a `must` predecessor), `publish` a direct
 * neighbour (distance 1, a `can` successor), and `notify` two hops out
 * (distance 2, downstream of `publish`).
 *
 * `FOCUS_AT` slices this same graph down to a given distance so both the
 * domain tests and the route's component tests can assert the N=0/1/2
 * cases against ONE underlying dataset, matching what
 * `resolveFocusCenters`'s BFS in the real server actually returns for a
 * growing `distance` query.
 */

import type { DeclarationFocus, DeclarationFocusDeclaration } from "../api/types";

function declaration(
  name: string,
  distance: number,
  triggerKind: string,
  actionKind: string,
): DeclarationFocusDeclaration {
  return {
    name,
    declaration_id: `decl-${name}`,
    version: 1,
    digest: `sha256:${name.repeat(8).slice(0, 64)}`,
    distance,
    trigger_kind: triggerKind,
    action_kind: actionKind,
    start_node: `${name}.waiting`,
    landing_node: `${name}.landed`,
  };
}

export const BUILD = declaration("build", 0, "pr_opened", "run_workflow");
export const TEST = declaration("test", 1, "artifact_ready", "run_tests");
export const PUBLISH = declaration("publish", 1, "tests_passed", "publish_artifact");
export const NOTIFY = declaration("notify", 2, "artifact_published", "notify_channel");

/** The full distance-2 focus, exactly as `GET .../build/focus?distance=2` would answer. */
export const FOCUS_DISTANCE_2: DeclarationFocus = {
  center: "build",
  distance: 2,
  direction: "both",
  link: "both",
  declarations: [BUILD, TEST, PUBLISH, NOTIFY],
  nodes: [BUILD, TEST, PUBLISH, NOTIFY].flatMap((d) => [
    { declaration: d.name, role: "start" as const, name: d.start_node, deadline: "" },
    { declaration: d.name, role: "landing" as const, name: d.landing_node, deadline: "" },
  ]),
  links: [
    { from: "test", to: "build", kind: "must" },
    { from: "build", to: "publish", kind: "can" },
    { from: "publish", to: "notify", kind: "must" },
  ],
};

/** The same center, sliced to a smaller `distance` -- what the server itself would return. */
export function focusAtDistance(distance: 0 | 1 | 2): DeclarationFocus {
  const declarations = FOCUS_DISTANCE_2.declarations.filter(
    (d) => d.distance <= distance,
  );
  const present = new Set(declarations.map((d) => d.name));
  return {
    ...FOCUS_DISTANCE_2,
    distance,
    declarations,
    nodes: FOCUS_DISTANCE_2.nodes.filter((n) => present.has(n.declaration)),
    links: FOCUS_DISTANCE_2.links.filter(
      (l) => present.has(l.from) && present.has(l.to),
    ),
  };
}

export const FOCUS_DISTANCE_0 = focusAtDistance(0);
export const FOCUS_DISTANCE_1 = focusAtDistance(1);
