package devague_test

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// memStore is a minimal in-memory ledger.Store, used only by
// TestAuthorityHonestyMatchesLedgerRules to run a mapped record through the
// ledger's real Append/CreateReviewRequest/CommitReview lifecycle. It is not
// a fake of convenience: internal/ledger.Ledger requires a real Store to
// operate at all (there is no in-package pure-function shortcut for
// CommitReview, unlike the projections), and proving this package's
// confirmed review records match what CommitReview actually produces needs a
// real — if minimal — one. Modeled on the shape ledger.Tx/ledger.Store
// require, not copied from internal/ledger's own (unexported, package-private)
// test store.
type memStore struct {
	mu      sync.Mutex
	records map[string]ledger.Record
	reviews map[string]ledger.ReviewRequest
	// actorKinds stands in for the actors table CommitReview resolves a
	// reviewer against, seeded with the human reviewer these tests decide as.
	actorKinds map[string]string
}

func newMemStore() *memStore {
	return &memStore{
		records:    map[string]ledger.Record{},
		reviews:    map[string]ledger.ReviewRequest{},
		actorKinds: map[string]string{"actor_human_reviewer": ledger.ActorKindHuman},
	}
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

func (m *memStore) InsertRecord(_ context.Context, rec ledger.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.records[rec.ID]; exists {
		return fmt.Errorf("memstore: record %s already exists", rec.ID)
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

// Lock is a no-op: this store is never mutated concurrently in these tests.
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

// InTx stages every write into a copy and adopts it only once fn succeeds.
func (m *memStore) InTx(ctx context.Context, fn func(context.Context, ledger.Tx) error) error {
	m.mu.Lock()
	staged := newMemStore()
	for id, rec := range m.records {
		staged.records[id] = rec.Clone()
	}
	for id, req := range m.reviews {
		staged.reviews[id] = req.Clone()
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
