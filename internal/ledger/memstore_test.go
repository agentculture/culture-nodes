package ledger_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// memStore is an in-memory ledger.Store used by the unit and property tests.
//
// It exists for two reasons beyond speed. First, implementing ledger.Store
// from outside the package proves the interface is implementable without
// reaching into unexported state. Second, its InTx really does stage writes
// into a copy and discard them on error, so "a stale review applies nothing"
// is tested as a property of the runtime rather than only as a property of
// PostgreSQL — the real transaction is exercised in
// internal/store/postgres/ledger_store_test.go.
type memStore struct {
	mu      sync.Mutex
	records map[string]ledger.Record
	reviews map[string]ledger.ReviewRequest
	// actorKinds stands in for the actors table CommitReview resolves a
	// reviewer against. It is seeded with the shared fixture actors below,
	// each under the kind its name says it is — so a test that reviews as
	// testHuman works, and a test that reviews as testAgent is refused for
	// the same reason production would refuse it.
	actorKinds map[string]string
}

func newMemStore() *memStore {
	return &memStore{
		records: map[string]ledger.Record{},
		reviews: map[string]ledger.ReviewRequest{},
		actorKinds: map[string]string{
			testHuman:   ledger.ActorKindHuman,
			testAgent:   "agent",
			testRunner:  "runner",
			testEngine:  "engine",
			testService: "service",
		},
	}
}

func (m *memStore) InsertRecord(_ context.Context, rec ledger.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.records[rec.ID]; exists {
		return fmt.Errorf("memstore: record %s already exists (records are immutable)", rec.ID)
	}
	m.records[rec.ID] = rec.Clone()
	return nil
}

func (m *memStore) GetRecord(_ context.Context, id string) (ledger.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[id]
	if !ok {
		return ledger.Record{}, fmt.Errorf("memstore: %s: %w", id, ledger.ErrRecordNotFound)
	}
	return rec.Clone(), nil
}

func (m *memStore) RunRecords(_ context.Context, runID string) ([]ledger.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ledger.Record, 0, len(m.records))
	for _, rec := range m.records {
		if rec.RunID == runID {
			out = append(out, rec.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memStore) LedgerVersion(_ context.Context, runID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var count int64
	for _, rec := range m.records {
		if rec.RunID == runID {
			count++
		}
	}
	return count, nil
}

func (m *memStore) LiveSupersessors(_ context.Context, recordID string) ([]ledger.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	replaced := map[string]bool{}
	for _, rec := range m.records {
		if id := rec.Supersedes.String(); id != "" {
			replaced[id] = true
		}
	}

	out := make([]ledger.Record, 0)
	for _, rec := range m.records {
		if rec.Supersedes.String() == recordID && !replaced[rec.ID] {
			out = append(out, rec.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Lock is a no-op: the fake store is not concurrently mutated by the tests
// that use it, and pretending otherwise would fake a guarantee it cannot make.
func (m *memStore) Lock(context.Context, string) error { return nil }

func (m *memStore) InsertReviewRequest(_ context.Context, req ledger.ReviewRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.reviews[req.ID]; exists {
		return fmt.Errorf("memstore: review %s already exists", req.ID)
	}
	m.reviews[req.ID] = req.Clone()
	return nil
}

func (m *memStore) GetReviewRequest(_ context.Context, id string) (ledger.ReviewRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	req, ok := m.reviews[id]
	if !ok {
		return ledger.ReviewRequest{}, fmt.Errorf("memstore: %s: %w", id, ledger.ErrReviewNotFound)
	}
	return req.Clone(), nil
}

func (m *memStore) ActorKind(_ context.Context, actorID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kind, ok := m.actorKinds[actorID]
	if !ok {
		return "", fmt.Errorf("memstore: %s: %w", actorID, ledger.ErrActorNotFound)
	}
	return kind, nil
}

func (m *memStore) MarkReviewCommitted(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	req, ok := m.reviews[id]
	if !ok {
		return false, fmt.Errorf("memstore: %s: %w", id, ledger.ErrReviewNotFound)
	}
	if req.Status != ledger.ReviewRequested {
		return false, nil
	}
	req.Status = ledger.ReviewCommitted
	m.reviews[id] = req
	return true, nil
}

// InTx stages every write into a copy and adopts it only when fn succeeds, so
// a failed transaction leaves the store exactly as it found it.
func (m *memStore) InTx(ctx context.Context, fn func(context.Context, ledger.Tx) error) error {
	m.mu.Lock()
	staged := newMemStore()
	for id, rec := range m.records {
		staged.records[id] = rec.Clone()
	}
	for id, req := range m.reviews {
		staged.reviews[id] = req.Clone()
	}
	for id, kind := range m.actorKinds {
		staged.actorKinds[id] = kind
	}
	m.mu.Unlock()

	if err := fn(ctx, staged); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = staged.records
	m.reviews = staged.reviews
	return nil
}

func (m *memStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.records)
}

func (m *memStore) all() []ledger.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ledger.Record, 0, len(m.records))
	for _, rec := range m.records {
		out = append(out, rec.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// --- shared fixtures -------------------------------------------------------

const (
	testRunID   = "run_01TESTRUN0000000000000001"
	testAgent   = "actor_planner_v3"
	testHuman   = "actor_human_reviewer"
	testRunner  = "runner_headspace_docker"
	testEngine  = "engine_projection"
	testService = "service_notifier"
)

var (
	agentOrigin     = ledger.Origin{Kind: ledger.OriginAgent, ActorID: testAgent}
	humanOrigin     = ledger.Origin{Kind: ledger.OriginHuman, ActorID: testHuman}
	runnerOrigin    = ledger.Origin{Kind: ledger.OriginRunner, ActorID: testRunner}
	engineOrigin    = ledger.Origin{Kind: ledger.OriginEngine, ActorID: testEngine}
	validatorOrigin = ledger.Origin{Kind: ledger.OriginValidator, ActorID: testEngine}
	serviceOrigin   = ledger.Origin{Kind: ledger.OriginService, ActorID: testService}
)

// sharedValidator compiles the embedded schemas once for the whole test
// binary. The property tests build thousands of ledgers; recompiling every
// schema for each one would make the suite slow enough that nobody runs it.
var (
	validatorOnce sync.Once
	validator     *contracts.Validator
	validatorErr  error
)

func testValidator(t *testing.T) *contracts.Validator {
	t.Helper()
	validatorOnce.Do(func() { validator, validatorErr = contracts.NewValidator() })
	if validatorErr != nil {
		t.Fatalf("contracts.NewValidator: %v", validatorErr)
	}
	return validator
}

// newTestLedger returns a ledger over a fresh in-memory store, with
// deterministic ids and a fixed clock so a test's records — and therefore
// their digests — are reproducible.
func newTestLedger(t *testing.T) (*ledger.Ledger, *memStore) {
	t.Helper()
	store := newMemStore()

	var counter int
	l, err := ledger.New(store,
		ledger.WithValidator(testValidator(t)),
		ledger.WithIDFactory(func() string {
			counter++
			return fmt.Sprintf("%s%026d", ledger.IDPrefix, counter)
		}),
		ledger.WithClock(fixedClock()),
	)
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	return l, store
}

// fixedClock advances one second per call, so records are distinguishable in
// time without depending on how fast the test runs.
func fixedClock() func() time.Time {
	var ticks int64
	base := time.Date(2026, 8, 8, 15, 0, 0, 0, time.UTC)
	return func() time.Time {
		ticks++
		return base.Add(time.Duration(ticks) * time.Second)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture payload: %v", err)
	}
	return raw
}

// claimRecord is the archetypal agent proposal: a completion claim.
func claimRecord(t *testing.T, statement string) ledger.Record {
	t.Helper()
	return ledger.Record{
		RecordType: ledger.RecordClaim,
		RunID:      testRunID,
		Origin:     agentOrigin,
		Authority:  ledger.AuthorityProposed,
		Data:       mustJSON(t, map[string]any{"statement": statement, "kind": "completion"}),
	}
}

// gradeRecord is a grade evaluating evaluatedActorID, authored by origin at
// authority. Callers own picking an origin/authority combination the
// producer/authority matrix will actually admit.
func gradeRecord(t *testing.T, origin ledger.Origin, authority ledger.Authority, evaluatedActorID string, rating int) ledger.Record {
	t.Helper()
	return ledger.Record{
		RecordType: ledger.RecordGrade,
		RunID:      testRunID,
		Origin:     origin,
		Authority:  authority,
		Data: mustJSON(t, map[string]any{
			"rating":             rating,
			"rationale":          "Delivered the change set with clear evidence and no rework.",
			"evaluated_actor_id": evaluatedActorID,
		}),
	}
}

func taskRecord(t *testing.T, goal, status, assurance string) ledger.Record {
	t.Helper()
	payload := map[string]any{"goal": goal, "status": status}
	if assurance != "" {
		payload["assurance_state"] = assurance
	}
	return ledger.Record{
		RecordType: ledger.RecordTask,
		RunID:      testRunID,
		Origin:     agentOrigin,
		Authority:  ledger.AuthorityProposed,
		Data:       mustJSON(t, payload),
	}
}

func mustAppend(t *testing.T, l *ledger.Ledger, rec ledger.Record, opts ...ledger.AppendOption) ledger.Record {
	t.Helper()
	out, err := l.Append(context.Background(), rec, opts...)
	if err != nil {
		t.Fatalf("Append(%s/%s): %v", rec.RecordType, rec.Authority, err)
	}
	return out
}
