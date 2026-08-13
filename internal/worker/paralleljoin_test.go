package worker

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// buildJoinOutput is design D5 in one function: an array ordered by arrival,
// each element carrying from_node, token_id, outcome, and output. Two things
// are load-bearing and unit-testable without a database — that two arrivals
// from the SAME node survive distinctly (a node-id-keyed object would lose
// one), and that a branch that recorded no output renders as JSON null
// rather than as an empty string a downstream guard would choke on.

func TestJoinOutputKeepsTwoArrivalsFromOneNode(t *testing.T) {
	at := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	agg := postgres.JoinAggregate{
		Cardinality: 2,
		Arrivals: []postgres.JoinAggregateRow{
			{FromNode: "check", TokenID: "tok_1", Outcome: "passed", Output: json.RawMessage(`{"n":1}`), ArrivedAt: at},
			{FromNode: "check", TokenID: "tok_2", Outcome: "passed", Output: json.RawMessage(`{"n":2}`), ArrivedAt: at.Add(time.Second)},
		},
	}

	raw, err := buildJoinOutput(agg, "all")
	if err != nil {
		t.Fatalf("buildJoinOutput: %v", err)
	}
	var out joinOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode join output: %v", err)
	}
	if out.Policy != "all" || out.Cardinality != 2 {
		t.Errorf("policy/cardinality = %q/%d, want all/2", out.Policy, out.Cardinality)
	}
	if len(out.Arrivals) != 2 {
		t.Fatalf("arrivals = %d, want 2 — two branches through one node must both survive", len(out.Arrivals))
	}
	if out.Arrivals[0].TokenID != "tok_1" || out.Arrivals[1].TokenID != "tok_2" {
		t.Errorf("arrival order = %s, %s; want tok_1 then tok_2", out.Arrivals[0].TokenID, out.Arrivals[1].TokenID)
	}
	if string(out.Arrivals[0].Output) != `{"n":1}` || string(out.Arrivals[1].Output) != `{"n":2}` {
		t.Errorf("arrival outputs = %s / %s, want the two distinct payloads",
			out.Arrivals[0].Output, out.Arrivals[1].Output)
	}
}

func TestJoinOutputRendersAMissingBranchOutputAsNull(t *testing.T) {
	raw, err := buildJoinOutput(postgres.JoinAggregate{
		Cardinality: 1,
		Arrivals:    []postgres.JoinAggregateRow{{FromNode: "route", TokenID: "tok_1", Outcome: "ship"}},
	}, "any")
	if err != nil {
		t.Fatalf("buildJoinOutput: %v", err)
	}
	// The whole payload must stay decodable: a raw-message field left empty
	// would produce invalid JSON, not an absent value.
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("join output is not valid JSON (%s): %v", raw, err)
	}
	arrivals := decoded["arrivals"].([]any)
	if got := arrivals[0].(map[string]any)["output"]; got != nil {
		t.Errorf("output = %v, want null", got)
	}
}

// An empty barrier cannot happen (a join work item exists only because the
// barrier satisfied, and satisfaction needs at least one arrival), but the
// renderer must still produce an empty ARRAY rather than JSON null, because
// a downstream guard indexing `output.arrivals` should fail on emptiness,
// not on a type error.
func TestJoinOutputWithNoArrivalsIsAnEmptyArray(t *testing.T) {
	raw, err := buildJoinOutput(postgres.JoinAggregate{Cardinality: 0}, "all")
	if err != nil {
		t.Fatalf("buildJoinOutput: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("join output is not valid JSON: %v", err)
	}
	arrivals, ok := decoded["arrivals"].([]any)
	if !ok {
		t.Fatalf("arrivals = %v, want an empty array", decoded["arrivals"])
	}
	if len(arrivals) != 0 {
		t.Errorf("arrivals = %v, want empty", arrivals)
	}
}
