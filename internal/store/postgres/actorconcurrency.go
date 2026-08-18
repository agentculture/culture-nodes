package postgres

import (
	"context"
	"errors"
	"fmt"
)

// CountWaitingActorInvocations is the durable half of task t16's per-actor
// concurrency ceiling (issue #166's second half, "one ticket per machine"):
// how many asynchronous invocations of one actor are currently in flight,
// counted the only way that fact is durable at all.
//
// It counts actor_invocations rows in state 'waiting_external' -- the
// window between a §13.3 async acceptance and its terminal callback (see
// migration 0009's doc comment: "A synchronous invocation needs no durable
// row... between the actor's 202 and its terminal callback there is no
// process anywhere holding anything about the invocation in memory. ...it
// lives here."). That is a real, INTENTIONAL boundary of what this counts,
// not an oversight: a synchronous dispatch that is still blocked in
// Client.Invoke has no row here, the same documented gap breaker.go states
// for its own capacity trip ("Only the SYNCHRONOUS dispatch path trips the
// breaker... This is a real gap, not a decision that async exhaustion does
// not matter"). It is an acceptable gap for THIS ceiling specifically
// because the actors "one ticket per machine" exists for are exactly the
// ones long enough to be dispatched asynchronously by policy (see
// internal/worker/concurrency.go's package doc comment for where the
// coverage stops and why closing it is deeper placement work than this
// task's seam).
func (s *Store) CountWaitingActorInvocations(ctx context.Context, namespaceID, actorID string) (int, error) {
	if namespaceID == "" || actorID == "" {
		return 0, errors.New("postgres: CountWaitingActorInvocations requires namespaceID and actorID")
	}
	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)::int FROM actor_invocations
		WHERE namespace_id = $1 AND actor_id = $2 AND state = 'waiting_external'
	`, namespaceID, actorID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("postgres: CountWaitingActorInvocations: %w", err)
	}
	return count, nil
}
