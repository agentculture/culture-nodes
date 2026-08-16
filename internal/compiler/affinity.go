package compiler

import (
	"fmt"
	"strconv"
	"strings"
)

// Declared actor affinity (issue #107, task t33).
//
// An affinity rule says: when a run of this workflow is started by an event
// matching `when`, the node named `node` should be executed by `actor`,
// overriding the actor its `uses` pins. Rules are evaluated in declaration
// order and the FIRST match for a node wins.
//
// WHY IT IS DECLARED AND NOT INFERRED. The obvious alternative -- pick the
// actor with the shortest queue, or the best recent success rate on similar
// work -- is a scheduler, and a scheduler's choices are not reproducible from
// the run's own record. The point of recording affinity on the run (migration
// 0033) is a COMPARATIVE record: which actor is better at what. That
// comparison is only readable if the workflow said, in advance and in
// writing, what kind of work this was. An inferred choice would tell you
// which actor ran and nothing about what it was chosen FOR.
//
// WHY IT LIVES ON THE WORKFLOW AND NOT THE SCHEDULE. A schedule is deployment
// configuration; which actor should handle a security finding versus a
// dependency bump is a statement about the WORK, which is exactly what the
// graph is for. Putting it in the immutable, content-addressed definition
// also means a run's affinity is pinned by the digest it already pins: you
// can always tell what the rules were when a given run started, even after
// they change.

// affinityRule is one declared routing rule. Node and Actor are required;
// Name labels the rule for the comparative record, and When is the optional
// condition, evaluated against the triggering event.
type affinityRule struct {
	Name  string `json:"name,omitempty"`
	Node  string `json:"node"`
	Actor string `json:"actor"`
	When  string `json:"when,omitempty"`
}

// actorRefPrefix is the scheme an affinity target must carry. An affinity
// rule replaces the actor a node dispatches to, and only an agent-style
// dispatch resolves an actor:// reference (internal/worker/registry.go).
const actorRefPrefix = "actor://"

// affinityDispatchableKinds are the node kinds whose dispatch resolves a
// component reference through the actor registry. Declaring affinity for
// anything else is a rule that could never take effect, so it is refused at
// publication rather than ignored at runtime -- a routing declaration that
// silently does nothing is worse than one that does the wrong thing, because
// nothing in the run says it was skipped.
var affinityDispatchableKinds = map[string]bool{
	KindAgent:      true,
	KindActionHTTP: true,
}

// checkAffinity validates the declared affinity block. Everything it refuses
// is something it can prove wrong from the document alone: whether the actor
// is actually reachable is a registry question, deliberately left to dispatch
// (§9.5 -- a definition that knew where an actor lived would stop being
// portable across deployments).
func (c *compilation) checkAffinity() {
	// defaulted records, per node, the index of the first rule with no
	// condition. Everything after it for that node is unreachable, because
	// an unconditional rule always matches.
	defaulted := map[string]int{}

	for i, rule := range c.doc.Spec.Affinity {
		base := "/spec/affinity/" + strconv.Itoa(i)

		if rule.Node == "" {
			c.add(LevelError, base+"/node", CodeAffinityNodeUnknown,
				"affinity rule names no node",
				"set node to the id of an agent or action.http node in this workflow")
			continue
		}
		n, ok := c.doc.Spec.Nodes[rule.Node]
		if !ok {
			c.add(LevelError, base+"/node", CodeAffinityNodeUnknown,
				fmt.Sprintf("affinity rule names node %q, which this workflow does not declare", rule.Node),
				"name a node declared under spec.nodes")
			continue
		}
		if !affinityDispatchableKinds[n.Kind] {
			c.add(LevelError, base+"/node", CodeAffinityNodeNotDispatchable,
				fmt.Sprintf("affinity rule targets node %q of kind %q, which does not dispatch to an actor",
					rule.Node, n.Kind),
				"declare affinity only for agent or action.http nodes; other kinds resolve no actor reference")
			continue
		}
		if !strings.HasPrefix(rule.Actor, actorRefPrefix) {
			c.add(LevelError, base+"/actor", CodeAffinityActorInvalid,
				fmt.Sprintf("affinity rule for node %q names %q, which is not an actor reference", rule.Node, rule.Actor),
				"use an actor:// reference, e.g. actor://company/developer")
			continue
		}

		if rule.When == "" {
			if first, seen := defaulted[rule.Node]; seen {
				c.add(LevelError, base+"/when", CodeAffinityDuplicateDefault,
					fmt.Sprintf("node %q already has an unconditional affinity rule at /spec/affinity/%d", rule.Node, first),
					"give this rule a `when` condition, or delete one of the two defaults — first match wins, so the second could never apply")
				continue
			}
			defaulted[rule.Node] = i
			continue
		}

		if first, seen := defaulted[rule.Node]; seen {
			c.add(LevelError, base, CodeAffinityUnreachableRule,
				fmt.Sprintf("affinity rule for node %q can never match: the unconditional rule at /spec/affinity/%d already matches everything",
					rule.Node, first),
				"move the unconditional rule after every conditional rule for the same node")
			continue
		}
		c.compileCEL(base+"/when", rule.When)
	}
}
