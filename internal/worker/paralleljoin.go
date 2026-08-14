package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Parallel/join dispatch (issue #43, parallel-tokens design D2).
//
// Both kinds are ROUTERS: they do no domain work, invoke no actor, and their
// one job is to complete so the engine's own §12.5 transaction performs the
// interesting part — the fan-out for a parallel node, the joined-outcome
// routing for a join. They dispatch as ordinary work items rather than being
// routed inline by the engine, because an inline route would create node
// runs that complete without any attempt row, breaking the
// attempt-per-execution invariant, and would lose the fencing/lease/restart
// safety every other node kind gets for free. The cost is one queue hop per
// split/join; design open item O7 tracks measuring whether that ever
// matters.

// dispatchParallel completes a parallel node run immediately: succeeded,
// kind-implied outcome `split`, trivial output. The fan-out itself — token
// group, branch tokens, node runs, work items — happens inside this
// completion's engine transaction (completion.fanOut).
func (w *Worker) dispatchParallel(ctx context.Context, claimed postgres.ClaimedWork) error {
	_, err := w.complete(ctx, claimed, engine.CompletionRequest{
		TechStatus: engine.StatusSucceeded,
		Outcome:    "split",
		Output:     json.RawMessage(`{}`),
	})
	if err != nil && !isStale(err) {
		return err
	}
	return nil
}

// dispatchJoin completes a satisfied join node run: it reads the recorded
// arrivals back and reports the design-D5 aggregated output — an array
// ordered by arrival, each element carrying from_node, token_id, outcome,
// and output — under the kind-implied outcome `joined`. The barrier
// counting already happened in the arrivals' own transactions; a join work
// item only exists because the barrier satisfied.
func (w *Worker) dispatchJoin(ctx context.Context, claimed postgres.ClaimedWork, node *nodeSpec, dc DispatchContext) error {
	agg, err := w.db.JoinAggregate(ctx, dc.NodeRunID)
	if err != nil {
		return w.failAttempt(ctx, claimed, "", engine.StatusFailed, "definition",
			fmt.Sprintf("join node %q has no readable arrivals for node run %s: %v", node.ID, dc.NodeRunID, err))
	}

	output, err := buildJoinOutput(agg, node.JoinPolicy)
	if err != nil {
		return w.failAttempt(ctx, claimed, "", engine.StatusFailed, "definition",
			fmt.Sprintf("join node %q output could not be built: %v", node.ID, err))
	}

	_, err = w.complete(ctx, claimed, engine.CompletionRequest{
		TechStatus: engine.StatusSucceeded,
		Outcome:    "joined",
		Output:     output,
	})
	if err != nil && !isStale(err) {
		return err
	}
	return nil
}

// joinOutputArrival is one element of the joined output's ordered arrival
// array (design D5: an array ordered by arrival is collision-free where a
// node-id-keyed object is not — two branches may route through one terminal
// node — and it preserves arrival order as information any/quorum consumers
// legitimately want).
type joinOutputArrival struct {
	FromNode  string          `json:"from_node"`
	TokenID   string          `json:"token_id"`
	Outcome   string          `json:"outcome"`
	Output    json.RawMessage `json:"output"`
	ArrivedAt time.Time       `json:"arrived_at"`
}

type joinOutput struct {
	Arrivals    []joinOutputArrival `json:"arrivals"`
	Policy      string              `json:"policy"`
	Cardinality int                 `json:"cardinality"`
}

// buildJoinOutput renders the aggregate as the join's output payload.
func buildJoinOutput(agg postgres.JoinAggregate, policy string) (json.RawMessage, error) {
	out := joinOutput{
		Arrivals:    make([]joinOutputArrival, 0, len(agg.Arrivals)),
		Policy:      policy,
		Cardinality: agg.Cardinality,
	}
	for _, row := range agg.Arrivals {
		output := row.Output
		if len(output) == 0 {
			output = json.RawMessage("null")
		}
		out.Arrivals = append(out.Arrivals, joinOutputArrival{
			FromNode:  row.FromNode,
			TokenID:   row.TokenID,
			Outcome:   row.Outcome,
			Output:    output,
			ArrivedAt: row.ArrivedAt,
		})
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}
