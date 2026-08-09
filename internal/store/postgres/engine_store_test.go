package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// These tests are about the store's half of the engine's contract — the parts
// internal/engine's own suite exercises only indirectly: how a definition is
// published and re-resolved, and how the counts the §9.7 loop bounds depend on
// are *derived* rather than kept in counter columns.

// seedRun creates a run with its entry node run inside one transaction, the
// way engine.CreateRun does, and returns the ids.
func seedRun(t *testing.T, es *postgres.EngineStore, namespaceID, entry string) (runID, tokenID, nodeRunID, versionID string) {
	t.Helper()
	ctx := context.Background()

	runID = store.NewULID()
	tokenID = store.NewULID()
	nodeRunID = store.NewULID()

	err := es.InTx(ctx, func(ctx context.Context, tx engine.Tx) error {
		var err error
		versionID, err = tx.EnsureWorkflowVersion(ctx, engine.WorkflowVersionInput{
			WorkflowKey:   "derived-counts",
			SourceFormat:  "yaml",
			Source:        "apiVersion: nodes.culture.dev/v1alpha1\n",
			NormalizedIR:  json.RawMessage(`{"spec":{"entry":"` + entry + `"}}`),
			ContentDigest: "sha256:" + store.NewULID(),
		})
		if err != nil {
			return err
		}
		if err := tx.InsertRun(ctx, engine.Run{
			ID:                runID,
			NamespaceID:       namespaceID,
			WorkflowVersionID: versionID,
			State:             engine.RunRunning,
			Input:             json.RawMessage(`{}`),
			CreatedAt:         time.Now().UTC(),
		}); err != nil {
			return err
		}
		if err := tx.InsertToken(ctx, engine.Token{
			ID: tokenID, NamespaceID: namespaceID, RunID: runID, NodeID: entry, State: engine.TokenActive,
		}); err != nil {
			return err
		}
		return tx.InsertNodeRun(ctx, engine.NodeRun{
			ID: nodeRunID, NamespaceID: namespaceID, RunID: runID, TokenID: tokenID,
			NodeID: entry, State: engine.NodeRunReady, VisitCount: 1,
		})
	})
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID, tokenID, nodeRunID, versionID
}

func newEngineStore(t *testing.T) (*postgres.EngineStore, postgres.Namespace) {
	t.Helper()
	s := requireStore(t)
	ns := mustNamespace(t, s, "engine-store")
	es, err := postgres.NewEngineStore(s, ns.ID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	return es, ns
}

// A digest resolves to one immutable version row however many times it is
// published, and a second definition of the same workflow key gets the next
// version number.
func TestEnsureWorkflowVersionIsIdempotentByDigest(t *testing.T) {
	es, _ := newEngineStore(t)
	ctx := context.Background()

	digest := "sha256:" + store.NewULID()
	in := engine.WorkflowVersionInput{
		WorkflowKey:   "idempotent",
		SourceFormat:  "yaml",
		Source:        "kind: Workflow\n",
		NormalizedIR:  json.RawMessage(`{"spec":{"entry":"a"}}`),
		ContentDigest: digest,
	}

	var first, second, other string
	err := es.InTx(ctx, func(ctx context.Context, tx engine.Tx) error {
		var err error
		if first, err = tx.EnsureWorkflowVersion(ctx, in); err != nil {
			return err
		}
		if second, err = tx.EnsureWorkflowVersion(ctx, in); err != nil {
			return err
		}
		next := in
		next.ContentDigest = "sha256:" + store.NewULID()
		next.NormalizedIR = json.RawMessage(`{"spec":{"entry":"b"}}`)
		other, err = tx.EnsureWorkflowVersion(ctx, next)
		return err
	})
	if err != nil {
		t.Fatalf("EnsureWorkflowVersion: %v", err)
	}

	if first != second {
		t.Errorf("the same digest resolved to two versions: %s and %s", first, second)
	}
	if other == first {
		t.Error("a different digest should be a different version")
	}

	// The IR is readable back, which is what a restart depends on.
	err = es.InTx(ctx, func(ctx context.Context, tx engine.Tx) error {
		gotDigest, ir, err := tx.WorkflowIR(ctx, first)
		if err != nil {
			return err
		}
		if gotDigest != digest {
			t.Errorf("digest = %q, want %q", gotDigest, digest)
		}
		if string(ir) != `{"spec": {"entry": "a"}}` && string(ir) != `{"spec":{"entry":"a"}}` {
			t.Errorf("normalized IR = %s, want the bytes that were published", ir)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WorkflowIR: %v", err)
	}
}

func TestWorkflowIRReportsAMissingVersion(t *testing.T) {
	es, _ := newEngineStore(t)

	err := es.InTx(context.Background(), func(ctx context.Context, tx engine.Tx) error {
		_, _, err := tx.WorkflowIR(ctx, "no-such-version")
		if !errors.Is(err, engine.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
}

// The transition and visit counts the loop bounds are enforced against are
// derived from node_runs. This proves the derivation: the entry node run is
// not a transition, and every node run after it is.
func TestTransitionAndVisitCountsAreDerivedFromNodeRuns(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()
	runID, _, _, _ := seedRun(t, es, ns.ID, "a")

	err := es.InTx(ctx, func(ctx context.Context, tx engine.Tx) error {
		count, err := tx.TransitionCount(ctx, runID)
		if err != nil {
			return err
		}
		if count != 0 {
			t.Errorf("a run at its entry node has taken %d transitions, want 0", count)
		}

		// Two more node runs: one revisiting `a`, one at `b`.
		for _, node := range []string{"b", "a"} {
			if err := tx.InsertNodeRun(ctx, engine.NodeRun{
				ID: store.NewULID(), NamespaceID: ns.ID, RunID: runID,
				NodeID: node, State: engine.NodeRunReady, VisitCount: 2,
			}); err != nil {
				return err
			}
		}

		if count, err = tx.TransitionCount(ctx, runID); err != nil {
			return err
		}
		if count != 2 {
			t.Errorf("transitions = %d, want 2", count)
		}

		visits, err := tx.NodeVisits(ctx, runID)
		if err != nil {
			return err
		}
		if visits["a"] != 2 || visits["b"] != 1 {
			t.Errorf("visits = %v, want a:2 b:1", visits)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
}

// A node with no succeeded attempt has no output, and saying so with a nil
// rather than an empty JSON document is what lets a binding fail loudly
// instead of resolving to something that looks like an answer.
func TestNodeOutputOnlyReadsSucceededAttempts(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()
	runID, _, nodeRunID, _ := seedRun(t, es, ns.ID, "a")

	err := es.InTx(ctx, func(ctx context.Context, tx engine.Tx) error {
		output, err := tx.NodeOutput(ctx, runID, "a")
		if err != nil {
			return err
		}
		if output != nil {
			t.Errorf("output = %s, want nil for a node with no attempts", output)
		}

		number, err := tx.NextAttemptNumber(ctx, nodeRunID)
		if err != nil {
			return err
		}
		if number != 1 {
			t.Errorf("first attempt number = %d, want 1", number)
		}
		if err := tx.InsertAttempt(ctx, engine.Attempt{
			ID: store.NewULID(), NamespaceID: ns.ID, NodeRunID: nodeRunID,
			Number: number, Status: engine.StatusFailed, Result: json.RawMessage(`{"partial":true}`),
		}); err != nil {
			return err
		}

		if output, err = tx.NodeOutput(ctx, runID, "a"); err != nil {
			return err
		}
		if output != nil {
			t.Errorf("output = %s; a failed attempt produced no output a binding may read", output)
		}

		if number, err = tx.NextAttemptNumber(ctx, nodeRunID); err != nil {
			return err
		}
		if number != 2 {
			t.Errorf("second attempt number = %d, want 2", number)
		}
		if err := tx.InsertAttempt(ctx, engine.Attempt{
			ID: store.NewULID(), NamespaceID: ns.ID, NodeRunID: nodeRunID,
			Number: number, Status: engine.StatusSucceeded, Result: json.RawMessage(`{"ok":true}`),
		}); err != nil {
			return err
		}

		if output, err = tx.NodeOutput(ctx, runID, "a"); err != nil {
			return err
		}
		if string(output) != `{"ok": true}` && string(output) != `{"ok":true}` {
			t.Errorf("output = %s, want the succeeded attempt's result", output)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
}

// The engine's fenced completion is the same guard Store.CompleteWork
// applies, and it reports a failed guard as both sentinels.
func TestEngineCompleteWorkReportsBothStaleSentinels(t *testing.T) {
	es, ns := newEngineStore(t)
	s := requireStore(t)
	ctx := context.Background()
	_, _, nodeRunID, _ := seedRun(t, es, ns.ID, "a")

	if err := s.EnqueueWork(ctx, postgres.WorkItem{NamespaceID: ns.ID, NodeRunID: nodeRunID}); err != nil {
		t.Fatalf("EnqueueWork: %v", err)
	}
	var claimed postgres.ClaimedWork
	for attempt := 0; attempt < 10 && claimed.ID == ""; attempt++ {
		items, err := s.ClaimWork(ctx, ns.ID, "engine-store-worker", time.Minute, 20)
		if err != nil {
			t.Fatalf("ClaimWork: %v", err)
		}
		for _, item := range items {
			if item.NodeRunID == nodeRunID {
				claimed = item
			}
		}
	}
	if claimed.ID == "" {
		t.Fatal("the enqueued work item was never claimable")
	}

	err := es.InTx(ctx, func(ctx context.Context, tx engine.Tx) error {
		// The right tuple commits.
		if err := tx.CompleteWork(ctx, claimed.ID, "engine-store-worker", claimed.FencingToken, int(claimed.Attempt)); err != nil {
			t.Fatalf("CompleteWork with the current claim: %v", err)
		}
		// A stale fencing token does not, and says so in both vocabularies.
		err := tx.CompleteWork(ctx, claimed.ID, "engine-store-worker", claimed.FencingToken-1, int(claimed.Attempt))
		if !errors.Is(err, engine.ErrStaleClaim) {
			t.Errorf("err = %v, want engine.ErrStaleClaim", err)
		}
		if !errors.Is(err, postgres.ErrStaleClaim) {
			t.Errorf("err = %v, want postgres.ErrStaleClaim", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
}

// A transaction that returns an error applies none of its writes — the
// property every "nothing was committed" claim in the engine rests on.
func TestEngineInTxRollsBackEverything(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()
	runID, _, _, _ := seedRun(t, es, ns.ID, "a")

	sentinel := errors.New("abort")
	err := es.InTx(ctx, func(ctx context.Context, tx engine.Tx) error {
		if err := tx.InsertNodeRun(ctx, engine.NodeRun{
			ID: store.NewULID(), NamespaceID: ns.ID, RunID: runID,
			NodeID: "b", State: engine.NodeRunReady, VisitCount: 1,
		}); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(ctx, runID, engine.EventInput{
			Type: "dev.culture.nodes.test.event",
			Data: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx err = %v, want the sentinel", err)
	}

	var nodeRuns, events int
	if err := requireStore(t).Pool().QueryRow(ctx,
		`SELECT (SELECT COUNT(*) FROM node_runs WHERE run_id = $1)::int,
		        (SELECT COUNT(*) FROM events WHERE aggregate_id = $1)::int`, runID,
	).Scan(&nodeRuns, &events); err != nil {
		t.Fatalf("count: %v", err)
	}
	if nodeRuns != 1 {
		t.Errorf("%d node runs survived a rolled-back transaction, want 1 (the seeded one)", nodeRuns)
	}
	if events != 0 {
		t.Errorf("%d events survived a rolled-back transaction, want 0", events)
	}
}
