package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// testLease is the lease duration handed to Store.ClaimWork in tests that
// drive a run forward by hand — long enough that no test's own runtime can
// ever cause a lease to expire mid-assertion.
const testLease = 2 * time.Minute

// fixture bundles a running httptest server over a fresh namespace with the
// underlying Go handles a test needs to drive a run forward directly (the
// same "hand-operated worker" shape internal/engine's own harness_test.go
// uses — see this package's package doc for why: driving CompleteAttempt
// through the real claiming path, not a shortcut, is what proves the SSE
// stream sees genuinely committed state).
type fixture struct {
	t      *testing.T
	server *httptest.Server
	api    *api.Server
	store  *storepg.Store
	nsID   string
	client *http.Client
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s := requireStore(t)

	nsID := pgtest.MustNamespace(t, s, "api").ID
	srv, err := api.NewServer(s, nsID, api.WithPollInterval(30*time.Millisecond))
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &fixture{t: t, server: ts, api: srv, store: s, nsID: nsID, client: ts.Client()}
}

// url joins the server's base URL with an API path.
func (f *fixture) url(path string) string {
	return f.server.URL + path
}

// insertActor inserts a minimal actor row, matching
// internal/engine/harness_test.go's own helper — a ledger record's
// origin.actor_id must both satisfy the envelope schema's identifier shape
// and the ledger_records.origin_actor_id foreign key, so tests that append
// agent-origin records need a real row to point at.
func (f *fixture) insertActor(key string) string {
	f.t.Helper()
	id := store.NewULID()
	_, err := f.store.Pool().Exec(context.Background(),
		`INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		 VALUES ($1, $2, $3, 1, 'agent', 'http')`,
		id, f.nsID, key+"-"+id)
	if err != nil {
		f.t.Fatalf("insert actor: %v", err)
	}
	return id
}

// readFixtureWorkflow reads a workflow definition from
// internal/compiler/testdata, the source of record for compiler-level
// fixtures (PRD-driven, already exercised by that package's own suite).
func readFixtureWorkflow(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "compiler", "testdata", name)
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return source
}

// claim claims the ready work item belonging to nodeRunID through the real
// claiming path (Store.ClaimWork), releasing back to 'ready' anything it
// wins that this test did not ask for — the same discipline
// internal/engine/harness_test.go's fixture.claim uses, since ClaimWork has
// no namespace filter and this package's tests share one PostgreSQL
// instance with every other package's.
func (f *fixture) claim(workerID, nodeRunID string) storepg.ClaimedWork {
	f.t.Helper()

	for attempt := 0; attempt < 40; attempt++ {
		claimed, err := f.store.ClaimWork(context.Background(), workerID, testLease, 20)
		if err != nil {
			f.t.Fatalf("ClaimWork: %v", err)
		}
		var found *storepg.ClaimedWork
		for i := range claimed {
			if claimed[i].NodeRunID == nodeRunID && found == nil {
				found = &claimed[i]
				continue
			}
			f.release(claimed[i].ID)
		}
		if found != nil {
			return *found
		}
		time.Sleep(25 * time.Millisecond)
	}
	f.t.Fatalf("no work item became claimable for node run %s", nodeRunID)
	return storepg.ClaimedWork{}
}

func (f *fixture) release(workID string) {
	f.t.Helper()
	_, err := f.store.Pool().Exec(context.Background(),
		`UPDATE work_items SET state = 'ready', lease_owner = NULL, lease_expires_at = NULL WHERE id = $1`, workID)
	if err != nil {
		f.t.Fatalf("release work item %s: %v", workID, err)
	}
}
