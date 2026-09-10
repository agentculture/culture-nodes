package actors

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Lane locking after a credential_spent failure (decision c43's OR rule;
// code-review fix D).
//
// Two paths discover a spent session credential: a SYNCHRONOUS invocation
// whose classified error reaches internal/worker/dispatch.go, and an
// ASYNCHRONOUS one whose bridge answered 202 and later reported a §13.4
// `failed` event carrying class credential_spent through HandleCallback —
// in the API process, which holds no worker. Production codex is the
// second kind (`always_async: true`). Before this file only the first path
// locked the actor_liveness row, so a dead async lane was leased again as
// soon as the collector's fact aged out and failed the same way, forever,
// while its fallback_actor was never taken. Both paths now call
// LockLaneOnCredentialSpent, so they cannot drift apart again.
//
// The helper knows nothing about PostgreSQL: LaneLocker is implemented by
// postgres.CallbackStore, which the worker and the API server both already
// hold, and RunEventAppender is the same store's AppendRunEvent.

// LivenessLockReason is the reason a control-plane lock carries: the
// bridges' own vocabulary word for a spent refresh token, so a row locked
// from either side reads the same.
const LivenessLockReason = "refresh_token_spent"

// TypeLaneLocked records the control plane locking a lane after a
// credential_spent attempt, from either path.
const TypeLaneLocked = "dev.culture.nodes.actor.lane_locked"

// LaneLock is one lock request: which lane, discovered by which attempt.
type LaneLock struct {
	NamespaceID string
	// ActorKey is the lane. When empty it is derived from ActorRef with
	// ActorKeyOf, so a caller holding only the dispatched reference (the
	// durable invocation row) can lock without a second lookup.
	ActorKey string
	ActorRef string
	// Reason defaults to LivenessLockReason.
	Reason string
	// Provenance, recorded on the row (run, attempt) and the event (all).
	RunID     string
	NodeRunID string
	NodeID    string
	AttemptID string
	WorkID    string
	CheckedAt time.Time
}

// LaneLockResult is what the store wrote back.
type LaneLockResult struct {
	Reason string
	Locked bool
}

// LaneLocker writes the control-plane lock for one lane.
type LaneLocker interface {
	LockLane(ctx context.Context, lock LaneLock) (LaneLockResult, error)
}

// RunEventAppender appends one diagnostic event against a run. It is the
// slice of CallbackStore the lock's audit line needs, declared separately
// so the worker can pass its own callback store without implementing the
// whole ingest interface.
type RunEventAppender interface {
	AppendRunEvent(ctx context.Context, namespaceID, runID, eventType string, data map[string]any) error
}

// ActorKeyOf strips the scheme and any @digest from an actor reference:
// "actor://company/analyzer@sha256:..." -> "company/analyzer". A bare key
// is returned unchanged. It is the one parser both lock paths and the
// dispatch gates key on, so a lock and the gate that reads it agree on
// what the lane is called.
func ActorKeyOf(ref string) string {
	trimmed := ref
	if _, rest, ok := strings.Cut(trimmed, "://"); ok {
		trimmed = rest
	}
	if key, _, ok := strings.Cut(trimmed, "@"); ok {
		trimmed = key
	}
	return strings.Trim(trimmed, "/")
}

// LockLaneOnCredentialSpent locks the lane and records TypeLaneLocked. It
// is best-effort by contract: the caller has already committed the failed
// attempt, and a lock that could not be written is returned for reporting
// rather than allowed to disturb that completion — the next dispatch to the
// same lane discovers the same wall, and the bridge's own latched fact (if
// it has landed) routes around it in the meantime. A nil events appender
// locks without the audit line.
func LockLaneOnCredentialSpent(ctx context.Context, locker LaneLocker, events RunEventAppender, lock LaneLock) error {
	if locker == nil {
		return nil
	}
	if lock.ActorKey == "" {
		lock.ActorKey = ActorKeyOf(lock.ActorRef)
	}
	if lock.ActorKey == "" {
		return fmt.Errorf("actors: node %q reported %s but its actor reference %q names no actor key; no lock recorded",
			lock.NodeID, ClassCredentialSpent, lock.ActorRef)
	}
	if lock.Reason == "" {
		lock.Reason = LivenessLockReason
	}
	if lock.CheckedAt.IsZero() {
		lock.CheckedAt = time.Now().UTC()
	}
	result, err := locker.LockLane(ctx, lock)
	if err != nil {
		return fmt.Errorf("actors: lock liveness for actor %s after %s: %w", lock.ActorKey, ClassCredentialSpent, err)
	}
	if events == nil {
		return nil
	}
	data := map[string]any{
		"run_id":      lock.RunID,
		"node_run_id": lock.NodeRunID,
		"node_id":     lock.NodeID,
		"attempt_id":  lock.AttemptID,
		"actor_key":   lock.ActorKey,
		"actor_ref":   lock.ActorRef,
		"reason":      result.Reason,
		"locked":      result.Locked,
		"unlock": "POST /v1alpha1/actors/{id}/resume clears the lock; the lane leases again once the bridge's " +
			"next liveness fact reports session_ok=true",
	}
	if lock.WorkID != "" {
		data["work_id"] = lock.WorkID
	}
	if err := events.AppendRunEvent(ctx, lock.NamespaceID, lock.RunID, TypeLaneLocked, data); err != nil {
		return fmt.Errorf("actors: append %s event for actor %s: %w", TypeLaneLocked, lock.ActorKey, err)
	}
	return nil
}

// TypeLaneLockFailed records that a credential_spent completion committed
// but the lane could not be locked from the callback path — the async
// equivalent of the worker's OnError report, which the API process has
// nowhere else to put.
const TypeLaneLockFailed = "dev.culture.nodes.actor.lane_lock_failed"

// lockLaneIfCredentialSpent is the callback ingest's call into the shared
// helper: after the engine has decided a terminal `failed` event whose
// class is credential_spent — committed OR refused as late, because a spent
// credential reported late is still spent — lock the lane the durable
// invocation row says was dispatched. Nil LaneLocker (every deployment and
// test that predates this) locks nothing.
func (d CallbackDeps) lockLaneIfCredentialSpent(ctx context.Context, inv PendingInvocation, ev CallbackEvent) {
	if d.LaneLocker == nil || ev.Kind != EventFailed {
		return
	}
	if class := failedClassOf(ev); class != ClassCredentialSpent {
		return
	}
	err := LockLaneOnCredentialSpent(ctx, d.LaneLocker, d.Store, LaneLock{
		NamespaceID: inv.NamespaceID,
		ActorRef:    inv.ActorRef,
		RunID:       inv.RunID,
		NodeRunID:   inv.NodeRunID,
		NodeID:      inv.NodeID,
		AttemptID:   inv.AttemptID,
		WorkID:      inv.WorkID,
		CheckedAt:   d.now(),
	})
	if err != nil {
		d.recordDetail(ctx, inv, TypeLaneLockFailed, ev, err.Error(), map[string]any{"actor_ref": inv.ActorRef})
	}
}

// failedClassOf reads the §13.5 class a `failed` event declares, with the
// same default completionFor applies: an unknown or absent class is an
// execution failure.
func failedClassOf(ev CallbackEvent) ErrorClass {
	payload := failedPayloadOf(ev)
	if !payload.Class.Valid() {
		return ClassExecution
	}
	return payload.Class
}
