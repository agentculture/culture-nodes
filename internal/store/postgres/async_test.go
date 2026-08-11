package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The store half of issue #16's terminal-commit incident: the two writes the
// callback ingest needs to undo a delivery it could not finish, proved against
// the real tables and the real claim/reclaim path.
//
// Every fixture here is namespace-scoped (mustNamespace mints a ULID-suffixed
// slug) because the sweep these tests exercise -- ReclaimExpired -- is
// deliberately global, so an assertion written against a shared row would be
// answering another test's question (issue #9).

// parkedItem is one work item taken all the way through the real dispatch
// path: enqueued, claimed (so the fencing tuple is the one claiming.go
// assigns), then parked by StartAsyncWait exactly as a worker parks an
// asynchronous invocation.
type parkedItem struct {
	ns        postgres.Namespace
	runID     string
	nodeRunID string
	claimed   postgres.ClaimedWork
	attemptID string
	callbacks *postgres.CallbackStore
	inv       actors.PendingInvocation
}

func newParkedItem(t *testing.T, s *postgres.Store, slug string) *parkedItem {
	t.Helper()
	ctx := context.Background()

	ns := mustNamespace(t, s, slug)
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
		ActorRef:     "actor://company/long-runner@sha256:aaaaaa",
		InvocationID: "external_" + attemptID,
	}); err != nil {
		t.Fatalf("StartAsyncWait: %v", err)
	}

	callbacks, err := postgres.NewCallbackStore(s, ns.ID)
	if err != nil {
		t.Fatalf("NewCallbackStore: %v", err)
	}
	inv, err := callbacks.Invocation(ctx, attemptID)
	if err != nil {
		t.Fatalf("Invocation: %v", err)
	}
	return &parkedItem{
		ns: ns, runID: runID, nodeRunID: nodeRunID, claimed: claimed[0],
		attemptID: attemptID, callbacks: callbacks, inv: inv,
	}
}

func (p *parkedItem) workState(t *testing.T, s *postgres.Store) (state string, owner *string) {
	t.Helper()
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT state, lease_owner FROM work_items WHERE id = $1`, p.claimed.ID).Scan(&state, &owner); err != nil {
		t.Fatalf("read work item: %v", err)
	}
	return state, owner
}

func (p *parkedItem) nodeRunStatus(t *testing.T, s *postgres.Store) string {
	t.Helper()
	var status string
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT status FROM node_runs WHERE id = $1`, p.nodeRunID).Scan(&status); err != nil {
		t.Fatalf("read node run: %v", err)
	}
	return status
}

// TestResumedWorkThatNeverCompletesFeedsTheReclaimLoop pins the mechanism
// behind the live billable loop (issue #16, c16): ResumeWaitingWork re-leases
// the item BEFORE the engine's transaction runs, so a commit that fails
// leaves it leased with nobody working it, and the very next lease sweep
// hands it back for re-dispatch. This is the state sequence the ingest's
// compensation exists to break; asserting it here keeps the motor itself
// pinned, so a future change that reintroduces it fails this test rather than
// a production quota.
func TestResumedWorkThatNeverCompletesFeedsTheReclaimLoop(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	p := newParkedItem(t, s, "test-resume-loop")

	if state, owner := p.workState(t, s); state != postgres.WaitingWorkState || owner != nil {
		t.Fatalf("parked work item = %q owned by %v, want %q with no owner", state, owner, postgres.WaitingWorkState)
	}

	if err := p.callbacks.ResumeWaitingWork(ctx, p.inv, time.Minute); err != nil {
		t.Fatalf("ResumeWaitingWork: %v", err)
	}
	state, owner := p.workState(t, s)
	if state != "leased" || owner == nil || *owner != p.inv.WorkerID {
		t.Fatalf("resumed work item = %q owned by %v, want leased by %s", state, owner, p.inv.WorkerID)
	}
	if got := p.nodeRunStatus(t, s); got != "running" {
		t.Errorf("node run status = %q, want running while the completion commits", got)
	}

	// ... and now the commit fails. Nothing parks the item, its lease expires,
	// and the sweep makes it claimable again: one more dispatch, one more
	// billable session.
	backdateLeaseExpiry(t, s, p.claimed.ID)
	if _, err := s.ReclaimExpired(ctx); err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if state, _ := p.workState(t, s); state != "ready" {
		t.Fatalf("after the lease expired the work item is %q, want ready: this is the re-dispatch loop", state)
	}
	reclaimed, err := s.ClaimWork(ctx, p.ns.ID, "next-worker", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Attempt <= p.claimed.Attempt {
		t.Fatalf("re-claim = %+v, want one item at an attempt above %d", reclaimed, p.claimed.Attempt)
	}
}

// TestReparkResumedWorkReturnsTheItemToTheParkedState is the compensation:
// after it, the row is byte-for-byte in the state StartAsyncWait left (bar
// state_version), the sweep ignores it, and the redelivery's own
// ResumeWaitingWork matches again -- which is the whole point, since a
// redelivery that cannot re-resume can never commit.
func TestReparkResumedWorkReturnsTheItemToTheParkedState(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	p := newParkedItem(t, s, "test-repark")

	if err := p.callbacks.ResumeWaitingWork(ctx, p.inv, time.Minute); err != nil {
		t.Fatalf("ResumeWaitingWork: %v", err)
	}
	if err := p.callbacks.ReparkResumedWork(ctx, p.inv); err != nil {
		t.Fatalf("ReparkResumedWork: %v", err)
	}

	state, owner := p.workState(t, s)
	if state != postgres.WaitingWorkState || owner != nil {
		t.Fatalf("reparked work item = %q owned by %v, want %q with no owner", state, owner, postgres.WaitingWorkState)
	}
	if got := p.nodeRunStatus(t, s); got != "waiting_external" {
		t.Errorf("node run status = %q, want waiting_external again", got)
	}

	// The fencing tuple is untouched: reparking is an undo, not a new claim,
	// so the callback that arrives next is still the one this dispatch minted
	// a token for.
	var fencingToken int64
	var attempt int32
	if err := s.Pool().QueryRow(ctx,
		`SELECT fencing_token, attempt FROM work_items WHERE id = $1`, p.claimed.ID).Scan(&fencingToken, &attempt); err != nil {
		t.Fatalf("read fencing tuple: %v", err)
	}
	if fencingToken != p.claimed.FencingToken || attempt != p.claimed.Attempt {
		t.Errorf("fencing tuple = (%d, %d), want the dispatch's (%d, %d)",
			fencingToken, attempt, p.claimed.FencingToken, p.claimed.Attempt)
	}

	// No lease to expire, so no sweep can hand it out -- the loop is broken.
	backdateLeaseExpiry(t, s, p.claimed.ID)
	if _, err := s.ReclaimExpired(ctx); err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if state, _ := p.workState(t, s); state != postgres.WaitingWorkState {
		t.Fatalf("after a lease sweep the work item is %q, want still %q", state, postgres.WaitingWorkState)
	}
	if claimed, err := s.ClaimWork(ctx, p.ns.ID, "next-worker", time.Minute, 10); err != nil {
		t.Fatalf("ClaimWork: %v", err)
	} else if len(claimed) != 0 {
		t.Fatalf("a reparked item was claimable: %+v", claimed)
	}

	// And the redelivery can pick up exactly where the failed one left off.
	if err := p.callbacks.ResumeWaitingWork(ctx, p.inv, time.Minute); err != nil {
		t.Fatalf("ResumeWaitingWork after a repark: %v", err)
	}
}

// TestReparkResumedWorkLeavesAnItemItNoLongerOwnsAlone: an undo that could
// move a row it no longer owns would be a second way to lose a completion.
func TestReparkResumedWorkLeavesAnItemItNoLongerOwnsAlone(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	p := newParkedItem(t, s, "test-repark-foreign")

	if err := p.callbacks.ResumeWaitingWork(ctx, p.inv, time.Minute); err != nil {
		t.Fatalf("ResumeWaitingWork: %v", err)
	}
	// The engine committed after all: the item is completed, and the undo
	// arrives late.
	if _, err := s.Pool().Exec(ctx,
		`UPDATE work_items SET state = 'completed', lease_owner = NULL, lease_expires_at = NULL WHERE id = $1`,
		p.claimed.ID); err != nil {
		t.Fatalf("complete work item: %v", err)
	}
	if err := p.callbacks.ReparkResumedWork(ctx, p.inv); err != nil {
		t.Fatalf("ReparkResumedWork on a completed item: %v, want a silent no-op", err)
	}
	if state, _ := p.workState(t, s); state != "completed" {
		t.Fatalf("work item = %q after a late repark, want completed", state)
	}
}

// TestRollbackCallbackSequenceRestoresOnlyItsOwnAdvance: the mark must be
// returnable for a delivery that failed, without becoming a way to replay an
// accepted stream backwards (§13.4's monotonic rule for everything else).
func TestRollbackCallbackSequenceRestoresOnlyItsOwnAdvance(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	p := newParkedItem(t, s, "test-sequence-rollback")

	advance := func(sequence int64) bool {
		t.Helper()
		advanced, err := p.callbacks.AdvanceCallbackSequence(ctx, p.attemptID, sequence)
		if err != nil {
			t.Fatalf("AdvanceCallbackSequence(%d): %v", sequence, err)
		}
		return advanced
	}
	rollback := func(sequence, previous int64) {
		t.Helper()
		if err := p.callbacks.RollbackCallbackSequence(ctx, p.attemptID, sequence, previous); err != nil {
			t.Fatalf("RollbackCallbackSequence(%d -> %d): %v", sequence, previous, err)
		}
	}
	mark := func() int64 {
		t.Helper()
		inv, err := p.callbacks.Invocation(ctx, p.attemptID)
		if err != nil {
			t.Fatalf("Invocation: %v", err)
		}
		return inv.LastSequence
	}

	if !advance(4) || !advance(5) {
		t.Fatal("a rising sequence did not advance the mark")
	}

	// The delivery of 5 failed: the mark goes back, and 5 is deliverable
	// again -- the redelivery §13.4 mandates.
	rollback(5, 4)
	if got := mark(); got != 4 {
		t.Fatalf("mark after a rollback = %d, want 4", got)
	}
	if !advance(5) {
		t.Fatal("the redelivery of the failed event was still refused after the mark was returned")
	}

	// A genuinely reordered event is still refused: the rollback returned the
	// mark, it did not open the ratchet.
	if advance(3) {
		t.Error("sequence 3 advanced a mark already at 5: the monotonic rule was weakened")
	}
	if advance(5) {
		t.Error("sequence 5 advanced a mark already at 5")
	}

	// And a rollback whose advance is no longer the newest one does nothing:
	// a later event's mark outranks an earlier event's undo.
	if !advance(9) {
		t.Fatal("sequence 9 did not advance")
	}
	rollback(5, 4)
	if got := mark(); got != 9 {
		t.Fatalf("mark = %d after a stale rollback, want the newer 9", got)
	}
}
