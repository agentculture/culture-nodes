package engine

import (
	"encoding/json"
	"fmt"
)

// Declared actor affinity, resolved (issue #107, task t33).
//
// See internal/compiler/affinity.go for what the declaration means and why it
// is declared rather than inferred. This file is the other half: turning the
// declaration plus one triggering event into the value that gets written to
// runs.actor_affinity (migration 0033) and read back at dispatch
// (internal/worker/worker.go).

// ResolvedAffinity is one node's routing decision, as it is stored on the run.
//
// Rule is the declared rule's name, and it is carried rather than dropped
// because it is the label the comparative record slices by. "actor X ran this
// node" is a fact you can already get from the attempt; "actor X was chosen
// because the workflow classified this as security-findings work" is the fact
// that makes a comparison between actors mean something. A rule that declared
// no name records an empty Rule -- honestly unlabelled rather than
// synthetically labelled with an index nobody wrote.
type ResolvedAffinity struct {
	Actor string `json:"actor"`
	Rule  string `json:"rule,omitempty"`
}

// ResolvedAffinities maps node id to the actor its declared affinity chose.
// A nil map means the workflow declared no affinity; an empty non-nil map
// means it declared some and none matched. Both leave every node dispatching
// to the actor its `uses` pins, but they are different facts and the run
// records whichever one is true.
type ResolvedAffinities map[string]ResolvedAffinity

// ResolveAffinity evaluates the workflow's declared affinity rules against a
// triggering event and returns the first match per node.
//
// A condition that CANNOT BE EVALUATED is an error, not a non-match. That is
// the one judgement call in this function and it is deliberate: the rules
// that follow a failed one are usually a catch-all default, so treating an
// undecidable condition as "does not match" would route to the default and
// leave a run that looks correctly routed. A refusal here surfaces as a
// failure to create the triggered run at all -- loud, in the delivery
// response, at the moment the mistake was made -- which is the same
// fail-loud-at-the-boundary stance TriggerEvent already takes for a trigger
// condition it cannot evaluate.
func (w *Workflow) ResolveAffinity(ev PickupEvent) (ResolvedAffinities, error) {
	if len(w.Affinity) == 0 {
		return nil, nil
	}
	activation := map[string]any{
		celVarInput: map[string]any{}, celVarOutput: map[string]any{}, celVarOutcome: "",
		celVarEvent: map[string]any{
			"name": ev.Name, "emitter": ev.Emitter, "payload": decodeActivation(ev.Payload),
		},
	}
	out := ResolvedAffinities{}
	for _, rule := range w.Affinity {
		if _, decided := out[rule.Node]; decided {
			// First match wins. The compiler already refused a second
			// unconditional rule for the same node, so reaching here means an
			// earlier CONDITIONAL rule matched, and the later ones -- default
			// included -- are correctly skipped.
			continue
		}
		if rule.Condition == nil {
			out[rule.Node] = ResolvedAffinity{Actor: rule.Actor, Rule: rule.Name}
			continue
		}
		value, _, err := rule.Condition.Eval(activation)
		if err != nil {
			return nil, fmt.Errorf("affinity rule %s for node %q: %w", ruleLabel(rule), rule.Node, err)
		}
		ok, err := truthy(value)
		if err != nil {
			return nil, fmt.Errorf("affinity rule %s for node %q: %w", ruleLabel(rule), rule.Node, err)
		}
		if ok {
			out[rule.Node] = ResolvedAffinity{Actor: rule.Actor, Rule: rule.Name}
		}
	}
	return out, nil
}

// ruleLabel names a rule in a diagnostic: its declared name when it has one,
// otherwise the condition itself, which is what the author would recognise.
func ruleLabel(rule AffinityRule) string {
	if rule.Name != "" {
		return rule.Name
	}
	return fmt.Sprintf("%q", rule.When)
}

// affinityJSON encodes resolved affinities for the runs.actor_affinity
// column. nil in, nil out: a run that resolved nothing stores SQL NULL, not
// an empty object, so "this definition declares no routing" and "the routing
// declined to route" stay distinguishable in the row itself.
func affinityJSON(resolved ResolvedAffinities) json.RawMessage {
	if resolved == nil {
		return nil
	}
	encoded, err := json.Marshal(resolved)
	if err != nil {
		// A map of plain strings never fails to marshal.
		return nil
	}
	return encoded
}
