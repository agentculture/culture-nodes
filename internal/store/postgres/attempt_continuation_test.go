package postgres_test

import (
	"context"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Per-attempt continuation carriage (task t4, spec claim c3 / honesty h2,
// docs/adr/0010-continuation-ref-on-request.md).
//
// The ref is the handle §13.2 lets an actor offer for continuing the
// conversation it just had. Before this task nothing persisted it, so a
// handle captured by one worker process died with that process. These tests
// pin the two halves that make it durable: the column exists and is
// nullable, and the ordinary InsertAttempt/Attempts round trip preserves it
// exactly — including its absence.

// TestMigration0018AddsNullableContinuationRefColumn is 0012's and 0017's
// test for migrations/0018: one nullable TEXT column with no default,
// expand-only (docs/adr/0002-migration-policy.md). NULL is what "this
// attempt reported no continuation ref" means, and a default would make
// that unrepresentable.
func TestMigration0018AddsNullableContinuationRefColumn(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	var dataType, nullable, hasDefault string
	err := s.Pool().QueryRow(ctx,
		`SELECT data_type, is_nullable, COALESCE(column_default, '')
		 FROM information_schema.columns
		 WHERE table_name = 'attempts' AND column_name = 'continuation_ref'`,
	).Scan(&dataType, &nullable, &hasDefault)
	if err != nil {
		t.Fatalf("attempts.continuation_ref: %v", err)
	}
	if dataType != "text" {
		t.Errorf("attempts.continuation_ref data_type = %q, want text", dataType)
	}
	if nullable != "YES" {
		t.Errorf("attempts.continuation_ref is_nullable = %q, want YES (expand-only: nullable)", nullable)
	}
	if hasDefault != "" {
		t.Errorf("attempts.continuation_ref has a default (%q), want none -- a default is not what 'no ref reported' means",
			hasDefault)
	}
}

// TestInsertAttemptRoundTripsContinuationRef proves the ref survives the
// ordinary write/read path unaltered, and that an attempt which reported
// none leaves the column NULL rather than an empty string: "" is a value a
// bridge could mistake for a handle, NULL is not.
func TestInsertAttemptRoundTripsContinuationRef(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()
	_, _, nodeRunID, _ := seedRun(t, es, ns.ID, "a")

	ref := "sess_01JQZCONTINUE"

	cases := []struct {
		name string
		ref  *string
	}{
		{name: "actor offered a continuation ref", ref: &ref},
		{name: "actor offered none: the column stays NULL", ref: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attemptID := store.NewULID()
			err := es.InTx(ctx, func(ctx context.Context, tx engine.Tx) error {
				number, err := tx.NextAttemptNumber(ctx, nodeRunID)
				if err != nil {
					return err
				}
				return tx.InsertAttempt(ctx, engine.Attempt{
					ID: attemptID, NamespaceID: ns.ID, NodeRunID: nodeRunID,
					Number: number, Status: engine.StatusSucceeded,
					ContinuationRef: tc.ref,
				})
			})
			if err != nil {
				t.Fatalf("InsertAttempt: %v", err)
			}

			got := findAttempt(t, es, nodeRunID, attemptID)
			if !equalStringPtr(got.ContinuationRef, tc.ref) {
				t.Errorf("ContinuationRef = %v, want %v",
					derefString(got.ContinuationRef), derefString(tc.ref))
			}

			if tc.ref != nil {
				return
			}
			var column any
			if err := requireStore(t).Pool().QueryRow(ctx,
				`SELECT continuation_ref FROM attempts WHERE id = $1`, attemptID,
			).Scan(&column); err != nil {
				t.Fatalf("read attempt row: %v", err)
			}
			if column != nil {
				t.Errorf("continuation_ref = %v, want NULL", column)
			}
		})
	}
}

// TestLatestContinuationRefIsScopedToRunAndActor pins the lookup dispatch
// uses to fill the outbound request (ADR 0010 §4). Its scope is deliberately
// narrow and this test is where that narrowness is enforced:
//
//   - the LATEST ref within the run wins, not the first one;
//   - an attempt that reported no ref does not shadow an earlier one that
//     did — the query skips NULLs rather than reading the newest row;
//   - a ref issued by a DIFFERENT actor is never returned: a handle is
//     meaningful only to the actor that issued it;
//   - a ref from a different run is not returned either. The cross-run
//     workstream session key (spec claim c3: actor + repo + workstream) is a
//     later task; until it exists, resuming across runs would resume a
//     conversation nothing declared it wanted resumed.
func TestLatestContinuationRefIsScopedToRunAndActor(t *testing.T) {
	es, ns := newEngineStore(t)
	s := requireStore(t)
	ctx := context.Background()

	runID, _, nodeRunID, _ := seedRun(t, es, ns.ID, "a")
	otherRunID, _, otherNodeRunID, _ := seedRun(t, es, ns.ID, "a")

	actorA := mustActorRow(t, s, ns.ID)
	actorB := mustActorRow(t, s, ns.ID)

	insert := func(nodeRun, actorID string, ref *string) {
		t.Helper()
		err := es.InTx(ctx, func(ctx context.Context, tx engine.Tx) error {
			number, err := tx.NextAttemptNumber(ctx, nodeRun)
			if err != nil {
				return err
			}
			return tx.InsertAttempt(ctx, engine.Attempt{
				ID: store.NewULID(), NamespaceID: ns.ID, NodeRunID: nodeRun,
				Number: number, ActorID: actorID, Status: engine.StatusSucceeded,
				ContinuationRef: ref,
			})
		})
		if err != nil {
			t.Fatalf("InsertAttempt: %v", err)
		}
	}
	ptr := func(v string) *string { return &v }

	// Nothing recorded yet: an absent ref is not an error.
	if ref, ok, err := s.LatestContinuationRef(ctx, runID, actorA); err != nil || ok || ref != "" {
		t.Fatalf("LatestContinuationRef on an empty run = (%q, %v, %v), want (\"\", false, nil)", ref, ok, err)
	}

	insert(nodeRunID, actorA, ptr("sess_first"))
	insert(nodeRunID, actorA, ptr("sess_second"))
	// A later attempt that reported nothing must not erase the live handle.
	insert(nodeRunID, actorA, nil)
	// Another actor's conversation, in the same run.
	insert(nodeRunID, actorB, ptr("sess_other_actor"))
	// The same actor, in another run.
	insert(otherNodeRunID, actorA, ptr("sess_other_run"))

	ref, ok, err := s.LatestContinuationRef(ctx, runID, actorA)
	if err != nil {
		t.Fatalf("LatestContinuationRef: %v", err)
	}
	if !ok || ref != "sess_second" {
		t.Errorf("LatestContinuationRef = (%q, %v), want (\"sess_second\", true)", ref, ok)
	}

	if ref, ok, err := s.LatestContinuationRef(ctx, runID, actorB); err != nil || !ok || ref != "sess_other_actor" {
		t.Errorf("LatestContinuationRef for actor B = (%q, %v, %v), want (\"sess_other_actor\", true, nil)", ref, ok, err)
	}

	if ref, ok, err := s.LatestContinuationRef(ctx, otherRunID, actorB); err != nil || ok {
		t.Errorf("LatestContinuationRef for a run actor B never ran in = (%q, %v, %v), want (\"\", false, nil)", ref, ok, err)
	}

	// An unattributed dispatch ("" actor id) resolves nothing: attempts.actor_id
	// is NULL there, and NULL is not an identity to match against.
	if ref, ok, err := s.LatestContinuationRef(ctx, runID, ""); err != nil || ok {
		t.Errorf("LatestContinuationRef for an unattributed dispatch = (%q, %v, %v), want (\"\", false, nil)", ref, ok, err)
	}
}

// mustActorRow inserts a registered agent actor, since attempts.actor_id is
// a foreign key into `actors`.
func mustActorRow(t *testing.T, s *postgres.Store, namespaceID string) string {
	t.Helper()
	actorID := "actor-" + store.NewULID()
	if _, err := s.Pool().Exec(context.Background(), `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		VALUES ($1, $2, $3, 1, 'agent', 'internal')
	`, actorID, namespaceID, actorID); err != nil {
		t.Fatalf("mustActorRow: %v", err)
	}
	return actorID
}
