package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The actor liveness row (migration 0058; plan loop-closure t10, decision
// c43's "both" shape). These pin the write rules the router's correctness
// rests on:
//
//  1. a collector write upserts the fact and leaves `locked` exactly as it
//     was — it can neither set nor clear the control-plane lock;
//  2. a worker write sets session_ok=false and locks;
//  3. resume clears ONLY the lock: session_ok stays whatever was last
//     observed, so resume alone never reopens a lane whose last fact is
//     false (c43's AND);
//  4. NULL session_ok survives the round trip as "unmeasured", never as
//     false.

func boolPtr(b bool) *bool { return &b }

func TestActorLivenessCollectorWriteUpsertsAndPreservesTheLock(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-actor-liveness-collector")
	checked := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)

	row, err := s.RecordActorLiveness(ctx, postgres.RecordActorLivenessInput{
		NamespaceID: ns.ID, ActorKey: "company/codex-a",
		SessionOK: boolPtr(true), Reason: "ok", Mode: "CHECK", CheckedAt: checked,
		Source: postgres.LivenessSourceCollector,
	})
	if err != nil {
		t.Fatalf("RecordActorLiveness: %v", err)
	}
	if row.SessionOK == nil || !*row.SessionOK || row.Reason != "ok" || row.Mode != "CHECK" || row.Locked || !row.CheckedAt.Equal(checked) || row.Source != postgres.LivenessSourceCollector {
		t.Fatalf("collector row = %+v, want session_ok=true reason=ok mode=CHECK unlocked", row)
	}

	// The worker locks it.
	locked, err := s.LockActorLiveness(ctx, postgres.LockActorLivenessInput{
		NamespaceID: ns.ID, ActorKey: "company/codex-a", Reason: "refresh_token_spent",
		CheckedAt: time.Now().UTC(), RunID: "run-1", AttemptID: "attempt-1",
	})
	if err != nil {
		t.Fatalf("LockActorLiveness: %v", err)
	}
	if !locked.Locked || locked.SessionOK == nil || *locked.SessionOK || locked.Reason != "refresh_token_spent" || locked.Source != postgres.LivenessSourceWorker || locked.LockedByRunID != "run-1" || locked.LockedByAttemptID != "attempt-1" {
		t.Fatalf("locked row = %+v, want locked session_ok=false reason=refresh_token_spent source=worker with provenance", locked)
	}

	// A later collector write that says the session is fine again updates
	// the fact but does NOT clear the lock — that is resume's job.
	again, err := s.RecordActorLiveness(ctx, postgres.RecordActorLivenessInput{
		NamespaceID: ns.ID, ActorKey: "company/codex-a",
		SessionOK: boolPtr(true), Reason: "ok", Mode: "LOCK", CheckedAt: time.Now().UTC(),
		Source: postgres.LivenessSourceCollector,
	})
	if err != nil {
		t.Fatalf("second RecordActorLiveness: %v", err)
	}
	if !again.Locked || again.SessionOK == nil || !*again.SessionOK || again.Mode != "LOCK" {
		t.Fatalf("row after collector write = %+v, want still locked with the new fact", again)
	}
	if again.LockedByRunID != "run-1" {
		t.Errorf("lock provenance = %q after a collector write, want preserved", again.LockedByRunID)
	}

	// Exactly one row.
	var count int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM actor_liveness WHERE namespace_id = $1`, ns.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("actor_liveness rows = %d, want 1 (an upsert, not an append)", count)
	}
}

func TestActorLivenessResumeClearsOnlyTheLock(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-actor-liveness-resume")
	if _, err := s.LockActorLiveness(ctx, postgres.LockActorLivenessInput{
		NamespaceID: ns.ID, ActorKey: "company/codex-b", Reason: "refresh_token_spent", CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("LockActorLiveness: %v", err)
	}

	es, err := postgres.NewEngineStore(s, ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	row, ok, err := es.UnlockActorLiveness(ctx, "company/codex-b")
	if err != nil || !ok {
		t.Fatalf("UnlockActorLiveness = (%v, %v)", ok, err)
	}
	if row.Locked {
		t.Error("locked = true after resume, want false")
	}
	if row.SessionOK == nil || *row.SessionOK {
		t.Errorf("session_ok = %v after resume, want still false: resume alone must not reopen a lane whose last fact is false", row.SessionOK)
	}
	if row.Source != postgres.LivenessSourceResume {
		t.Errorf("source = %q after resume, want %q", row.Source, postgres.LivenessSourceResume)
	}

	// Unlocking an actor with no row is (zero, false, nil): nothing to clear.
	if _, ok, err := es.UnlockActorLiveness(ctx, "company/never-observed"); err != nil || ok {
		t.Fatalf("UnlockActorLiveness on a missing row = (%v, %v), want (false, nil)", ok, err)
	}

	// The reader surfaces agree.
	got, ok, err := s.ActorLiveness(ctx, ns.ID, "company/codex-b")
	if err != nil || !ok || got.Locked {
		t.Fatalf("ActorLiveness = (%+v, %v, %v)", got, ok, err)
	}
	all, err := es.ActorLivenessAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := all["company/codex-b"]; !present || len(all) != 1 {
		t.Fatalf("ActorLivenessAll = %v, want exactly the one key", all)
	}
}

func TestActorLivenessUnmeasuredStaysNull(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-actor-liveness-null")
	row, err := s.RecordActorLiveness(ctx, postgres.RecordActorLivenessInput{
		NamespaceID: ns.ID, ActorKey: "company/codex-c",
		SessionOK: nil, Reason: "unmeasured", Mode: "LOCK", CheckedAt: time.Now().UTC(),
		Source: postgres.LivenessSourceCollector,
	})
	if err != nil {
		t.Fatalf("RecordActorLiveness: %v", err)
	}
	if row.SessionOK != nil {
		t.Fatalf("session_ok = %v, want nil (unmeasured is neither true nor false)", *row.SessionOK)
	}
	if _, err := s.RecordActorLiveness(ctx, postgres.RecordActorLivenessInput{
		NamespaceID: ns.ID, ActorKey: "company/codex-c", Reason: "ok", CheckedAt: time.Now().UTC(), Source: "bridge",
	}); err == nil {
		t.Fatal("an unknown source was accepted; the column is CHECK-constrained and the wrapper must refuse it first")
	}
}
