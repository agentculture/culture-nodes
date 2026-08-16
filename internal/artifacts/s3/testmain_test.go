package s3_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/agentculture/culture-nodes/internal/artifacts/artifactstest"
	"github.com/agentculture/culture-nodes/internal/store"
	pgstore "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// This package needs two backends: Postgres (artifact metadata, shared with
// internal/artifacts/postgres) and MinIO (object content). Both are
// provisioned once for the whole package, mirroring
// internal/store/postgres/testmain_test.go's pattern; individual tests call
// requireStore/requireMinIO, which t.Skip() when a backend is unavailable
// rather than silently passing.
var (
	testStore          *pgstore.Store
	testMinIOEndpoint  string
	testMinIOAccessKey string
	testMinIOSecretKey string
	minioAvailable     bool
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()

	dbURL := os.Getenv(pgtest.DatabaseURLEnv)
	var releasePG func()
	if dbURL == "" {
		url, stop, err := artifactstest.StartPostgres(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"artifacts/s3 tests: no NODES_TEST_DATABASE_URL and no usable Docker for postgres (%v); all tests will report skipped\n", err)
			return m.Run()
		}
		dbURL = url
		releasePG = stop
	} else {
		// The variable names a server, not a database to share. See
		// pgtest.IsolatedDatabase.
		isolated, drop, err := pgtest.IsolatedDatabase(ctx, dbURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "artifacts/s3 tests: %v\n", err)
			return 1
		}
		dbURL = isolated
		releasePG = drop
	}
	if releasePG != nil {
		defer releasePG()
	}

	s, err := pgstore.Connect(ctx, dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "artifacts/s3 tests: connect to %s: %v\n", dbURL, err)
		return 1
	}
	defer s.Close()

	if _, err := s.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "artifacts/s3 tests: initial migrate: %v\n", err)
		return 1
	}
	testStore = s

	endpoint, accessKey, secretKey, stopMinIO, err := artifactstest.StartMinIO(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"artifacts/s3 tests: no usable Docker for minio (%v); all tests will report skipped\n", err)
		return m.Run()
	}
	defer stopMinIO()

	testMinIOEndpoint = endpoint
	testMinIOAccessKey = accessKey
	testMinIOSecretKey = secretKey
	minioAvailable = true

	return m.Run()
}

func requireBackends(t *testing.T) *pgstore.Store {
	t.Helper()
	if testStore == nil {
		t.Skip("no PostgreSQL available for this test: set NODES_TEST_DATABASE_URL, or ensure Docker can run postgres:17-alpine")
	}
	if !minioAvailable {
		t.Skip("no MinIO available for this test: ensure Docker can run minio/minio")
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
