package compiler

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// splitEdgeFrom decomposes an edge source of the form "<node>.<outcome>".
func splitEdgeFrom(from string) (nodeName, outcome string, ok bool) {
	nodeName, outcome, ok = strings.Cut(from, ".")
	if !ok || nodeName == "" || outcome == "" {
		return "", "", false
	}
	return nodeName, outcome, true
}

// declaredOutcomes returns every domain outcome a node can produce, sorted:
// the outcomes its contract declares, the ports a decision node selects, and
// the outcomes its kind implies (an approval node's approved/rejected/expired
// come from the human decision, not from a contract).
func declaredOutcomes(n *node) []string {
	set := make(map[string]bool)
	if n.Contract != nil {
		for name := range n.Contract.Outcomes {
			set[name] = true
		}
	}
	for _, port := range n.Select {
		if port.Outcome != "" {
			set[port.Outcome] = true
		}
	}
	for _, name := range impliedOutcomes[n.Kind] {
		set[name] = true
	}
	return sortedKeys(set)
}

// checkGraph is the §11.4 graph level: references exist, every node is
// reachable from the entry, at least one terminal path exists, and loops are
// bounded by policy rather than by an agent's judgement.
func (c *compilation) checkGraph() {
	nodes := c.doc.Spec.Nodes
	entry := c.doc.Spec.Entry

	entryExists := false
	if _, ok := nodes[entry]; ok {
		entryExists = true
	} else {
		c.add(LevelError, "/spec/entry", CodeGraphEntryUnknown,
			fmt.Sprintf("entry node %q is not declared in spec.nodes", entry),
			fmt.Sprintf("set spec.entry to one of: %s", strings.Join(sortedKeys(nodes), ", ")))
	}

	// adjacency holds only edges whose endpoints both exist, so reachability
	// reports one honest verdict per node instead of compounding a bad edge
	// into a wave of "unreachable" noise.
	adjacency := make(map[string][]string, len(nodes))

	// eventTargets are the nodes an `onEvent` edge can create a token at
	// (issue #43, design D9). They are reachability ROOTS beside the entry
	// node: a node an event picks up is reached by the world, not by a path
	// from the entry, and calling it unreachable would refuse exactly the
	// graphs this feature exists to allow.
	var eventTargets []string

	for i, e := range c.doc.Spec.Edges {
		base := "/spec/edges/" + strconv.Itoa(i)

		if e.OnEvent != "" {
			c.checkEventEdge(base, e, &eventTargets)
			continue
		}

		fromNode, outcome, ok := splitEdgeFrom(e.From)
		if !ok {
			// The schema's `from` pattern already rejected this; saying so
			// again here would add nothing.
			continue
		}

		source, sourceExists := nodes[fromNode]
		if !sourceExists {
			c.add(LevelError, base+"/from", CodeGraphEdgeSourceUnknown,
				fmt.Sprintf("edge source names node %q, which is not declared in spec.nodes", fromNode),
				"declare the node, or point the edge at an existing one")
		}
		_, targetExists := nodes[e.To]
		if !targetExists {
			c.add(LevelError, base+"/to", CodeGraphEdgeTargetUnknown,
				fmt.Sprintf("edge target %q is not declared in spec.nodes", e.To),
				fmt.Sprintf("point the edge at one of: %s", strings.Join(sortedKeys(nodes), ", ")))
		}

		if sourceExists {
			switch {
			case source.Kind == KindEnd:
				c.add(LevelError, base+"/from", CodeGraphEdgeFromEndNode,
					fmt.Sprintf("node %q is an end node and produces the workflow result; it has no outgoing edges", fromNode),
					"remove the edge, or change the node's kind if the run should continue")
			case refusalOutcomes[outcome] && !dispatchGuardedKinds[source.Kind]:
				// A refusal name is reserved but not universal: only a kind
				// whose dispatch the control plane guards can ever produce
				// one.
				c.add(LevelError, base+"/from", CodeGraphOutcomeUndeclared,
					fmt.Sprintf("node %q is kind %q, whose dispatch the control plane does not guard, so it can never produce outcome %q",
						fromNode, source.Kind, outcome),
					fmt.Sprintf("route %q only from an agent or action.http node; refusals are produced at the actor-dispatch site",
						outcome))
			case refusalOutcomes[outcome]:
				// Routable on a guarded kind without being declared anywhere,
				// exactly like a technical status.
			case !technicalStatuses[outcome] && !contains(declaredOutcomes(source), outcome):
				c.add(LevelError, base+"/from", CodeGraphOutcomeUndeclared,
					fmt.Sprintf("node %q does not declare outcome %q", fromNode, outcome),
					fmt.Sprintf("declare it under the node's contract.outcomes, or route one of: %s",
						strings.Join(append(declaredOutcomes(source), sortedKeys(technicalStatuses)...), ", ")))
			}
		}

		if sourceExists && targetExists {
			adjacency[fromNode] = append(adjacency[fromNode], e.To)
		}
	}

	if !entryExists {
		// Reachability, terminality, and cycles are all statements about the
		// graph *from the entry*. Without one there is nothing true to say.
		return
	}

	reachable := reachableFrom(append([]string{entry}, eventTargets...), adjacency)

	for _, id := range sortedKeys(nodes) {
		if !reachable[id] {
			c.add(LevelError, pointerJoin("/spec/nodes", id), CodeGraphNodeUnreachable,
				fmt.Sprintf("node %q is not reachable from entry node %q", id, entry),
				"add an edge that reaches it, or remove the node")
		}
	}

	terminal := false
	for id := range reachable {
		if n, ok := nodes[id]; ok && n.Kind == KindEnd {
			terminal = true
			break
		}
	}
	if !terminal {
		c.add(LevelError, "/spec/nodes", CodeGraphNoEndReachable,
			fmt.Sprintf("no end node is reachable from entry node %q, so no run can produce a result", entry),
			"add a node of kind 'end' and an edge that reaches it")
	}

	if cycle := findCycle(entry, adjacency); cycle != nil && !c.doc.Spec.Limits.bounded() {
		c.add(LevelWarning, "/spec/limits", CodeGraphUnboundedCycle,
			fmt.Sprintf("loop %s relies on compiler default bounds, not on a declared policy", strings.Join(cycle, " -> ")),
			"declare spec.limits.maxTransitions and spec.limits.maxVisitsPerNode; no loop may rely solely on an agent deciding when to stop (PRD §9.7)")
	}
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// checkEventEdge validates one `onEvent` edge and records its target as a
// reachability root. Two refusals, both structural:
//
//   - the target must exist, like any edge target;
//   - the target must not be an `end` node. A run must not COMPLETE as a side
//     effect of an event arriving: completion resolves the workflow's output
//     and, with parallel tokens, has to be able to prove no sibling is still
//     live (design D7). An event that could end a run from the outside would
//     race both of those guarantees, and the honest place to say so is here.
func (c *compilation) checkEventEdge(base string, e edge, eventTargets *[]string) {
	target, ok := c.doc.Spec.Nodes[e.To]
	if !ok {
		c.add(LevelError, base+"/to", CodeGraphEdgeTargetUnknown,
			fmt.Sprintf("event edge target %q is not declared in spec.nodes", e.To),
			fmt.Sprintf("point the edge at one of: %s", strings.Join(sortedKeys(c.doc.Spec.Nodes), ", ")))
		return
	}
	if target.Kind == KindEnd {
		c.add(LevelError, base+"/to", CodeGraphEventEdgeToEnd,
			fmt.Sprintf("event %q routes to end node %q; a run must not complete as a side effect of an event being delivered", e.OnEvent, e.To),
			"route the event at a working node and let the run reach its end node through the graph")
		return
	}
	*eventTargets = append(*eventTargets, e.To)
}

// reachableFrom walks the graph breadth-first from every root.
func reachableFrom(roots []string, adjacency map[string][]string) map[string]bool {
	seen := make(map[string]bool, len(roots))
	queue := make([]string, 0, len(roots))
	for _, root := range roots {
		if seen[root] {
			continue
		}
		seen[root] = true
		queue = append(queue, root)
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adjacency[current] {
			if seen[next] {
				continue
			}
			seen[next] = true
			queue = append(queue, next)
		}
	}
	return seen
}

// findCycle returns one cycle reachable from start as a node path, or nil.
// Which cycle is found is made deterministic by visiting successors in sorted
// order — an arbitrary choice would make the warning's message flap between
// otherwise identical compilations.
func findCycle(start string, adjacency map[string][]string) []string {
	const (
		white = 0 // unvisited
		grey  = 1 // on the current stack
		black = 2 // fully explored
	)
	colour := make(map[string]int)
	var stack []string
	var found []string

	var visit func(id string) bool
	visit = func(id string) bool {
		colour[id] = grey
		stack = append(stack, id)

		successors := append([]string(nil), adjacency[id]...)
		sort.Strings(successors)
		for _, next := range successors {
			switch colour[next] {
			case grey:
				// Close the loop at the first stack entry equal to next.
				for i, entry := range stack {
					if entry == next {
						found = append(append([]string(nil), stack[i:]...), next)
						return true
					}
				}
				found = []string{next, next}
				return true
			case white:
				if visit(next) {
					return true
				}
			}
		}

		stack = stack[:len(stack)-1]
		colour[id] = black
		return false
	}

	if visit(start) {
		return found
	}
	return nil
}
