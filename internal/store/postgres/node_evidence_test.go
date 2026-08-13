package postgres_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The node-run-scoped evidence selector (task t7): /nodes/<id>/evidence
// resolves the run's live evidence records belonging to that node's node
// runs — identity is node_run_id, which the engine stamps on every accepted
// delta record; node evidence carries no SubjectRef. These tests prove the
// join over real rows: same run, joined through node_runs on run_id +
// node_key, evidence records only, live records only, id order.

func insertNodeRun(t *testing.T, f *ledgerFixture, nodeKey string) string {
	t.Helper()
	id := "nr_" + store.NewULID()
	if _, err := f.store.Pool().Exec(context.Background(),
		`INSERT INTO node_runs (id, namespace_id, run_id, node_key) VALUES ($1, $2, $3, $4)`,
		id, f.namespaceID, f.runID, nodeKey); err != nil {
		t.Fatalf("insert node run %s: %v", nodeKey, err)
	}
	return id
}

// nodeEvidence is an agent-proposed evidence record attached to a node run —
// the shape a node's accepted ledger delta leaves behind. No SubjectRef:
// node evidence is identified by its node run, not by a subject.
func (f *ledgerFixture) nodeEvidence(nodeRunID, method string) ledger.Record {
	return ledger.Record{
		RecordType: ledger.RecordEvidence,
		RunID:      f.runID,
		NodeRunID:  ledger.NullableID(nodeRunID),
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: f.agentActor},
		Authority:  ledger.AuthorityProposed,
		Data:       json.RawMessage(`{"collection_method":` + mustQuote(method) + `,"completeness":"partial"}`),
	}
}

func TestNodeEvidenceSelectsOneNodesLiveRecords(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, "node-evidence")

	gather := insertNodeRun(t, f, "gather")
	distract := insertNodeRun(t, f, "distract")

	mustAppend := func(rec ledger.Record) ledger.Record {
		t.Helper()
		appended, err := f.ledger.Append(ctx, rec)
		if err != nil {
			t.Fatalf("append %s: %v", rec.RecordType, err)
		}
		return appended
	}

	evFirst := mustAppend(f.nodeEvidence(gather, "agent_self_report"))

	// A non-evidence record on the same node run must not be selected.
	claim := f.claim(t, "gather says it is done")
	claim.NodeRunID = ledger.NullableID(gather)
	mustAppend(claim)

	// Another node's evidence in the same run must not leak in.
	evOtherNode := mustAppend(f.nodeEvidence(distract, "agent_self_report"))

	// A superseded record is not live; its correction is.
	evStale := mustAppend(f.nodeEvidence(gather, "misread"))
	evCorrection, err := f.ledger.AppendSuperseding(ctx, f.nodeEvidence(gather, "corrected"), evStale.ID)
	if err != nil {
		t.Fatalf("append superseding evidence: %v", err)
	}

	// Evidence attached to no node run belongs to no node's surface.
	mustAppend(f.nodeEvidence("", "runless"))

	got, err := f.store.NodeEvidence(ctx, f.runID, "gather")
	if err != nil {
		t.Fatalf("NodeEvidence(gather): %v", err)
	}
	if len(got) != 2 || got[0].ID != evFirst.ID || got[1].ID != evCorrection.ID {
		t.Fatalf("NodeEvidence(gather) = %v, want [%s %s] in id order",
			recordIDs(got), evFirst.ID, evCorrection.ID)
	}
	for _, rec := range got {
		if rec.NodeRunID.String() != gather {
			t.Errorf("record %s carries node_run_id %q, want %q", rec.ID, rec.NodeRunID, gather)
		}
		if rec.SubjectRef.String() != "" {
			t.Errorf("record %s carries subject_ref %q; node evidence has none", rec.ID, rec.SubjectRef)
		}
	}

	other, err := f.store.NodeEvidence(ctx, f.runID, "distract")
	if err != nil {
		t.Fatalf("NodeEvidence(distract): %v", err)
	}
	if len(other) != 1 || other[0].ID != evOtherNode.ID {
		t.Fatalf("NodeEvidence(distract) = %v, want [%s]", recordIDs(other), evOtherNode.ID)
	}

	// A node with no evidence — or no node runs at all — answers an empty
	// slice, never an error: zero records is the true answer.
	empty, err := f.store.NodeEvidence(ctx, f.runID, "absent")
	if err != nil {
		t.Fatalf("NodeEvidence(absent): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("NodeEvidence(absent) = %v, want none", recordIDs(empty))
	}
}

// The engine's transaction surface runs the same statement, so an end node's
// output binding inside a completion sees exactly what a worker resolving a
// dispatch binding outside one sees.
func TestNodeEvidenceAnswersTheSameInsideAnEngineTransaction(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, "node-evidence-tx")

	gather := insertNodeRun(t, f, "gather")
	appended, err := f.ledger.Append(ctx, f.nodeEvidence(gather, "agent_self_report"))
	if err != nil {
		t.Fatalf("append evidence: %v", err)
	}

	es, err := postgres.NewEngineStore(f.store, f.namespaceID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	var inTx []ledger.Record
	err = es.InTx(ctx, func(ctx context.Context, tx engine.Tx) error {
		var err error
		inTx, err = tx.NodeEvidence(ctx, f.runID, "gather")
		return err
	})
	if err != nil {
		t.Fatalf("NodeEvidence inside the engine transaction: %v", err)
	}

	outside, err := f.store.NodeEvidence(ctx, f.runID, "gather")
	if err != nil {
		t.Fatalf("NodeEvidence outside a transaction: %v", err)
	}
	if len(inTx) != 1 || inTx[0].ID != appended.ID {
		t.Fatalf("in-transaction answer = %v, want [%s]", recordIDs(inTx), appended.ID)
	}
	if len(outside) != len(inTx) || outside[0].ID != inTx[0].ID {
		t.Errorf("outside = %v, inside = %v; the two surfaces must give the same answer",
			recordIDs(outside), recordIDs(inTx))
	}
}
