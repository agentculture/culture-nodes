package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The tests in this file exercise the ledger runtime against a real
// PostgreSQL instance. internal/ledger's own suite proves the rules; these
// prove they still hold when the transaction, the immutability trigger, and
// the foreign keys are real -- in particular that a refused review really
// does roll back, rather than merely being un-applied by an in-memory fake.

// ledgerFixture is one namespace, one run, and the registered actors a
// ledger record may name as its origin.
type ledgerFixture struct {
	store       *postgres.Store
	ledger      *ledger.Ledger
	namespaceID string
	runID       string
	agentActor  string
	humanActor  string
	runnerActor string
}

func newLedgerFixture(t *testing.T, slugPrefix string) *ledgerFixture {
	t.Helper()
	ctx := context.Background()
	s := requireStore(t)

	ns := mustNamespace(t, s, slugPrefix)

	version, err := s.CreateWorkflowVersion(ctx, postgres.CreateWorkflowVersionInput{
		NamespaceID:   ns.ID,
		WorkflowKey:   "deliver-change",
		Version:       1,
		SourceFormat:  "json",
		Source:        `{"kind":"Workflow"}`,
		NormalizedIR:  json.RawMessage(`{"kind":"Workflow"}`),
		ContentDigest: "sha256:" + strings.ToLower(store.NewULID()) + "0000000000000000000000000000000000000000",
	})
	if err != nil {
		t.Fatalf("CreateWorkflowVersion: %v", err)
	}

	f := &ledgerFixture{
		store:       s,
		namespaceID: ns.ID,
		runID:       "run_" + store.NewULID(),
		agentActor:  "actor_agent_" + store.NewULID(),
		humanActor:  "actor_human_" + store.NewULID(),
		runnerActor: "runner_" + store.NewULID(),
	}

	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO runs (id, namespace_id, workflow_version_id) VALUES ($1, $2, $3)`,
		f.runID, ns.ID, version.ID); err != nil {
		t.Fatalf("insert run fixture: %v", err)
	}

	for _, actor := range []struct{ id, kind string }{
		{f.agentActor, "agent"},
		{f.humanActor, "human"},
		{f.runnerActor, "runner"},
	} {
		if _, err := s.Pool().Exec(ctx,
			`INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
			 VALUES ($1, $2, $3, 1, $4, 'http')`,
			actor.id, ns.ID, actor.id, actor.kind); err != nil {
			t.Fatalf("insert actor fixture %s: %v", actor.id, err)
		}
	}

	l, err := postgres.NewLedger(s, ns.ID)
	if err != nil {
		t.Fatalf("postgres.NewLedger: %v", err)
	}
	f.ledger = l
	return f
}

func (f *ledgerFixture) claim(t *testing.T, statement string) ledger.Record {
	t.Helper()
	return ledger.Record{
		RecordType: ledger.RecordClaim,
		RunID:      f.runID,
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: f.agentActor, ActorRevision: "planner-2026-07-11"},
		Authority:  ledger.AuthorityProposed,
		Data:       json.RawMessage(`{"statement":` + mustQuote(statement) + `,"kind":"completion"}`),
	}
}

func mustQuote(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// TestLedgerAppendProjectReviewSupersedeEndToEnd walks the whole runtime
// against a real database: an agent proposes, a runner observes, a human
// reviews, and a correction supersedes -- with the projections read back
// after each step.
func TestLedgerAppendProjectReviewSupersedeEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, "test-ledger-e2e")

	// 1. An agent proposes a task and a completion claim.
	task, err := f.ledger.Append(ctx, ledger.Record{
		RecordType: ledger.RecordTask,
		RunID:      f.runID,
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: f.agentActor},
		Authority:  ledger.AuthorityProposed,
		Data:       json.RawMessage(`{"goal":"run the suite","status":"ready","assurance_state":"unverified"}`),
	})
	if err != nil {
		t.Fatalf("append task: %v", err)
	}
	claim, err := f.ledger.Append(ctx, f.claim(t, "the pinned suite exited zero"))
	if err != nil {
		t.Fatalf("append claim: %v", err)
	}

	ready, err := f.ledger.ProjectRun(ctx, f.runID, ledger.KindReadyTasks, "")
	if err != nil {
		t.Fatalf("ready tasks: %v", err)
	}
	if len(ready.Items) != 1 || ready.Items[0].ID != task.ID {
		t.Fatalf("ready tasks = %v, want [%s]", recordIDs(ready.Items), task.ID)
	}

	// 2. A runner observes the exit status, and only the exit status.
	evidence, err := f.ledger.Append(ctx, ledger.Record{
		RecordType: ledger.RecordEvidence,
		RunID:      f.runID,
		Origin:     ledger.Origin{Kind: ledger.OriginRunner, ActorID: f.runnerActor, ActorRevision: "sha256:runner"},
		Authority:  ledger.AuthorityObserved,
		SubjectRef: ledger.NullableID(task.ID),
		Data: json.RawMessage(`{"collection_method":"runner_wait_status",` +
			`"covered_scope":"Exit status of the pinned test command.",` +
			`"completeness":"partial","measurements":{"exit_code":0}}`),
		ProvenanceRefs: []string{claim.ID},
	}, ledger.WithRunnerManifest(ledger.RunnerManifest{
		ActorID:          f.runnerActor,
		ObservableFields: []string{"/collection_method", "/covered_scope", "/completeness", "/measurements"},
	}))
	if err != nil {
		t.Fatalf("append evidence: %v", err)
	}

	forTask, err := f.ledger.ProjectRun(ctx, f.runID, ledger.KindEvidenceFor, task.ID)
	if err != nil {
		t.Fatalf("evidence for subject: %v", err)
	}
	if len(forTask.Items) != 1 || forTask.Items[0].ID != evidence.ID {
		t.Fatalf("evidence for %s = %v, want [%s]", task.ID, recordIDs(forTask.Items), evidence.ID)
	}

	// 3. A human confirms the claim through a review transaction.
	version, err := f.ledger.LedgerVersion(ctx, f.runID)
	if err != nil {
		t.Fatalf("LedgerVersion: %v", err)
	}
	req, err := f.ledger.CreateReviewRequest(ctx, f.runID, []string{claim.ID}, version, ledger.WithReviewer(f.humanActor))
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}
	result, err := f.ledger.CommitReview(ctx, req.ID, map[string]ledger.Verdict{claim.ID: ledger.VerdictConfirm}, version)
	if err != nil {
		t.Fatalf("CommitReview: %v", err)
	}
	if len(result.Records) != 1 || result.Records[0].Authority != ledger.AuthorityConfirmed {
		t.Fatalf("review result = %+v, want one confirmed review record", result.Records)
	}

	confirmed, err := f.ledger.ProjectRun(ctx, f.runID, ledger.KindConfirmedClaims, "")
	if err != nil {
		t.Fatalf("confirmed claims: %v", err)
	}
	if len(confirmed.Items) != 1 || confirmed.Items[0].ID != claim.ID {
		t.Fatalf("confirmed claims = %v, want [%s]", recordIDs(confirmed.Items), claim.ID)
	}

	// The reviewed claim is untouched -- the database would not have let it
	// be otherwise.
	reread, err := f.ledger.Record(ctx, claim.ID)
	if err != nil {
		t.Fatalf("re-read the reviewed claim: %v", err)
	}
	if reread.Authority != ledger.AuthorityProposed || reread.ContentDigest != claim.ContentDigest {
		t.Fatalf("the reviewed claim changed: authority %q, digest %q", reread.Authority, reread.ContentDigest)
	}

	// 4. A correction supersedes the task; the superseded record leaves the
	// projections but stays in the ledger.
	running, err := f.ledger.AppendSuperseding(ctx, ledger.Record{
		RecordType: ledger.RecordTask,
		RunID:      f.runID,
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: f.agentActor},
		Authority:  ledger.AuthorityProposed,
		Data:       json.RawMessage(`{"goal":"run the suite","status":"running","assurance_state":"unverified"}`),
	}, task.ID)
	if err != nil {
		t.Fatalf("AppendSuperseding: %v", err)
	}

	ready, err = f.ledger.ProjectRun(ctx, f.runID, ledger.KindReadyTasks, "")
	if err != nil {
		t.Fatalf("ready tasks after supersession: %v", err)
	}
	if len(ready.Items) != 0 {
		t.Fatalf("ready tasks = %v, want none: the superseded task must not reappear", recordIDs(ready.Items))
	}
	active, err := f.ledger.ProjectRun(ctx, f.runID, ledger.KindActiveTasks, "")
	if err != nil {
		t.Fatalf("active tasks: %v", err)
	}
	if len(active.Items) != 1 || active.Items[0].ID != running.ID {
		t.Fatalf("active tasks = %v, want [%s]", recordIDs(active.Items), running.ID)
	}

	if _, err := f.ledger.Record(ctx, task.ID); err != nil {
		t.Fatalf("the superseded record must still be in the ledger: %v", err)
	}

	// 5. The delivery summary reports what actually happened, including the
	// evidence's partial coverage.
	summary, err := f.ledger.ProjectRun(ctx, f.runID, ledger.KindDeliverySummary, "")
	if err != nil {
		t.Fatalf("delivery summary: %v", err)
	}
	if summary.Summary.ConfirmedClaims != 1 {
		t.Fatalf("confirmed_claims = %d, want 1", summary.Summary.ConfirmedClaims)
	}
	if summary.Summary.SupersededRecords != 1 {
		t.Fatalf("superseded_records = %d, want 1", summary.Summary.SupersededRecords)
	}
	if summary.Summary.EvidenceByCompleteness["partial"] != 1 {
		t.Fatalf("evidence_by_completeness = %v, want one partial", summary.Summary.EvidenceByCompleteness)
	}
}

// TestLedgerRecordsRoundTripPreservesContentDigest proves the store loses
// nothing on the way in or out: a record read back from PostgreSQL still
// verifies against the digest it was appended with.
func TestLedgerRecordsRoundTripPreservesContentDigest(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, "test-ledger-roundtrip")

	appended, err := f.ledger.Append(ctx, f.claim(t, "a claim with every optional field set"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	reread, err := f.ledger.Record(ctx, appended.ID)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := reread.VerifyDigest(); err != nil {
		t.Fatalf("a record read back from PostgreSQL fails digest verification: %v", err)
	}
	if reread.Origin.ActorRevision != appended.Origin.ActorRevision {
		t.Fatalf("origin.actor_revision = %q, want %q — a dropped envelope field breaks the digest",
			reread.Origin.ActorRevision, appended.Origin.ActorRevision)
	}
	if reread.ContentDigest != appended.ContentDigest {
		t.Fatalf("content digest changed across the round trip: %q -> %q", appended.ContentDigest, reread.ContentDigest)
	}
}

// TestLedgerStaleReviewRollsBackTheWholeTransaction is the review guarantee
// against a real transaction: the version check fires after the review has
// begun, and PostgreSQL discards everything.
func TestLedgerStaleReviewRollsBackTheWholeTransaction(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, "test-ledger-stale")

	first, err := f.ledger.Append(ctx, f.claim(t, "reviewed"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	second, err := f.ledger.Append(ctx, f.claim(t, "also reviewed"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	version, err := f.ledger.LedgerVersion(ctx, f.runID)
	if err != nil {
		t.Fatalf("LedgerVersion: %v", err)
	}
	req, err := f.ledger.CreateReviewRequest(ctx, f.runID,
		[]string{first.ID, second.ID}, version, ledger.WithReviewer(f.humanActor))
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}

	// The ledger moves under the reviewer.
	if _, err := f.ledger.Append(ctx, f.claim(t, "appended after the reviewer looked")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	before := f.recordCount(t)

	_, err = f.ledger.CommitReview(ctx, req.ID, map[string]ledger.Verdict{
		first.ID:  ledger.VerdictConfirm,
		second.ID: ledger.VerdictConfirm,
	}, version)
	if !errors.Is(err, ledger.ErrStaleReview) {
		t.Fatalf("CommitReview error = %v, want ErrStaleReview", err)
	}

	if after := f.recordCount(t); after != before {
		t.Fatalf("record count = %d, want %d — a stale review must apply nothing", after, before)
	}

	stored, err := f.ledger.ReviewRequest(ctx, req.ID)
	if err != nil {
		t.Fatalf("re-read review: %v", err)
	}
	if stored.Status != ledger.ReviewRequested {
		t.Fatalf("review status = %q, want it left at %q", stored.Status, ledger.ReviewRequested)
	}
}

// TestLedgerAuthorityIsEnforcedAgainstTheRealStore proves the producer matrix
// runs before anything reaches the database.
func TestLedgerAuthorityIsEnforcedAgainstTheRealStore(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, "test-ledger-authority")

	promoted := f.claim(t, "an agent confirming itself")
	promoted.Authority = ledger.AuthorityConfirmed

	_, err := f.ledger.Append(ctx, promoted)
	var authErr *ledger.AuthorityError
	if !errors.As(err, &authErr) || authErr.Rule != ledger.RuleAgentProposesOnly {
		t.Fatalf("Append error = %v, want rule %s", err, ledger.RuleAgentProposesOnly)
	}
	if count := f.recordCount(t); count != 0 {
		t.Fatalf("record count = %d, want 0 — a refused append must not reach the database", count)
	}
}

// TestLedgerOriginActorMustBeRegistered proves a record cannot name a
// producer that does not exist: origin_actor_id is a foreign key, so an
// unregistered actor is refused by the database rather than accepted as a
// string nobody can resolve.
func TestLedgerOriginActorMustBeRegistered(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, "test-ledger-actor-fk")

	rec := f.claim(t, "produced by an actor nobody registered")
	rec.Origin.ActorID = "actor_never_registered_" + store.NewULID()

	if _, err := f.ledger.Append(ctx, rec); err == nil {
		t.Fatal("Append accepted a record naming an unregistered actor")
	}
}

// TestLedgerRefusesADuplicateRecordID proves the store has no overwrite path:
// re-appending an id is refused, not silently applied.
func TestLedgerRefusesADuplicateRecordID(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, "test-ledger-duplicate")

	first, err := f.ledger.Append(ctx, f.claim(t, "original"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	duplicate := f.claim(t, "an impostor reusing the id")
	duplicate.ID = first.ID

	if _, err := f.ledger.Append(ctx, duplicate); err == nil {
		t.Fatal("Append accepted a duplicate record id")
	}

	reread, err := f.ledger.Record(ctx, first.ID)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if reread.ContentDigest != first.ContentDigest {
		t.Fatal("the original record changed after a refused duplicate append")
	}
}

// TestLedgerSupersessionIsRefusedTwice proves the live-replacement guard
// holds against the real store.
func TestLedgerSupersessionIsRefusedTwice(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, "test-ledger-supersede")

	original, err := f.ledger.Append(ctx, f.claim(t, "original"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := f.ledger.AppendSuperseding(ctx, f.claim(t, "first correction"), original.ID); err != nil {
		t.Fatalf("first AppendSuperseding: %v", err)
	}

	before := f.recordCount(t)
	_, err = f.ledger.AppendSuperseding(ctx, f.claim(t, "second correction"), original.ID)
	if !errors.Is(err, ledger.ErrAlreadySuperseded) {
		t.Fatalf("second AppendSuperseding error = %v, want ErrAlreadySuperseded", err)
	}
	if after := f.recordCount(t); after != before {
		t.Fatalf("record count = %d, want %d", after, before)
	}
}

// TestLedgerVersionIsScopedToItsRun proves one run's appends never move
// another run's optimistic-concurrency token.
func TestLedgerVersionIsScopedToItsRun(t *testing.T) {
	ctx := context.Background()
	first := newLedgerFixture(t, "test-ledger-version-a")
	second := newLedgerFixture(t, "test-ledger-version-b")

	if _, err := first.ledger.Append(ctx, first.claim(t, "one")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := second.ledger.Append(ctx, second.claim(t, "two")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	for _, f := range []*ledgerFixture{first, second} {
		version, err := f.ledger.LedgerVersion(ctx, f.runID)
		if err != nil {
			t.Fatalf("LedgerVersion: %v", err)
		}
		if version != 1 {
			t.Fatalf("ledger version of run %s = %d, want 1", f.runID, version)
		}
	}
}

// TestLedgerConcurrentAppendsToOneRunStayCounted drives many writers at one
// run at once. The ledger version is a record count, so it is only a usable
// concurrency token if every concurrent append lands exactly once: no lost
// row, no duplicate id, and a final version equal to the number of appends.
func TestLedgerConcurrentAppendsToOneRunStayCounted(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, "test-ledger-concurrent")

	const writers = 20

	var wg sync.WaitGroup
	appended := make([]ledger.Record, writers)
	errs := make([]error, writers)

	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			appended[i], errs[i] = f.ledger.Append(ctx, f.claim(t, "concurrent claim"))
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Append %d: %v", i, err)
		}
		if seen[appended[i].ID] {
			t.Fatalf("two concurrent appends produced the same record id %s", appended[i].ID)
		}
		seen[appended[i].ID] = true
	}

	version, err := f.ledger.LedgerVersion(ctx, f.runID)
	if err != nil {
		t.Fatalf("LedgerVersion: %v", err)
	}
	if version != writers {
		t.Fatalf("ledger version = %d after %d concurrent appends, want %d", version, writers, writers)
	}
}

// TestLedgerAppendsQueueBehindTheRunLock is the direct evidence for the
// serialisation the review guards rest on: while any writer holds a run's
// advisory lock, an append to that run waits rather than slipping past, and
// it proceeds the moment the lock is released.
//
// Without this, a review could pass its ledger-version check and then have
// an append commit underneath it before it wrote its decisions.
func TestLedgerAppendsQueueBehindTheRunLock(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, "test-ledger-runlock")

	store, err := postgres.NewLedgerStore(f.store, f.namespaceID)
	if err != nil {
		t.Fatalf("NewLedgerStore: %v", err)
	}

	held := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan struct{})

	go func() {
		defer close(holderDone)
		// The holder rolls its transaction back, so it writes nothing and
		// the only thing under test is the lock it held.
		_ = store.InTx(ctx, func(ctx context.Context, tx ledger.Tx) error {
			if err := tx.Lock(ctx, ledger.RunLockKey(f.runID)); err != nil {
				return err
			}
			close(held)
			<-release
			return errors.New("holder rolls back on purpose")
		})
	}()

	<-held

	blocked, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if _, err := f.ledger.Append(blocked, f.claim(t, "should wait for the lock")); err == nil {
		t.Fatal("an append completed while another writer held the run lock")
	}

	close(release)
	<-holderDone

	if _, err := f.ledger.Append(ctx, f.claim(t, "proceeds once the lock is released")); err != nil {
		t.Fatalf("Append after the lock was released: %v", err)
	}
	if count := f.recordCount(t); count != 1 {
		t.Fatalf("record count = %d, want 1 — only the unblocked append should have landed", count)
	}
}

// TestLedgerConcurrentReviewCommitsElectOneWinner proves a review is a
// transaction and not a toggle: two commits of the same request racing each
// other produce exactly one applied review.
func TestLedgerConcurrentReviewCommitsElectOneWinner(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, "test-ledger-race-review")

	target, err := f.ledger.Append(ctx, f.claim(t, "reviewed by two racing commits"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	version, err := f.ledger.LedgerVersion(ctx, f.runID)
	if err != nil {
		t.Fatalf("LedgerVersion: %v", err)
	}
	req, err := f.ledger.CreateReviewRequest(ctx, f.runID, []string{target.ID}, version, ledger.WithReviewer(f.humanActor))
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			_, results[i] = f.ledger.CommitReview(ctx, req.ID,
				map[string]ledger.Verdict{target.ID: ledger.VerdictConfirm}, version)
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		if !errors.Is(err, ledger.ErrReviewAlreadyCommitted) && !errors.Is(err, ledger.ErrStaleReview) {
			t.Fatalf("losing commit failed with %v, want ErrReviewAlreadyCommitted or ErrStaleReview", err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d of 2 racing commits succeeded, want exactly 1", winners)
	}

	if count := f.recordCount(t); count != 2 {
		t.Fatalf("record count = %d, want 2 (the claim and one review record)", count)
	}
}

// TestLedgerProjectionCheckpointsAreIdempotentAndCatchDrift proves the
// checkpoint table does the job it exists for: the same projection at the
// same ledger version can be recorded repeatedly, and a digest that differs
// for the same version is reported as the determinism failure it is.
func TestLedgerProjectionCheckpointsAreIdempotentAndCatchDrift(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, "test-ledger-checkpoint")

	if _, err := f.ledger.Append(ctx, f.claim(t, "one")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	version, err := f.ledger.LedgerVersion(ctx, f.runID)
	if err != nil {
		t.Fatalf("LedgerVersion: %v", err)
	}
	summary, err := f.ledger.ProjectRun(ctx, f.runID, ledger.KindDeliverySummary, "")
	if err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}

	store, err := postgres.NewLedgerStore(f.store, f.namespaceID)
	if err != nil {
		t.Fatalf("NewLedgerStore: %v", err)
	}

	if err := store.CheckpointProjection(ctx, f.runID, version, summary); err != nil {
		t.Fatalf("first CheckpointProjection: %v", err)
	}
	if err := store.CheckpointProjection(ctx, f.runID, version, summary); err != nil {
		t.Fatalf("re-checkpointing an identical projection must succeed: %v", err)
	}

	drifted := summary
	drifted.Digest = "sha256:" + strings.Repeat("0", 64)
	if err := store.CheckpointProjection(ctx, f.runID, version, drifted); err == nil {
		t.Fatal("CheckpointProjection accepted a projection whose digest does not match its content")
	}

	// A projection that is internally consistent but disagrees with the
	// recorded checkpoint for the same version is a determinism failure.
	forged := summary
	forged.Summary = &ledger.DeliveryCounts{RunID: f.runID, LiveRecords: 99}
	forgedDigest, err := forged.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	forged.Digest = forgedDigest
	err = store.CheckpointProjection(ctx, f.runID, version, forged)
	if err == nil {
		t.Fatal("CheckpointProjection accepted a second, different digest for the same ledger version")
	}
	if !strings.Contains(err.Error(), "identical ledger inputs") {
		t.Fatalf("error = %v, want it to name the determinism violation", err)
	}
}

func (f *ledgerFixture) recordCount(t *testing.T) int {
	t.Helper()
	var count int
	err := f.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM ledger_records WHERE run_id = $1`, f.runID).Scan(&count)
	if err != nil {
		t.Fatalf("count ledger_records: %v", err)
	}
	return count
}

func recordIDs(records []ledger.Record) []string {
	out := make([]string, 0, len(records))
	for _, rec := range records {
		out = append(out, rec.ID)
	}
	return out
}
