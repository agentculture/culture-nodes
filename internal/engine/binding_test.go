package engine

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// The engine's resolver is tested in-package against a stub Tx: what is
// under test is which surfaces it answers and how, not the store's queries —
// those are exercised by internal/store/postgres and the worker's
// end-to-end suite. The stub embeds the Tx interface, so a resolver that
// starts calling a method the stub does not implement panics loudly instead
// of silently widening this test's surface.

type bindingStubTx struct {
	Tx
	outputs  map[string]json.RawMessage
	evidence map[string][]ledger.Record
}

func (s bindingStubTx) NodeOutput(_ context.Context, _, nodeID string) (json.RawMessage, error) {
	return s.outputs[nodeID], nil
}

func (s bindingStubTx) NodeEvidence(_ context.Context, _, nodeID string) ([]ledger.Record, error) {
	return s.evidence[nodeID], nil
}

func bindingFixture() (Tx, Run) {
	tx := bindingStubTx{
		outputs: map[string]json.RawMessage{
			"analyze": json.RawMessage(`{"score":0.91}`),
		},
		evidence: map[string][]ledger.Record{
			"analyze": {
				{
					ID:         ledger.IDPrefix + "00000000000000000000000001",
					RecordType: ledger.RecordEvidence,
					RunID:      "run_1",
					NodeRunID:  ledger.NullableID("nr_00000000000000000000000001"),
					Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: "actor_agent"},
					Authority:  ledger.AuthorityProposed,
					Data:       json.RawMessage(`{"collection_method":"agent_self_report","completeness":"partial"}`),
				},
			},
		},
	}
	run := Run{ID: "run_1", Input: json.RawMessage(`{"subject":"widget"}`)}
	return tx, run
}

func TestEngineResolverAnswersItsSurfaces(t *testing.T) {
	tx, run := bindingFixture()

	cases := []struct {
		name    string
		pointer string
		want    string
	}{
		{"run input member", "/run/input/subject", `"widget"`},
		{"node output", "/nodes/analyze/output", `{"score":0.91}`},
		{"member of node output", "/nodes/analyze/output/score", `0.91`},
		{"into the evidence array", "/nodes/analyze/evidence/0/data/completeness", `"partial"`},
		{"evidence of a node that appended none", "/nodes/silent/evidence", `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveBinding(context.Background(), tx, run, tc.pointer)
			if err != nil {
				t.Fatalf("resolve %s: %v", tc.pointer, err)
			}
			var a, b any
			if err := json.Unmarshal(got, &a); err != nil {
				t.Fatalf("decode %s: %v", got, err)
			}
			if err := json.Unmarshal([]byte(tc.want), &b); err != nil {
				t.Fatalf("decode %s: %v", tc.want, err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Errorf("resolved = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestEngineResolverEvidenceIsThatNodesRecords is t7's engine half: an end
// node's OutputFrom may bind /nodes/<id>/evidence and receives exactly that
// node's evidence records, identified by node run.
func TestEngineResolverEvidenceIsThatNodesRecords(t *testing.T) {
	tx, run := bindingFixture()

	got, err := resolveBinding(context.Background(), tx, run, "/nodes/analyze/evidence")
	if err != nil {
		t.Fatalf("resolve /nodes/analyze/evidence: %v", err)
	}
	var records []ledger.Record
	if err := json.Unmarshal(got, &records); err != nil {
		t.Fatalf("decode resolved evidence: %v", err)
	}
	if len(records) != 1 || records[0].ID != ledger.IDPrefix+"00000000000000000000000001" {
		t.Fatalf("resolved %d records (%v), want exactly the node's evidence record", len(records), records)
	}
	if records[0].NodeRunID.String() != "nr_00000000000000000000000001" {
		t.Errorf("node_run_id = %q; the node run is evidence's identity", records[0].NodeRunID)
	}
}

// TestEngineResolverNodeSurfaceSetMatchesTheWorkers pins the two resolvers to
// one node-surface vocabulary (t7's agreement, engine side): the surfaces the
// compiler accepts (output, evidence) are answered, and the deferred ones
// (artifacts, error) are refused with the same loud verdict the compiler
// gives — internal/worker's TestCompilerAndResolverAgreeOnNodeBindingSurfaces
// proves the compiler half against the same set.
func TestEngineResolverNodeSurfaceSetMatchesTheWorkers(t *testing.T) {
	tx, run := bindingFixture()

	answered := map[string]bool{
		"output":    true,
		"evidence":  true,
		"artifacts": false,
		"error":     false,
	}
	for surface, want := range answered {
		t.Run(surface, func(t *testing.T) {
			_, err := resolveBinding(context.Background(), tx, run, "/nodes/analyze/"+surface)
			refused := err != nil && strings.Contains(err.Error(), "not resolvable")
			if got := !refused; got != want {
				t.Errorf("engine resolver answers /nodes/<id>/%s: %v, want %v (err: %v)", surface, got, want, err)
			}
		})
	}
}
