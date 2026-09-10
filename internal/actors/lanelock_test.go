package actors_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The asynchronous half of decision c43's OR rule (code-review fix D). A
// production codex lane is LOCK mode AND async (`always_async: true`): the
// bridge answers 202 and reports class credential_spent through THIS
// handler, in the API process, which holds no worker. Before this fix only
// the synchronous path (internal/worker/dispatch.go) locked the row, so a
// dead async lane was leased again as soon as the collector's stale fact
// aged out. These pin that the callback ingest locks the lane exactly as
// the sync path does, through the shared helper.

const parkedActorKey = "company/long-runner"

func failedEvent(id string, class actors.ErrorClass) actors.CallbackEvent {
	payload, _ := json.Marshal(actors.FailedPayload{
		Class:   class,
		Message: "Your access token could not be refreshed because your refresh token was revoked",
	})
	return actors.CallbackEvent{EventID: id, Sequence: 1, Kind: actors.EventFailed, Payload: payload}
}

func TestCallbackFailedCredentialSpentLocksTheLane(t *testing.T) {
	f := newAsyncFixture(t)
	f.deps.LaneLocker = f.callbacks

	result := f.handle(failedEvent("ev-spent", actors.ClassCredentialSpent))
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed: the lock is a layer above the attempt, not a replacement for it",
			result.Disposition, result.Diagnostic)
	}

	row, ok, err := f.store.ActorLiveness(f.ctx, f.ns.ID, parkedActorKey)
	if err != nil || !ok {
		t.Fatalf("ActorLiveness = (%v, %v), want the locked row for %s", ok, err, parkedActorKey)
	}
	if !row.Locked || row.SessionOK == nil || *row.SessionOK {
		t.Fatalf("row = %+v, want locked session_ok=false", row)
	}
	if row.Reason != actors.LivenessLockReason || row.Source != storepg.LivenessSourceWorker {
		t.Errorf("reason/source = %s/%s, want %s/%s: the async lock reads the same as the sync one",
			row.Reason, row.Source, actors.LivenessLockReason, storepg.LivenessSourceWorker)
	}
	if row.LockedByRunID != f.run.ID || row.LockedByAttemptID != f.attemptID {
		t.Errorf("lock provenance = run %q attempt %q, want run %q attempt %q",
			row.LockedByRunID, row.LockedByAttemptID, f.run.ID, f.attemptID)
	}
	if row.Live(time.Now().UTC()) {
		t.Error("a freshly locked row reads live")
	}
	if !f.hasEvent(actors.TypeLaneLocked) {
		t.Errorf("run events %v carry no %s", f.eventTypes(), actors.TypeLaneLocked)
	}
}

func TestCallbackFailedOtherClassesDoNotLockTheLane(t *testing.T) {
	f := newAsyncFixture(t)
	f.deps.LaneLocker = f.callbacks

	result := f.handle(failedEvent("ev-crash", actors.ClassExecution))
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}
	if _, ok, err := f.store.ActorLiveness(f.ctx, f.ns.ID, parkedActorKey); err != nil || ok {
		t.Fatalf("ActorLiveness = (found=%v, %v), want no row: an execution failure says nothing about the credential", ok, err)
	}
	if f.hasEvent(actors.TypeLaneLocked) {
		t.Error("an execution failure recorded a lane lock")
	}
}

// A deployment wired without a locker (every pre-existing caller) behaves
// exactly as before: the attempt commits, nothing is locked.
func TestCallbackWithoutALaneLockerCommitsAndLocksNothing(t *testing.T) {
	f := newAsyncFixture(t)

	result := f.handle(failedEvent("ev-spent-unwired", actors.ClassCredentialSpent))
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}
	if _, ok, err := f.store.ActorLiveness(f.ctx, f.ns.ID, parkedActorKey); err != nil || ok {
		t.Fatalf("ActorLiveness = (found=%v, %v), want no row without a locker", ok, err)
	}
}

// The shape the fix closes (Qodo High, PR #326). A production async lane is
// the one whose deadline is most likely to fire first: the scheduler
// reclaims the work item and records a `timed_out` attempt, and only then
// does the bridge's `failed`/credential_spent event arrive — to a stale
// claim. §13.4 still refuses that completion, and must. But the refusal is
// about the ATTEMPT, and a spent refresh token is a fact about the LANE.
// Locking only after a successful re-lease meant this path returned through
// late() without ever touching actor_liveness, so the dead lane was leased
// again as soon as the collector's fact aged out and its fallback_actor was
// never taken — exactly the failure lanelock.go exists to end, surviving in
// the one ordering that matters most.
func TestLateCallbackFailedCredentialSpentStillLocksTheLane(t *testing.T) {
	f := newAsyncFixtureForActor(t)
	f.deps.LaneLocker = f.callbacks

	f.deadlineExpiry()

	result := f.handle(failedEvent("ev-late-spent", actors.ClassCredentialSpent))
	if result.Disposition != actors.DispositionLate {
		t.Fatalf("disposition = %s (%s), want late: the work item was reclaimed, so §13.4 refuses the completion",
			result.Disposition, result.Diagnostic)
	}
	if result.Completion != nil {
		t.Error("a late completion committed a result; §13.4 forbids it")
	}

	row, ok, err := f.store.ActorLiveness(f.ctx, f.ns.ID, parkedActorKey)
	if err != nil || !ok {
		t.Fatalf("ActorLiveness = (found=%v, %v), want the locked row for %s: a credential reported late is still spent",
			ok, err, parkedActorKey)
	}
	if !row.Locked || row.SessionOK == nil || *row.SessionOK {
		t.Fatalf("row = %+v, want locked session_ok=false", row)
	}
	if row.Reason != actors.LivenessLockReason || row.Source != storepg.LivenessSourceWorker {
		t.Errorf("reason/source = %s/%s, want %s/%s: a late lock reads the same as a committed one",
			row.Reason, row.Source, actors.LivenessLockReason, storepg.LivenessSourceWorker)
	}
	if row.LockedByRunID != f.run.ID || row.LockedByAttemptID != f.attemptID {
		t.Errorf("lock provenance = run %q attempt %q, want run %q attempt %q",
			row.LockedByRunID, row.LockedByAttemptID, f.run.ID, f.attemptID)
	}
	if row.Live(time.Now().UTC()) {
		t.Error("a freshly locked row reads live")
	}
	if !f.hasEvent(actors.TypeLaneLocked) {
		t.Errorf("run events %v carry no %s", f.eventTypes(), actors.TypeLaneLocked)
	}
	if !f.hasEvent(actors.TypeCallbackLate) {
		t.Errorf("run events %v carry no %s; the lock must not replace the §13.4 refusal record",
			f.eventTypes(), actors.TypeCallbackLate)
	}
}
