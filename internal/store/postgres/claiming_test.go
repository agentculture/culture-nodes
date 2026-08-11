package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// mustNodeRun creates the full fixture chain a work_items row requires:
// namespace -> workflow_version -> run -> node_run. work_items.node_run_id
// is NOT NULL, so every claiming test needs one of these, even though
// node_runs itself is out of this task's scope (t9 owns its typed Store
// methods) -- runs/node_runs are inserted with raw SQL via s.Pool(), the
// same escape hatch insertTestLedgerRecord uses in ledger_test.go.
func mustNodeRun(t *testing.T, s *postgres.Store, namespaceID string) string {
	t.Helper()
	ctx := context.Background()

	// WorkflowKey is suffixed with a fresh ULID so repeated calls within
	// the same namespace (several tests call this in a loop) never collide
	// on workflow_versions' (namespace_id, workflow_key, version) uniqueness
	// constraint -- a collision here would abort the fixture chain partway
	// through and leak an orphaned 'ready' work_items row into the shared
	// test database for later tests to (incorrectly) claim, since
	// Store.ClaimWork is a global claim with no namespace filter.
	wv, err := s.CreateWorkflowVersion(ctx, postgres.CreateWorkflowVersionInput{
		NamespaceID:   namespaceID,
		WorkflowKey:   "test-claiming-workflow-" + store.NewULID(),
		Version:       1,
		SourceFormat:  "yaml",
		Source:        "entrypoint: intake\n",
		ContentDigest: "sha256:" + store.NewULID(),
	})
	if err != nil {
		t.Fatalf("mustNodeRun: CreateWorkflowVersion: %v", err)
	}

	runID := store.NewULID()
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO runs (id, namespace_id, workflow_version_id) VALUES ($1, $2, $3)`,
		runID, namespaceID, wv.ID,
	); err != nil {
		t.Fatalf("mustNodeRun: insert run: %v", err)
	}

	return mustNodeRunForRun(t, s, namespaceID, runID)
}

// mustNodeRunForRun inserts one more node_run against an already-created
// run, for tests that need several node runs sharing one run (e.g. the
// duplicate-signal shape, where two work items point at the *same*
// node_run_id).
func mustNodeRunForRun(t *testing.T, s *postgres.Store, namespaceID, runID string) string {
	t.Helper()
	nodeRunID := store.NewULID()
	if _, err := s.Pool().Exec(context.Background(),
		`INSERT INTO node_runs (id, namespace_id, run_id, node_key) VALUES ($1, $2, $3, 'intake')`,
		nodeRunID, namespaceID, runID,
	); err != nil {
		t.Fatalf("mustNodeRunForRun: insert node_run: %v", err)
	}
	return nodeRunID
}

// mustRunAndNodeRun is mustNodeRun for a test that also needs the run id --
// the terminal-run guard tests, which have to move runs.status underneath an
// existing work item.
func mustRunAndNodeRun(t *testing.T, s *postgres.Store, namespaceID string) (runID, nodeRunID string) {
	t.Helper()
	nodeRunID = mustNodeRun(t, s, namespaceID)
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT run_id FROM node_runs WHERE id = $1`, nodeRunID,
	).Scan(&runID); err != nil {
		t.Fatalf("mustRunAndNodeRun: read run id: %v", err)
	}
	return runID, nodeRunID
}

// setRunStatus moves a run into an arbitrary status, standing in for whatever
// really put it there (a cancel from the API, a failed completion from the
// engine). The guard under test reads runs.status and must not care which.
func setRunStatus(t *testing.T, s *postgres.Store, runID, status string) {
	t.Helper()
	if _, err := s.Pool().Exec(context.Background(),
		`UPDATE runs SET status = $2, updated_at = now() WHERE id = $1`, runID, status,
	); err != nil {
		t.Fatalf("setRunStatus(%s): %v", status, err)
	}
}

// workItemState reads one work item's current state and attempt counter.
func workItemState(t *testing.T, s *postgres.Store, workID string) (state string, attempt int32) {
	t.Helper()
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT state, attempt FROM work_items WHERE id = $1`, workID,
	).Scan(&state, &attempt); err != nil {
		t.Fatalf("read work item %s: %v", workID, err)
	}
	return state, attempt
}

// mustEnqueued enqueues one ready work item for nodeRunID. It does not
// return the assigned ID -- EnqueueWork itself returns only an error (see
// claiming.go) -- so tests that need the ID should claim it themselves via
// ClaimWork; this helper is for tests that only need a ready row to exist.
func mustEnqueued(t *testing.T, s *postgres.Store, namespaceID, nodeRunID string) {
	t.Helper()
	if err := s.EnqueueWork(context.Background(), postgres.WorkItem{
		NamespaceID: namespaceID,
		NodeRunID:   nodeRunID,
	}); err != nil {
		t.Fatalf("EnqueueWork: %v", err)
	}
}

// backdateLeaseExpiry forces workID's lease_expires_at into the past so
// ReclaimExpired treats it as expired right now, without the test actually
// sleeping through a real lease duration.
func backdateLeaseExpiry(t *testing.T, s *postgres.Store, workID string) {
	t.Helper()
	if _, err := s.Pool().Exec(context.Background(),
		`UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE id = $1`,
		workID,
	); err != nil {
		t.Fatalf("backdateLeaseExpiry: %v", err)
	}
}

func TestEnqueueWorkAndClaimWorkRoundTrip(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-claim-roundtrip")
	nodeRunID := mustNodeRun(t, s, ns.ID)
	mustEnqueued(t, s, ns.ID, nodeRunID)

	claimed, err := s.ClaimWork(ctx, ns.ID, "worker-1", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimWork returned %d items, want 1", len(claimed))
	}

	got := claimed[0]
	if got.NamespaceID != ns.ID {
		t.Fatalf("NamespaceID = %q, want %q", got.NamespaceID, ns.ID)
	}
	if got.NodeRunID != nodeRunID {
		t.Fatalf("NodeRunID = %q, want %q", got.NodeRunID, nodeRunID)
	}
	if got.State != "leased" {
		t.Fatalf("State = %q, want %q", got.State, "leased")
	}
	if got.LeaseOwner != "worker-1" {
		t.Fatalf("LeaseOwner = %q, want %q", got.LeaseOwner, "worker-1")
	}
	if got.FencingToken != 1 {
		t.Fatalf("FencingToken = %d, want 1 (first claim)", got.FencingToken)
	}
	if got.Attempt != 1 {
		t.Fatalf("Attempt = %d, want 1 (first claim)", got.Attempt)
	}
	if got.LeaseExpiresAt.Before(time.Now()) {
		t.Fatalf("LeaseExpiresAt = %v, want it in the future", got.LeaseExpiresAt)
	}

	// The row is no longer ready, so a second claim must return nothing.
	again, err := s.ClaimWork(ctx, ns.ID, "worker-2", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork (second): %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("ClaimWork (second) returned %d items, want 0 (already leased)", len(again))
	}
}

func TestClaimWorkRespectsAvailableAt(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-claim-available-at")
	nodeRunID := mustNodeRun(t, s, ns.ID)

	if err := s.EnqueueWork(ctx, postgres.WorkItem{
		NamespaceID: ns.ID,
		NodeRunID:   nodeRunID,
		AvailableAt: time.Now().Add(1 * time.Hour),
	}); err != nil {
		t.Fatalf("EnqueueWork: %v", err)
	}

	claimed, err := s.ClaimWork(ctx, ns.ID, "worker-1", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("ClaimWork returned %d items, want 0 (not yet available)", len(claimed))
	}
}

func TestClaimWorkRespectsLimit(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-claim-limit")
	const total = 5
	for i := 0; i < total; i++ {
		mustEnqueued(t, s, ns.ID, mustNodeRun(t, s, ns.ID))
	}

	first, err := s.ClaimWork(ctx, ns.ID, "worker-1", time.Minute, 3)
	if err != nil {
		t.Fatalf("ClaimWork (first): %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("ClaimWork (first) returned %d items, want 3 (the limit)", len(first))
	}

	rest, err := s.ClaimWork(ctx, ns.ID, "worker-2", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork (rest): %v", err)
	}
	if len(rest) != total-3 {
		t.Fatalf("ClaimWork (rest) returned %d items, want %d (the remainder)", len(rest), total-3)
	}
}

// TestClaimWorkIsExclusiveUnderConcurrency proves §12.4's "claiming is
// atomic" and §20.4's "SQS signal is duplicated | PostgreSQL claim permits
// one current owner": two goroutines race ClaimWork against a Postgres
// backend holding exactly one ready row, and exactly one of them must win
// it. This exercises the real FOR UPDATE SKIP LOCKED path against a real
// server -- two ClaimWork statements really do arrive concurrently, not
// serialized by a single-goroutine test harness.
func TestClaimWorkIsExclusiveUnderConcurrency(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-claim-exclusive")
	nodeRunID := mustNodeRun(t, s, ns.ID)
	mustEnqueued(t, s, ns.ID, nodeRunID)

	const racers = 8
	var wg sync.WaitGroup
	results := make([][]postgres.ClaimedWork, racers)
	errs := make([]error, racers)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claimed, err := s.ClaimWork(ctx, ns.ID, "racer", time.Minute, 1)
			results[i] = claimed
			errs[i] = err
		}(i)
	}
	wg.Wait()

	winners := 0
	var wonToken int64
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ClaimWork (racer %d): %v", i, err)
		}
		if len(results[i]) > 0 {
			winners++
			wonToken = results[i][0].FencingToken
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d across %d concurrent ClaimWork calls over 1 ready row, want exactly 1", winners, racers)
	}
	if wonToken != 1 {
		t.Fatalf("winning FencingToken = %d, want 1 (first-ever claim)", wonToken)
	}
}

// TestReclaimExpiredThenClaimGetsHigherFencingToken proves §20.4's "Worker
// dies before dispatch | Lease expires; another worker claims", and that
// the fencing token strictly increases across a reclaim+re-claim cycle even
// though ReclaimExpired itself does not touch fencing_token (see the
// invariant-3 doc comment in claiming.go).
func TestReclaimExpiredThenClaimGetsHigherFencingToken(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-reclaim-fencing")
	nodeRunID := mustNodeRun(t, s, ns.ID)
	mustEnqueued(t, s, ns.ID, nodeRunID)

	first, err := s.ClaimWork(ctx, ns.ID, "worker-dead", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork (first): %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("ClaimWork (first) returned %d items, want 1", len(first))
	}
	workID := first[0].ID
	if first[0].FencingToken != 1 {
		t.Fatalf("first FencingToken = %d, want 1", first[0].FencingToken)
	}

	// Nothing is due for reclaim yet.
	n, err := s.ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpired (too early): %v", err)
	}
	if n != 0 {
		t.Fatalf("ReclaimExpired (too early) reclaimed %d rows, want 0 (lease not yet expired)", n)
	}

	backdateLeaseExpiry(t, s, workID)

	n, err = s.ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReclaimExpired reclaimed %d rows, want 1", n)
	}

	// Reclaiming twice in a row must be idempotent -- nothing left to
	// reclaim the second time.
	n, err = s.ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpired (second call): %v", err)
	}
	if n != 0 {
		t.Fatalf("ReclaimExpired (second call) reclaimed %d rows, want 0", n)
	}

	second, err := s.ClaimWork(ctx, ns.ID, "worker-survivor", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork (second, after reclaim): %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("ClaimWork (second) returned %d items, want 1", len(second))
	}
	if second[0].ID != workID {
		t.Fatalf("ClaimWork (second) claimed a different row (%s), want the reclaimed one (%s)", second[0].ID, workID)
	}
	if second[0].FencingToken <= first[0].FencingToken {
		t.Fatalf("FencingToken after reclaim+re-claim = %d, want strictly greater than the original %d",
			second[0].FencingToken, first[0].FencingToken)
	}
	if second[0].LeaseOwner != "worker-survivor" {
		t.Fatalf("LeaseOwner after re-claim = %q, want %q", second[0].LeaseOwner, "worker-survivor")
	}
}

// TestCompleteWorkStaleTokenRejected proves §20.4's "Actor callback arrives
// after a newer attempt | Record as late; fencing rejects state change":
// once a work item has been reclaimed and re-claimed under a new fencing
// token, a completion carrying the OLD token must be rejected, not silently
// applied over the newer attempt.
func TestCompleteWorkStaleTokenRejected(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-complete-stale")
	nodeRunID := mustNodeRun(t, s, ns.ID)
	mustEnqueued(t, s, ns.ID, nodeRunID)

	stale, err := s.ClaimWork(ctx, ns.ID, "worker-slow", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork (stale claimant): %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("ClaimWork (stale claimant) returned %d items, want 1", len(stale))
	}
	workID := stale[0].ID
	staleToken := stale[0].FencingToken
	staleAttempt := int(stale[0].Attempt)

	backdateLeaseExpiry(t, s, workID)
	if _, err := s.ReclaimExpired(ctx); err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}

	fresh, err := s.ClaimWork(ctx, ns.ID, "worker-fast", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork (fresh claimant): %v", err)
	}
	if len(fresh) != 1 {
		t.Fatalf("ClaimWork (fresh claimant) returned %d items, want 1", len(fresh))
	}

	// The slow worker, unaware it was reclaimed, now tries to complete
	// with its stale owner name and token.
	err = s.CompleteWork(ctx, workID, "worker-slow", staleToken, staleAttempt)
	if !errors.Is(err, postgres.ErrStaleClaim) {
		t.Fatalf("CompleteWork (stale token) error = %v, want ErrStaleClaim", err)
	}

	// The fresh worker's own completion, using the current token/attempt,
	// must still succeed -- the stale rejection above must not have
	// disturbed the current lease.
	if err := s.CompleteWork(ctx, workID, "worker-fast", fresh[0].FencingToken, int(fresh[0].Attempt)); err != nil {
		t.Fatalf("CompleteWork (fresh claimant): %v", err)
	}
}

// TestCompleteWorkSucceedsAndTransitionsState is the CompleteWork positive
// path: state becomes 'completed' and the lease is cleared.
func TestCompleteWorkSucceedsAndTransitionsState(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-complete-success")
	nodeRunID := mustNodeRun(t, s, ns.ID)
	mustEnqueued(t, s, ns.ID, nodeRunID)

	claimed, err := s.ClaimWork(ctx, ns.ID, "worker-1", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}
	item := claimed[0]

	if err := s.CompleteWork(ctx, item.ID, "worker-1", item.FencingToken, int(item.Attempt)); err != nil {
		t.Fatalf("CompleteWork: %v", err)
	}

	var state string
	var leaseOwner *string
	if err := s.Pool().QueryRow(ctx,
		`SELECT state, lease_owner FROM work_items WHERE id = $1`, item.ID,
	).Scan(&state, &leaseOwner); err != nil {
		t.Fatalf("query work_items: %v", err)
	}
	if state != "completed" {
		t.Fatalf("state = %q, want %q", state, "completed")
	}
	if leaseOwner != nil {
		t.Fatalf("lease_owner = %v, want nil after completion", *leaseOwner)
	}

	// A second completion attempt (e.g. a duplicated callback) must not
	// succeed a second time -- the item is no longer 'leased'.
	err = s.CompleteWork(ctx, item.ID, "worker-1", item.FencingToken, int(item.Attempt))
	if !errors.Is(err, postgres.ErrStaleClaim) {
		t.Fatalf("CompleteWork (duplicate) error = %v, want ErrStaleClaim", err)
	}
}

// TestExtendLeaseWrongOwnerRejected proves the same §20.4 "fencing rejects
// state change" row applies to lease renewal, not just completion: a
// heartbeat/extend call from anyone other than the current lease owner
// must fail with ErrStaleClaim, never quietly renew someone else's lease.
func TestExtendLeaseWrongOwnerRejected(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-extend-wrong-owner")
	nodeRunID := mustNodeRun(t, s, ns.ID)
	mustEnqueued(t, s, ns.ID, nodeRunID)

	claimed, err := s.ClaimWork(ctx, ns.ID, "worker-owner", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}
	item := claimed[0]

	err = s.ExtendLease(ctx, item.ID, "worker-impostor", item.FencingToken, time.Minute)
	if !errors.Is(err, postgres.ErrStaleClaim) {
		t.Fatalf("ExtendLease (wrong owner) error = %v, want ErrStaleClaim", err)
	}

	// The rightful owner extending with the correct token must succeed and
	// push lease_expires_at further into the future.
	before := item.LeaseExpiresAt
	if err := s.ExtendLease(ctx, item.ID, "worker-owner", item.FencingToken, time.Hour); err != nil {
		t.Fatalf("ExtendLease (rightful owner): %v", err)
	}

	var after time.Time
	if err := s.Pool().QueryRow(ctx,
		`SELECT lease_expires_at FROM work_items WHERE id = $1`, item.ID,
	).Scan(&after); err != nil {
		t.Fatalf("query work_items: %v", err)
	}
	if !after.After(before) {
		t.Fatalf("lease_expires_at after ExtendLease = %v, want later than %v", after, before)
	}
}

// TestExtendLeaseStaleTokenRejected mirrors TestCompleteWorkStaleTokenRejected
// for ExtendLease: an old fencing token must not be able to renew a lease a
// newer claim now holds.
func TestExtendLeaseStaleTokenRejected(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-extend-stale-token")
	nodeRunID := mustNodeRun(t, s, ns.ID)
	mustEnqueued(t, s, ns.ID, nodeRunID)

	first, err := s.ClaimWork(ctx, ns.ID, "worker-slow", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork (first): %v", err)
	}
	workID := first[0].ID
	staleToken := first[0].FencingToken

	backdateLeaseExpiry(t, s, workID)
	if _, err := s.ReclaimExpired(ctx); err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if _, err := s.ClaimWork(ctx, ns.ID, "worker-fast", time.Minute, 10); err != nil {
		t.Fatalf("ClaimWork (second): %v", err)
	}

	err = s.ExtendLease(ctx, workID, "worker-slow", staleToken, time.Minute)
	if !errors.Is(err, postgres.ErrStaleClaim) {
		t.Fatalf("ExtendLease (stale token) error = %v, want ErrStaleClaim", err)
	}
}

func TestReclaimExpiredIgnoresActiveLeases(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-reclaim-active")
	nodeRunID := mustNodeRun(t, s, ns.ID)
	mustEnqueued(t, s, ns.ID, nodeRunID)

	if _, err := s.ClaimWork(ctx, ns.ID, "worker-1", time.Hour, 10); err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}

	n, err := s.ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if n != 0 {
		t.Fatalf("ReclaimExpired reclaimed %d rows, want 0 (lease is not expired)", n)
	}
}

func TestEnqueueWorkRequiresNamespaceAndNodeRun(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-enqueue-validation")
	nodeRunID := mustNodeRun(t, s, ns.ID)

	if err := s.EnqueueWork(ctx, postgres.WorkItem{NodeRunID: nodeRunID}); err == nil {
		t.Fatal("EnqueueWork with empty NamespaceID succeeded, want an error")
	}
	if err := s.EnqueueWork(ctx, postgres.WorkItem{NamespaceID: ns.ID}); err == nil {
		t.Fatal("EnqueueWork with empty NodeRunID succeeded, want an error")
	}
}

// TestClaimWorkRefusesItemsOfTerminalRuns proves the terminal-run guard on
// the claim path (spec claim c6, honesty condition h6): a work item whose run
// has already reached a state it never leaves is not claimable, no matter how
// it got there. Both live 2026-08-11 incident shapes are covered -- a run
// CANCELLED out from under an in-flight item (issue #19) and a run that
// reached FAILED with an item still around (the second shape) -- plus
// `completed` for symmetry, because "terminal" is a property of the state,
// not of the story that produced it.
//
// Each status subtest ends by putting the run back to 'running' and claiming
// successfully, so a passing refusal can never be an accident of a fixture
// that was unclaimable for some unrelated reason.
func TestClaimWorkRefusesItemsOfTerminalRuns(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	for _, status := range []string{"cancelled", "failed", "completed"} {
		t.Run(status, func(t *testing.T) {
			ns := mustNamespace(t, s, "test-claim-terminal-run-"+status)
			runID, nodeRunID := mustRunAndNodeRun(t, s, ns.ID)
			mustEnqueued(t, s, ns.ID, nodeRunID)

			setRunStatus(t, s, runID, status)

			claimed, err := s.ClaimWork(ctx, ns.ID, "worker-1", time.Minute, 10)
			if err != nil {
				t.Fatalf("ClaimWork: %v", err)
			}
			if len(claimed) != 0 {
				t.Fatalf("ClaimWork returned %d items for a %s run, want 0: a terminal run must never dispatch again",
					len(claimed), status)
			}

			// Control: the same row is claimable while the run is running, so
			// the refusal above is the run status and nothing else.
			setRunStatus(t, s, runID, "running")
			claimed, err = s.ClaimWork(ctx, ns.ID, "worker-1", time.Minute, 10)
			if err != nil {
				t.Fatalf("ClaimWork (control, running run): %v", err)
			}
			if len(claimed) != 1 {
				t.Fatalf("ClaimWork (control, running run) returned %d items, want 1", len(claimed))
			}
		})
	}
}

// TestReclaimExpiredRefusesItemsOfTerminalRuns proves the same guard on the
// other half of the loop motor (spec claim c16): the 2026-08-11 zombie
// cycled because an expired lease on a cancelled run's item was swept back to
// 'ready' every minute. ReclaimExpired must leave such a row alone.
//
// The assertion is on the row's own state rather than on ReclaimExpired's
// returned count: the sweep is namespace-wide, so another test's expired row
// could legitimately be counted in the same call.
func TestReclaimExpiredRefusesItemsOfTerminalRuns(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	for _, status := range []string{"cancelled", "failed"} {
		t.Run(status, func(t *testing.T) {
			ns := mustNamespace(t, s, "test-reclaim-terminal-run-"+status)
			runID, nodeRunID := mustRunAndNodeRun(t, s, ns.ID)
			mustEnqueued(t, s, ns.ID, nodeRunID)

			claimed, err := s.ClaimWork(ctx, ns.ID, "worker-doomed", time.Minute, 10)
			if err != nil {
				t.Fatalf("ClaimWork: %v", err)
			}
			if len(claimed) != 1 {
				t.Fatalf("ClaimWork returned %d items, want 1", len(claimed))
			}
			workID := claimed[0].ID

			backdateLeaseExpiry(t, s, workID)
			setRunStatus(t, s, runID, status)

			if _, err := s.ReclaimExpired(ctx); err != nil {
				t.Fatalf("ReclaimExpired: %v", err)
			}
			if state, _ := workItemState(t, s, workID); state != "leased" {
				t.Fatalf("work item state after ReclaimExpired on a %s run = %q, want it left %q "+
					"(reclaiming it would put a terminal run's work back in the dispatch loop)", status, state, "leased")
			}

			// Control: the identical expired row IS reclaimed once its run is
			// no longer terminal.
			setRunStatus(t, s, runID, "running")
			if _, err := s.ReclaimExpired(ctx); err != nil {
				t.Fatalf("ReclaimExpired (control, running run): %v", err)
			}
			if state, _ := workItemState(t, s, workID); state != "ready" {
				t.Fatalf("work item state after ReclaimExpired on a running run = %q, want %q", state, "ready")
			}
		})
	}
}

// TestWaitingWorkAccruesNoAttempts proves spec assumption c20 (honesty
// condition h20), the assumption the dispatch budget rests on: a healthy
// long-running actor's parked item must not burn budget merely by waiting.
// work_items.attempt is incremented by exactly one statement -- claimWorkSQL
// -- so a 'waiting' row's counter survives repeated ReclaimExpired sweeps and
// an expired lease timestamp, and only a genuine re-claim moves it.
func TestWaitingWorkAccruesNoAttempts(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-waiting-accrues-nothing")
	nodeRunID := mustNodeRun(t, s, ns.ID)
	mustEnqueued(t, s, ns.ID, nodeRunID)

	claimed, err := s.ClaimWork(ctx, ns.ID, "worker-1", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimWork returned %d items, want 1", len(claimed))
	}
	workID := claimed[0].ID
	if claimed[0].Attempt != 1 {
		t.Fatalf("Attempt after first claim = %d, want 1", claimed[0].Attempt)
	}

	// Park it exactly as async.go's parkWorkSQL does when an actor answers
	// 202, then leave a stale (already expired) lease timestamp behind it --
	// the harshest version of "lease-duration passage" a waiting row could
	// ever see.
	if _, err := s.Pool().Exec(ctx, `
		UPDATE work_items
		SET state            = 'waiting',
		    lease_owner      = NULL,
		    lease_expires_at = now() - interval '1 hour',
		    state_version    = state_version + 1,
		    updated_at       = now()
		WHERE id = $1
	`, workID); err != nil {
		t.Fatalf("park work item: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := s.ReclaimExpired(ctx); err != nil {
			t.Fatalf("ReclaimExpired (sweep %d): %v", i+1, err)
		}
		state, attempt := workItemState(t, s, workID)
		if state != "waiting" {
			t.Fatalf("state after sweep %d = %q, want %q (a waiting item is invisible to the sweep)", i+1, state, "waiting")
		}
		if attempt != 1 {
			t.Fatalf("attempt after sweep %d = %d, want 1: waiting must accrue nothing", i+1, attempt)
		}
	}

	// A claim -- and only a claim -- moves the counter.
	if _, err := s.Pool().Exec(ctx,
		`UPDATE work_items SET state = 'ready', lease_expires_at = NULL, available_at = now() WHERE id = $1`, workID,
	); err != nil {
		t.Fatalf("resume work item to ready: %v", err)
	}
	again, err := s.ClaimWork(ctx, ns.ID, "worker-2", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork (after waiting): %v", err)
	}
	if len(again) != 1 || again[0].ID != workID {
		t.Fatalf("ClaimWork (after waiting) returned %v, want the resumed item %s", again, workID)
	}
	if again[0].Attempt != 2 {
		t.Fatalf("Attempt after re-claim = %d, want 2 (exactly one increment, from the claim itself)", again[0].Attempt)
	}
}

// TestTerminalRunStatusesMatchTheEngineVocabulary keeps the SQL guard's
// notion of "terminal" tied to the engine's. The guard is a data-plane
// filter, so a run state added to engine.RunState without being classified
// here would silently keep dispatching.
func TestTerminalRunStatusesMatchTheEngineVocabulary(t *testing.T) {
	guarded := map[string]bool{}
	for _, status := range postgres.TerminalRunStatuses() {
		guarded[status] = true
	}

	// Every state the engine declares, checked both ways.
	for _, state := range []engine.RunState{
		engine.RunCreated, engine.RunRunning, engine.RunWaiting,
		engine.RunCompleted, engine.RunFailed, engine.RunCancelled,
	} {
		if got, want := guarded[string(state)], state.Terminal(); got != want {
			t.Errorf("run state %q: guarded as terminal = %v, engine.RunState.Terminal() = %v", state, got, want)
		}
	}
	if len(guarded) != len(postgres.TerminalRunStatuses()) {
		t.Errorf("TerminalRunStatuses() contains duplicates: %v", postgres.TerminalRunStatuses())
	}
}

// TestPendingInvocationForWorkReportsAbsenceRatherThanFailing covers the
// lookup's two non-happy branches. Its positive path is exercised end to end
// by internal/worker's budget-exhaustion tests, which cancel a real parked
// invocation through it; what matters here is that a work item with nothing
// in flight is an ordinary "no", not an error a best-effort caller has to
// distinguish from a real database failure.
func TestPendingInvocationForWorkReportsAbsenceRatherThanFailing(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-pending-invocation-absent")
	nodeRunID := mustNodeRun(t, s, ns.ID)
	mustEnqueued(t, s, ns.ID, nodeRunID)

	claimed, err := s.ClaimWork(ctx, ns.ID, "worker-1", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimWork returned %d items, want 1", len(claimed))
	}

	inv, ok, err := s.PendingInvocationForWork(ctx, claimed[0].ID)
	if err != nil {
		t.Fatalf("PendingInvocationForWork: %v", err)
	}
	if ok {
		t.Fatalf("PendingInvocationForWork found %+v, want none: this item never parked on an actor", inv)
	}

	if _, _, err := s.PendingInvocationForWork(ctx, ""); err == nil {
		t.Fatal("PendingInvocationForWork with an empty work id succeeded, want an error")
	}
}

func TestClaimWorkRejectsInvalidArguments(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-claim-invalid-args")

	if _, err := s.ClaimWork(ctx, "", "worker-1", time.Minute, 10); err == nil {
		t.Fatal("ClaimWork with empty namespaceID succeeded, want an error")
	}
	if _, err := s.ClaimWork(ctx, ns.ID, "", time.Minute, 10); err == nil {
		t.Fatal("ClaimWork with empty workerID succeeded, want an error")
	}
	if _, err := s.ClaimWork(ctx, ns.ID, "worker-1", 0, 10); err == nil {
		t.Fatal("ClaimWork with zero leaseDuration succeeded, want an error")
	}
	if _, err := s.ClaimWork(ctx, ns.ID, "worker-1", time.Minute, 0); err == nil {
		t.Fatal("ClaimWork with zero limit succeeded, want an error")
	}
}
