package worker

import (
	"encoding/json"
)

// Applying a run's recorded actor affinity at dispatch (issue #107, task
// t33).
//
// The whole override happens at ONE seam -- in Worker.dispatch, immediately
// after the node is resolved from the pinned definition and before anything
// reads node.Uses. That placement is the design decision, because node.Uses
// is read in a dozen places downstream: the registry lookup, the dispatch
// pacing budget, the capacity breaker, the economic session budget, the
// preflight capability gate, the telemetry attribute, and the actor id
// stamped on the attempt. Overriding at the registry lookup alone would route
// the request to the affinity-chosen actor while charging the pacing budget,
// tripping the breaker, and attributing the attempt to the declared one --
// which would corrupt precisely the per-actor comparative record this feature
// exists to feed. One seam, applied before the first read, keeps all of them
// consistent by construction.

// runAffinity is the decoded shape of runs.actor_affinity (migration 0034).
type runAffinity map[string]struct {
	Actor string `json:"actor"`
	Rule  string `json:"rule"`
}

// applyAffinity returns the node this dispatch should use: the pinned one
// unchanged, or a COPY whose Uses is the actor the run's declared affinity
// resolved for this node.
//
// The copy is load-bearing, not defensive style. Worker.specs caches one
// *nodeSpec per digest and hands the same pointer to every concurrent
// dispatch of every run of that workflow. Writing the override into that
// shared value would make the last trigger's routing decision apply to runs
// it has nothing to do with, intermittently and without a trace -- and it
// would do so in exactly the deployments where affinity is worth having,
// which are the ones running many concurrent triggered runs of one workflow.
//
// A value it cannot read falls back to the declared actor rather than
// refusing the dispatch. The declaration is always a legitimate answer (it is
// what the definition pinned), whereas a refusal here would turn a bad column
// value into a run that cannot proceed at all.
func applyAffinity(node *nodeSpec, nodeID string, raw json.RawMessage) *nodeSpec {
	if len(raw) == 0 {
		return node
	}
	var affinity runAffinity
	if err := json.Unmarshal(raw, &affinity); err != nil {
		return node
	}
	chosen, ok := affinity[nodeID]
	if !ok || chosen.Actor == "" {
		return node
	}
	routed := *node
	routed.Uses = chosen.Actor
	return &routed
}
