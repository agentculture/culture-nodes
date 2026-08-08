package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/agentculture/culture-nodes/internal/artifacts/artifactstest"
	"github.com/agentculture/culture-nodes/internal/store"
	pgstore "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// testStore is shared across every test in this package, mirroring
// internal/store/postgres/testmain_test.go's pattern (it is nil only when
// neither NODES_TEST_DATABASE_URL nor a usable Docker install is
// available; individual tests call requireStore(t), which t.Skip()s in
// that case).
var testStore *pgstore.Store

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()

	dbURL := os.Getenv("NODES_TEST_DATABASE_URL")
	var stopContainer func()

	if dbURL == "" {
		url, stop, err := artifactstest.StartPostgres(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"artifacts/postgres tests: no NODES_TEST_DATABASE_URL and no usable Docker (%v); all tests will report skipped\n", err)
			return m.Run()
		}
		dbURL = url
		stopContainer = stop
	}
	if stopContainer != nil {
		defer stopContainer()
	}

	s, err := pgstore.Connect(ctx, dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "artifacts/postgres tests: connect to %s: %v\n", dbURL, err)
		return 1
	}
	defer s.Close()

	if _, err := s.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "artifacts/postgres tests: initial migrate: %v\n", err)
		return 1
	}

	testStore = s
	return m.Run()
}

func requireStore(t *testing.T) *pgstore.Store {
	t.Helper()
	if testStore == nil {
		t.Skip("no PostgreSQL available for this test: set NODES_TEST_DATABASE_URL, or ensure Docker can run postgres:17-alpine")
	}
	return testStore
}

func mustNamespace(t *testing.T, s *pgstore.Store, slugPrefix string) pgstore.Namespace {
	t.Helper()
	ns, err := s.CreateNamespace(context.Background(), slugPrefix+"-"+store.NewULID(), "Test Namespace")
	if err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	return ns
}
