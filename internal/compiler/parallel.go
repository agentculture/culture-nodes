package compiler

import (
	"fmt"
	"sort"
)

// Split/join structural checks (issue #43; parallel-tokens design §8 item 5).
//
// A `parallel` node is a router that fans one token out per eligible edge
// from its kind-implied `split` outcome; a `join` node is a barrier that
// reconvenes the sibling set a split created. Both produce engine-shaped
// outputs, not domain contracts, and both lean on graph structure the
// compiler can refuse at publish time instead of the engine discovering at
// runtime:
//
//   - a parallel node with no split edge can never fan out (its completion
//     would hit the no-eligible-edge diagnostic on every run);
//   - a join node with no incoming edge is a barrier nothing can arrive at;
//   - an end node reachable from a split without passing a join would
//     complete the run with sibling tokens still active (design D7) — the
//     runtime keeps a loud defense-in-depth guard, but the compiler is where
//     the answer belongs;
//   - a join reachable outside any split has no token group to count
//     against — the barrier's identity is (run, node, group), and a token
//     that never passed a split carries no group.
//
// The last two are one reachability walk over (node, split-depth) states:
// following a `<parallel>.split` edge increments the depth, leaving a join
// node decrements it, and every other edge preserves it. Cycles are handled
// by the visited set over those states; the depth is capped at the number of
// parallel nodes + 1, which bounds the state space — a path that nests
// deeper than that has re-entered a split without joining more times than
// there are distinct splits, and everything refusable about it is already
// visible at the shallower depths the walk did explore.
func (c *compilation) checkParallelJoin() {
	nodes := c.doc.Spec.Nodes

	for _, id := range c.nodeIDs {
		n := nodes[id]
		base := pointerJoin("/spec/nodes", id)
		c.checkJoinConfig(base, id, n)

		if (n.Kind == KindParallel || n.Kind == KindJoin) && n.Contract != nil {
			c.add(LevelError, base+"/contract", CodeContractRouterWithContract,
				fmt.Sprintf("node %q is a %s node and carries a contract block; parallel and join nodes produce engine-shaped outputs, not domain contracts", id, n.Kind),
				"remove the contract block; route the branches' own outputs, or guard over the join's arrival array downstream")
		}

		switch n.Kind {
		case KindParallel:
			if !c.hasEdgeFromOutcome(id, "split") {
				c.add(LevelError, base, CodeGraphParallelNoSplitEdge,
					fmt.Sprintf("parallel node %q has no edge from %s.split, so it can never fan out", id, id),
					fmt.Sprintf("add at least one edge with from: %s.split", id))
			}
		case KindJoin:
			if !c.hasEdgeInto(id) {
				c.add(LevelError, base, CodeGraphJoinNoIncomingEdge,
					fmt.Sprintf("join node %q has no incoming edge, so no branch can ever arrive at it", id),
					"route at least one branch outcome into the join")
			}
		}
	}

	c.checkSplitReachability()
}

// checkJoinConfig enforces the join-block value semantics the schema cannot
// state: the block belongs to join nodes only, and quorum accompanies exactly
// the quorum policy.
func (c *compilation) checkJoinConfig(base, id string, n *node) {
	if n.Join != nil && n.Kind != KindJoin {
		c.add(LevelError, base+"/join", CodeContractJoinMisplaced,
			fmt.Sprintf("node %q declares a join block but is kind %q; a barrier policy is only meaningful on a join node", id, n.Kind),
			"remove the join block, or change the node's kind to join")
	}
	if n.Kind != KindJoin {
		return
	}
	if n.Join == nil {
		// The schema's if/then also reports the missing required property;
		// this is the contract level's own independent verdict, the same way
		// a missing ownerRef is reported twice.
		c.add(LevelError, base+"/join", CodeContractJoinPolicyMissing,
			fmt.Sprintf("join node %q declares no barrier policy", id),
			"add a join block: {policy: all | any | quorum}")
		return
	}
	switch n.Join.Policy {
	case JoinPolicyQuorum:
		if n.Join.Quorum == nil {
			c.add(LevelError, base+"/join/quorum", CodeContractJoinPolicyInvalid,
				fmt.Sprintf("join node %q declares policy quorum without a quorum value", id),
				"add join.quorum (an integer >= 1): how many arrivals fire the barrier")
		}
	case JoinPolicyAll, JoinPolicyAny:
		if n.Join.Quorum != nil {
			c.add(LevelError, base+"/join/quorum", CodeContractJoinPolicyInvalid,
				fmt.Sprintf("join node %q declares a quorum value under policy %q, which never reads one", id, n.Join.Policy),
				"remove join.quorum, or change the policy to quorum")
		}
	}
	// An unknown policy string is the schema enum's verdict; nothing useful
	// to add on top of it here.
}

func (c *compilation) hasEdgeFromOutcome(nodeID, outcome string) bool {
	for _, e := range c.doc.Spec.Edges {
		fromNode, fromOutcome, ok := splitEdgeFrom(e.From)
		if ok && fromNode == nodeID && fromOutcome == outcome {
			return true
		}
	}
	return false
}

func (c *compilation) hasEdgeInto(nodeID string) bool {
	for _, e := range c.doc.Spec.Edges {
		if e.To == nodeID {
			return true
		}
	}
	return false
}

// depthState is one (node, open-split depth) point of the reachability walk.
type depthState struct {
	node  string
	depth int
}

// checkSplitReachability performs the (node, depth) walk described in the
// package comment above: design D7's no-end-inside-a-split refusal and its
// dual, no join reachable outside any split. Guards are ignored — every edge
// is traversable — which is the conservative reading: a guard that happens
// to be false at runtime must not be what stands between a run and a
// stranded sibling set.
func (c *compilation) checkSplitReachability() {
	nodes := c.doc.Spec.Nodes
	entry := c.doc.Spec.Entry
	if _, ok := nodes[entry]; !ok {
		return // graph level already reported the unknown entry
	}

	maxDepth := 1
	for _, id := range c.nodeIDs {
		if nodes[id].Kind == KindParallel {
			maxDepth++
		}
	}

	// Adjacency in the depth model: each edge is (target, depth delta of the
	// EDGE itself). A `<parallel>.split` edge opens a split (+1); every edge
	// LEAVING a join closes the one its barrier counted (-1) — the post-join
	// token re-enters the enclosing group whatever outcome is routed; all
	// other edges preserve depth. Only edges whose endpoints both exist are
	// walked, matching checkGraph's adjacency discipline.
	type hop struct {
		to    string
		delta int
	}
	adjacency := make(map[string][]hop, len(nodes))
	for _, e := range c.doc.Spec.Edges {
		fromNode, fromOutcome, ok := splitEdgeFrom(e.From)
		if !ok {
			continue
		}
		source, sourceExists := nodes[fromNode]
		if _, targetExists := nodes[e.To]; !sourceExists || !targetExists {
			continue
		}
		delta := 0
		if source.Kind == KindParallel && fromOutcome == "split" {
			delta = 1
		}
		if source.Kind == KindJoin {
			delta = -1
		}
		adjacency[fromNode] = append(adjacency[fromNode], hop{to: e.To, delta: delta})
	}

	seen := map[depthState]bool{{node: entry, depth: 0}: true}
	queue := []depthState{{node: entry, depth: 0}}
	endInsideSplit := map[string]bool{}
	joinOutsideSplit := map[string]bool{}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		n := nodes[current.node]

		switch {
		case n.Kind == KindEnd && current.depth > 0:
			endInsideSplit[current.node] = true
			continue
		case n.Kind == KindJoin && current.depth == 0:
			joinOutsideSplit[current.node] = true
			continue
		}

		for _, h := range adjacency[current.node] {
			next := depthState{node: h.to, depth: current.depth + h.delta}
			if next.depth < 0 || next.depth > maxDepth || seen[next] {
				continue
			}
			seen[next] = true
			queue = append(queue, next)
		}
	}

	for _, id := range sortedSetKeys(endInsideSplit) {
		c.add(LevelError, pointerJoin("/spec/nodes", id), CodeGraphEndInsideSplit,
			fmt.Sprintf("end node %q is reachable from inside a split without passing through a join; completing the run there would strand the sibling tokens (design D7)", id),
			"route every split branch into a join node before any end node")
	}
	for _, id := range sortedSetKeys(joinOutsideSplit) {
		c.add(LevelError, pointerJoin("/spec/nodes", id), CodeGraphJoinOutsideSplit,
			fmt.Sprintf("join node %q is reachable outside any split; a token that never passed a parallel node carries no group for the barrier to count against", id),
			"put a parallel node on every path into the join, or remove the join")
	}
}

func sortedSetKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
