package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The store half of task t16's per-actor concurrency ceiling (issue #166's
// second half, "one ticket per machine"): CountWaitingActorInvocations,
// proved against the real actor_invocations rows the real §12.6 async park
// path (StartAsyncWait) writes -- not a hand-inserted fixture, since the
// query's WHERE clause has to agree with exactly the columns and the
// exactly the state string that path writes.

// mustWaitingInvocation parks one work item against actorID, the same way
// async_test.go's newParkedItem does, except it also stamps ActorID
// (migration 0015) -- the identity CountWaitingActorInvocations counts by --
// which newParkedItem's own fixture leaves unattributed because none of
// that file's tests need it.
func mustWaitingInvocation(t *testing.T, s *postgres.Store, ns postgres.Namespace, actorID string) {
	t.Helper()
	ctx := context.Background()

	nodeRunID := mustNodeRun(t, s, ns.ID)
	var runID string
	if err := s.Pool().QueryRow(ctx, `SELECT run_id FROM node_runs WHERE id = $1`, nodeRunID).Scan(&runID); err != nil {
		t.Fatalf("read run id: %v", err)
	}
	mustEnqueued(t, s, ns.ID, nodeRunID)

	workerID := "worker-" + store.NewULID()
	claimed, err := s.ClaimWork(ctx, ns.ID, workerID, time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimWork returned %d items, want 1", len(claimed))
	}

	attemptID := "att_" + claimed[0].ID
	if err := s.StartAsyncWait(ctx, postgres.StartAsyncWaitInput{
		WorkID:       claimed[0].ID,
		WorkerID:     workerID,
		FencingToken: claimed[0].FencingToken,
		Attempt:      int(claimed[0].Attempt),
		NamespaceID:  ns.ID,
		RunID:        runID,
		NodeRunID:    nodeRunID,
		NodeID:       "intake",
		AttemptID:    attemptID,
		ActorID:      actorID,
		ActorRef:     "actor://company/worker@sha256:aaaaaa",
		InvocationID: "external_" + attemptID,
	}); err != nil {
		t.Fatalf("StartAsyncWait: %v", err)
	}
}

// TestCountWaitingActorInvocationsCountsOnlyThisActorsInFlightRows proves
// the query's whole shape: it counts the target actor's waiting_external
// rows, not another actor's in the same namespace, not a terminal row of
// the target actor's, and not a namespace it was not asked about.
func TestCountWaitingActorInvocationsCountsOnlyThisActorsInFlightRows(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "actor-concurrency")

	actorX := mustActorRow(t, s, ns.ID)
	actorY := mustActorRow(t, s, ns.ID)

	if count, err := s.CountWaitingActorInvocations(ctx, ns.ID, actorX); err != nil || count != 0 {
		t.Fatalf("CountWaitingActorInvocations(actorX) before any dispatch = (%d, %v), want (0, nil)", count, err)
	}

	mustWaitingInvocation(t, s, ns, actorX)
	mustWaitingInvocation(t, s, ns, actorX)
	mustWaitingInvocation(t, s, ns, actorY)

	countX, err := s.CountWaitingActorInvocations(ctx, ns.ID, actorX)
	if err != nil {
		t.Fatalf("CountWaitingActorInvocations(actorX): %v", err)
	}
	if countX != 2 {
		t.Fatalf("CountWaitingActorInvocations(actorX) = %d, want 2", countX)
	}

	countY, err := s.CountWaitingActorInvocations(ctx, ns.ID, actorY)
	if err != nil {
		t.Fatalf("CountWaitingActorInvocations(actorY): %v", err)
	}
	if countY != 1 {
		t.Fatalf("CountWaitingActorInvocations(actorY) = %d, want 1", countY)
	}

	// A different namespace never sees this namespace's in-flight rows, even
	// asking about the exact same actor row id (foreign-key-scoped to this
	// namespace, but the belt-and-suspenders namespace_id filter is what the
	// query actually leans on).
	other := mustNamespace(t, s, "actor-concurrency-other")
	if count, err := s.CountWaitingActorInvocations(ctx, other.ID, actorX); err != nil || count != 0 {
		t.Fatalf("CountWaitingActorInvocations in an unrelated namespace = (%d, %v), want (0, nil)", count, err)
	}
}

// TestCountWaitingActorInvocationsRequiresNamespaceAndActor pins the guard
// clause: an empty namespace or actor id is a caller bug, not "zero
// in-flight invocations".
func TestCountWaitingActorInvocationsRequiresNamespaceAndActor(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "actor-concurrency-guard")

	if _, err := s.CountWaitingActorInvocations(ctx, "", "actor-x"); err == nil {
		t.Error("want an error for an empty namespace id")
	}
	if _, err := s.CountWaitingActorInvocations(ctx, ns.ID, ""); err == nil {
		t.Error("want an error for an empty actor id")
	}
}
