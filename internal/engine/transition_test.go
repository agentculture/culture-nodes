package engine

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// buildWorkflow assembles a Workflow directly, compiling any guard text, so a
// test can state the graph it wants to reason about instead of authoring a
// YAML document for every shape.
func buildWorkflow(t *testing.T, limits Limits, nodes map[string]*Node, edges ...Edge) *Workflow {
	t.Helper()
	env, err := newCELEnv()
	if err != nil {
		t.Fatalf("newCELEnv: %v", err)
	}
	for i := range edges {
		if edges[i].When == "" {
			continue
		}
		program, err := compileGuard(env, edges[i].When)
		if err != nil {
			t.Fatalf("compile guard %q: %v", edges[i].When, err)
		}
		edges[i].Guard = program
	}
	return &Workflow{Digest: "sha256:test", Entry: "a", Limits: limits, Nodes: nodes, Edges: edges}
}

func defaultLimits() Limits {
	return Limits{MaxDuration: time.Hour, MaxTransitions: 32, MaxVisitsPerNode: 8, MaxParallelTokens: 1}
}

// The loop edge is a *domain outcome* following a graph edge, not a failure:
// a checker that ran perfectly and answered `changes_required` sends the token
// back to work (PRD §3.4).
func TestPlanTransitionFollowsADomainOutcomeBackIntoTheLoop(t *testing.T) {
	wf := loadFixture(t, "loop.workflow.yaml")

	plan := planTransition(transitionInput{
		Workflow: wf,
		NodeID:   "check",
		Outcome:  "changes_required",
		Output:   json.RawMessage(`{"give_up":false}`),
		Visits:   map[string]int{"intake": 1, "work": 1, "check": 1},
	})
	if plan.Bound != nil || plan.Edge == nil {
		t.Fatalf("expected an eligible edge, got %+v", plan)
	}
	if plan.NextNodeID != "work" {
		t.Errorf("next node = %q, want work", plan.NextNodeID)
	}
}

// First match wins in normalized edge order, and the guard is what decides.
func TestPlanTransitionFirstMatchingGuardWins(t *testing.T) {
	wf := loadFixture(t, "loop.workflow.yaml")

	plan := planTransition(transitionInput{
		Workflow: wf,
		NodeID:   "check",
		Outcome:  "changes_required",
		Output:   json.RawMessage(`{"give_up":true}`),
		Visits:   map[string]int{"check": 1},
	})
	if plan.Edge == nil || plan.NextNodeID != "finish" {
		t.Fatalf("a checker that gave up should route to finish, got %+v", plan)
	}
	if plan.Edge.When == "" {
		t.Error("the winning edge should be the guarded one")
	}
}

// A guard that reaches into data the payload does not carry has not said
// "yes"; the run falls through to the next candidate rather than wedging.
func TestPlanTransitionGuardEvaluationFailureDoesNotMatch(t *testing.T) {
	wf := buildWorkflow(t, defaultLimits(),
		map[string]*Node{
			"a": {ID: "a", Kind: "agent", Outcomes: []string{"done"}},
			"b": {ID: "b", Kind: "agent", Outcomes: []string{"done"}},
			"c": {ID: "c", Kind: "agent", Outcomes: []string{"done"}},
		},
		Edge{From: "a.done", FromNode: "a", FromOutcome: "done", To: "b", When: "output.missing_field"},
		Edge{From: "a.done", FromNode: "a", FromOutcome: "done", To: "c"},
	)

	plan := planTransition(transitionInput{
		Workflow: wf,
		NodeID:   "a",
		Outcome:  "done",
		Output:   json.RawMessage(`{"present":1}`),
	})
	if plan.NextNodeID != "c" {
		t.Fatalf("expected the unguarded fallback, got %+v", plan)
	}
}

// A guard that evaluates to something other than a boolean has not answered
// the question the edge asks, so it does not match — and when it was the only
// candidate, the diagnostic says so rather than the run silently stalling.
func TestPlanTransitionNonBooleanGuardIsReported(t *testing.T) {
	wf := buildWorkflow(t, defaultLimits(),
		map[string]*Node{
			"a": {ID: "a", Kind: "agent", Outcomes: []string{"done"}},
			"b": {ID: "b", Kind: "agent", Outcomes: []string{"done"}},
		},
		Edge{From: "a.done", FromNode: "a", FromOutcome: "done", To: "b", When: "output.count"},
	)

	plan := planTransition(transitionInput{
		Workflow: wf,
		NodeID:   "a",
		Outcome:  "done",
		Output:   json.RawMessage(`{"count":3}`),
	})
	if plan.Edge != nil {
		t.Fatalf("a non-boolean guard should not match, got %+v", plan)
	}
	if plan.Diagnostic == "" {
		t.Fatal("a run that cannot move should say why")
	}
}

func TestPlanTransitionNoEligibleEdgeIsDiagnosed(t *testing.T) {
	wf := buildWorkflow(t, defaultLimits(),
		map[string]*Node{"a": {ID: "a", Kind: "agent", Outcomes: []string{"done", "blocked"}}},
		Edge{From: "a.done", FromNode: "a", FromOutcome: "done", To: "a"},
	)

	plan := planTransition(transitionInput{Workflow: wf, NodeID: "a", Outcome: "blocked"})
	if plan.Edge != nil || plan.Complete {
		t.Fatalf("expected no eligible edge, got %+v", plan)
	}
	if plan.Diagnostic == "" {
		t.Fatal("expected a diagnostic naming the outcome with no edge")
	}
}

// An end node has no outgoing edges by construction, so reaching one is the
// run finishing rather than a routing failure.
func TestPlanTransitionEndNodeCompletesTheRun(t *testing.T) {
	wf := buildWorkflow(t, defaultLimits(),
		map[string]*Node{"finish": {ID: "finish", Kind: kindEnd}},
	)

	plan := planTransition(transitionInput{Workflow: wf, NodeID: "finish", Outcome: "completed"})
	if !plan.Complete {
		t.Fatalf("expected the run to complete, got %+v", plan)
	}
}

func TestPlanTransitionEnforcesEachBound(t *testing.T) {
	nodes := map[string]*Node{
		"a": {ID: "a", Kind: "agent", Outcomes: []string{"done"}},
		"b": {ID: "b", Kind: "agent", Outcomes: []string{"done"}},
	}
	edge := Edge{From: "a.done", FromNode: "a", FromOutcome: "done", To: "b"}

	cases := []struct {
		name  string
		limit Limits
		in    transitionInput
		want  BoundKind
	}{
		{
			name:  "transitions",
			limit: Limits{MaxTransitions: 3, MaxVisitsPerNode: 100, MaxDuration: time.Hour},
			in:    transitionInput{Transitions: 3},
			want:  BoundTransitions,
		},
		{
			name:  "visits",
			limit: Limits{MaxTransitions: 100, MaxVisitsPerNode: 2, MaxDuration: time.Hour},
			in:    transitionInput{Visits: map[string]int{"b": 2}},
			want:  BoundVisits,
		},
		{
			name:  "duration",
			limit: Limits{MaxTransitions: 100, MaxVisitsPerNode: 100, MaxDuration: time.Minute},
			in:    transitionInput{Elapsed: 90 * time.Second},
			want:  BoundDuration,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.Workflow = buildWorkflow(t, tc.limit, nodes, edge)
			in.NodeID, in.Outcome = "a", "done"

			plan := planTransition(in)
			if plan.Bound == nil {
				t.Fatalf("expected the %s bound to stop the transition, got %+v", tc.want, plan)
			}
			if plan.Bound.Kind != tc.want {
				t.Errorf("bound = %s, want %s", plan.Bound.Kind, tc.want)
			}
			// The bound is reported *with* the edge it refused to cross, so an
			// operator can see where the run was heading.
			if plan.Edge == nil || plan.NextNodeID != "b" {
				t.Errorf("a bound should still name the blocked transition, got %+v", plan)
			}
		})
	}
}

// Property: whatever the graph, however it loops, and whatever outcomes the
// actors produce, a run never takes more than maxTransitions transitions.
// This is PRD §9.7's guarantee that no loop relies solely on an agent
// deciding when to stop — the agent picks the outcome here, and the engine
// still stops the run.
func TestPropertyTransitionsNeverExceedMaxTransitions(t *testing.T) {
	for seed := int64(0); seed < 200; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test input, not security
			wf := generateLoopingWorkflow(t, rng)

			transitions := 0
			visits := map[string]int{wf.Entry: 1}
			current := wf.Entry
			// The cap only exists to catch a planner that never terminates;
			// the assertions below are about the bound, not about the cap.
			maxSteps := wf.Limits.MaxTransitions*4 + 64
			steps := 0

			for ; steps < maxSteps; steps++ {
				node := wf.Nodes[current]
				if len(node.Outcomes) == 0 {
					break
				}
				outcome := node.Outcomes[rng.Intn(len(node.Outcomes))]

				plan := planTransition(transitionInput{
					Workflow:    wf,
					NodeID:      current,
					Outcome:     outcome,
					Output:      json.RawMessage(fmt.Sprintf(`{"n":%d}`, rng.Intn(4))),
					Transitions: transitions,
					Visits:      visits,
				})
				if plan.Bound != nil || plan.Complete || plan.Edge == nil {
					break
				}
				transitions++
				visits[plan.NextNodeID]++
				current = plan.NextNodeID

				if transitions > wf.Limits.MaxTransitions {
					t.Fatalf("took %d transitions with maxTransitions %d", transitions, wf.Limits.MaxTransitions)
				}
				for node, count := range visits {
					if count > wf.Limits.MaxVisitsPerNode {
						t.Fatalf("node %q visited %d times with maxVisitsPerNode %d",
							node, count, wf.Limits.MaxVisitsPerNode)
					}
				}
			}

			if steps == maxSteps {
				t.Fatalf("the planner never stopped: %d steps with maxTransitions %d",
					steps, wf.Limits.MaxTransitions)
			}
		})
	}
}

// generateLoopingWorkflow builds a graph that is all loop: every node's every
// outcome routes somewhere, so nothing but a bound can end the run. Half the
// edges carry a guard, some of which reach into fields the payload may not
// have, so guard failure is part of the property too.
func generateLoopingWorkflow(t *testing.T, rng *rand.Rand) *Workflow {
	t.Helper()

	count := 2 + rng.Intn(5)
	nodes := make(map[string]*Node, count)
	names := make([]string, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("n%d", i)
		names = append(names, name)
		nodes[name] = &Node{ID: name, Kind: "agent", Outcomes: []string{"a", "b"}}
	}

	guards := []string{"", "", "output.n > 1", "outcome == 'a'", "output.absent"}
	var edges []Edge
	for _, from := range names {
		for _, outcome := range []string{"a", "b"} {
			// Two candidates per outcome, so guard failure has somewhere to
			// fall through to and the loop cannot dead-end by accident.
			for k := 0; k < 2; k++ {
				to := names[rng.Intn(len(names))]
				when := guards[rng.Intn(len(guards))]
				if k == 1 {
					when = "" // the fallback is always unguarded
				}
				edges = append(edges, Edge{
					From:        from + "." + outcome,
					FromNode:    from,
					FromOutcome: outcome,
					To:          to,
					When:        when,
				})
			}
		}
	}

	limits := Limits{
		MaxTransitions:    1 + rng.Intn(12),
		MaxVisitsPerNode:  1 + rng.Intn(6),
		MaxDuration:       time.Hour,
		MaxParallelTokens: 1,
	}
	wf := buildWorkflow(t, limits, nodes, edges...)
	wf.Entry = names[0]
	return wf
}
