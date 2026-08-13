package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The capacity circuit breaker's durable state (task t9, spec claim c4,
// honesty conditions h3/h38).
//
// These tests pin the four properties the dispatch site depends on:
//
//  1. the trip is an idempotent UPSERT -- one row per (namespace, actor
//     key), however many workers trip it;
//  2. concurrent trips never error and never shorten a pause another worker
//     already committed;
//  3. a pause EXPIRES: presence of a row is not the predicate, paused_until
//     against now() is;
//  4. an operator can clear a pause early, and the clear stays explainable
//     afterwards (cleared_at/cleared_by survive, they are not a delete).

func mustPause(t *testing.T, s *postgres.Store, in postgres.PauseActorInput) postgres.ActorPause {
	t.Helper()
	pause, err := s.PauseActor(context.Background(), in)
	if err != nil {
		t.Fatalf("PauseActor: %v", err)
	}
	return pause
}

func TestPauseActorIsAnIdempotentUpsert(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-actor-availability-upsert")

	first := mustPause(t, s, postgres.PauseActorInput{
		NamespaceID: ns.ID,
		ActorKey:    "company/analyzer",
		PausedUntil: time.Now().UTC().Add(5 * time.Minute),
		Reason:      "capacity_exhausted",
		Detail:      "first trip",
	})
	second := mustPause(t, s, postgres.PauseActorInput{
		NamespaceID: ns.ID,
		ActorKey:    "company/analyzer",
		PausedUntil: time.Now().UTC().Add(30 * time.Minute),
		Reason:      "capacity_exhausted",
		Detail:      "second trip",
	})

	var rows int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM actor_availability WHERE namespace_id = $1 AND actor_key = $2`,
		ns.ID, "company/analyzer").Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("actor_availability rows = %d, want 1: a second trip must upsert, not append", rows)
	}
	if !second.PausedUntil.After(first.PausedUntil) {
		t.Errorf("second trip's paused_until = %s, want later than the first (%s)",
			second.PausedUntil, first.PausedUntil)
	}
	if second.Detail != "second trip" {
		t.Errorf("detail = %q, want the extending trip's own detail", second.Detail)
	}
}

// A concurrent trip may EXTEND a pause; it must never shorten one. The
// shorter deadline is the dangerous direction -- it would let dispatch
// resume while the provider is still refusing -- so the upsert keeps the
// later paused_until whichever writer lands second.
func TestPauseActorKeepsTheLaterDeadlineUnderConcurrency(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-actor-availability-concurrent")

	base := time.Now().UTC()
	deadlines := []time.Duration{time.Minute, 45 * time.Minute, 5 * time.Minute, 20 * time.Minute}

	var wg sync.WaitGroup
	errs := make([]error, len(deadlines))
	for i, d := range deadlines {
		wg.Add(1)
		go func(i int, d time.Duration) {
			defer wg.Done()
			_, errs[i] = s.PauseActor(ctx, postgres.PauseActorInput{
				NamespaceID: ns.ID,
				ActorKey:    "company/racer",
				PausedUntil: base.Add(d),
				Reason:      "capacity_exhausted",
			})
		}(i, d)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent PauseActor %d: %v", i, err)
		}
	}

	pause, ok, err := s.ActorPause(ctx, ns.ID, "company/racer")
	if err != nil || !ok {
		t.Fatalf("ActorPause: (%v, %v)", ok, err)
	}
	want := base.Add(45 * time.Minute)
	if pause.PausedUntil.Sub(want).Abs() > time.Second {
		t.Errorf("paused_until = %s, want the longest concurrent deadline %s", pause.PausedUntil, want)
	}
}

// A pause is a deadline, not a flag: once paused_until passes, the actor is
// dispatchable again with nothing to clean up. ActivePause is the predicate
// the dispatch site uses, and it must answer false for an expired row that
// is still sitting there as history.
func TestActivePauseExpires(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-actor-availability-expiry")

	mustPause(t, s, postgres.PauseActorInput{
		NamespaceID: ns.ID,
		ActorKey:    "company/expired",
		PausedUntil: time.Now().UTC().Add(-time.Second),
		Reason:      "capacity_exhausted",
	})

	if _, ok, err := s.ActivePause(ctx, ns.ID, "company/expired"); err != nil || ok {
		t.Errorf("ActivePause on an expired pause = (%v, %v), want (false, nil)", ok, err)
	}
	// The row itself is still there -- expiry is not a delete, so the
	// history of why an actor was paused survives the pause.
	if _, ok, err := s.ActorPause(ctx, ns.ID, "company/expired"); err != nil || !ok {
		t.Errorf("ActorPause after expiry = (%v, %v), want the row still readable", ok, err)
	}

	mustPause(t, s, postgres.PauseActorInput{
		NamespaceID: ns.ID,
		ActorKey:    "company/live",
		PausedUntil: time.Now().UTC().Add(time.Hour),
		Reason:      "capacity_exhausted",
	})
	if _, ok, err := s.ActivePause(ctx, ns.ID, "company/live"); err != nil || !ok {
		t.Errorf("ActivePause on a live pause = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestClearActorPauseIsReversibleAndExplainable(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-actor-availability-clear")

	mustPause(t, s, postgres.PauseActorInput{
		NamespaceID: ns.ID,
		ActorKey:    "company/paused",
		PausedUntil: time.Now().UTC().Add(2 * time.Hour),
		Reason:      "capacity_exhausted",
		RetryAfter:  90 * time.Second,
		Detail:      "provider quota exhausted",
		RunID:       "run-1",
		NodeRunID:   "nr-1",
		AttemptID:   "att-1",
		WorkID:      "work-1",
	})

	cleared, ok, err := s.ClearActorPause(ctx, ns.ID, "company/paused", "ori")
	if err != nil {
		t.Fatalf("ClearActorPause: %v", err)
	}
	if !ok {
		t.Fatal("ClearActorPause reported no pause to clear")
	}
	if cleared.ClearedBy != "ori" || cleared.ClearedAt == nil {
		t.Errorf("cleared pause = %+v, want cleared_by/cleared_at recorded", cleared)
	}
	if _, active, err := s.ActivePause(ctx, ns.ID, "company/paused"); err != nil || active {
		t.Errorf("ActivePause after a clear = (%v, %v), want (false, nil)", active, err)
	}
	// The provenance of the pause that WAS cleared is still readable: an
	// operator asking "why was this paused and who let it back in" gets both
	// halves from one row.
	row, ok, err := s.ActorPause(ctx, ns.ID, "company/paused")
	if err != nil || !ok {
		t.Fatalf("ActorPause after clear: (%v, %v)", ok, err)
	}
	if row.Reason != "capacity_exhausted" || row.TrippedByAttemptID != "att-1" {
		t.Errorf("cleared row lost its provenance: %+v", row)
	}
	if row.RetryAfterSeconds == nil || *row.RetryAfterSeconds != 90 {
		t.Errorf("retry_after_seconds = %v, want 90", row.RetryAfterSeconds)
	}

	// Clearing something that is not paused is not an error -- it is a no-op
	// an operator may safely repeat.
	if _, ok, err := s.ClearActorPause(ctx, ns.ID, "company/never-paused", "ori"); err != nil || ok {
		t.Errorf("ClearActorPause on an unpaused actor = (%v, %v), want (false, nil)", ok, err)
	}
}

// A provider that named no Retry-After leaves the column NULL, never 0: a
// zero there would read as "retry immediately", which is the opposite of
// what an unstated hint means.
func TestPauseWithoutRetryAfterStaysNull(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-actor-availability-null-retry")

	mustPause(t, s, postgres.PauseActorInput{
		NamespaceID: ns.ID,
		ActorKey:    "company/silent",
		PausedUntil: time.Now().UTC().Add(time.Hour),
		Reason:      "capacity_exhausted",
	})
	pause, ok, err := s.ActivePause(ctx, ns.ID, "company/silent")
	if err != nil || !ok {
		t.Fatalf("ActivePause: (%v, %v)", ok, err)
	}
	if pause.RetryAfterSeconds != nil {
		t.Errorf("retry_after_seconds = %v, want nil when the provider named none", *pause.RetryAfterSeconds)
	}
}

// ListActivePauses is what the actors read surface joins against: only
// currently-paused actors, and only in this namespace.
func TestListActivePauses(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-actor-availability-list")
	other := mustNamespace(t, s, "test-actor-availability-list-other")

	mustPause(t, s, postgres.PauseActorInput{
		NamespaceID: ns.ID, ActorKey: "company/live", Reason: "capacity_exhausted",
		PausedUntil: time.Now().UTC().Add(time.Hour),
	})
	mustPause(t, s, postgres.PauseActorInput{
		NamespaceID: ns.ID, ActorKey: "company/expired", Reason: "capacity_exhausted",
		PausedUntil: time.Now().UTC().Add(-time.Hour),
	})
	mustPause(t, s, postgres.PauseActorInput{
		NamespaceID: other.ID, ActorKey: "company/elsewhere", Reason: "capacity_exhausted",
		PausedUntil: time.Now().UTC().Add(time.Hour),
	})

	pauses, err := s.ListActivePauses(ctx, ns.ID)
	if err != nil {
		t.Fatalf("ListActivePauses: %v", err)
	}
	if len(pauses) != 1 || pauses[0].ActorKey != "company/live" {
		t.Fatalf("ListActivePauses = %+v, want only company/live", pauses)
	}
}
