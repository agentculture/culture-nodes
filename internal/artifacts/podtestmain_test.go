package artifacts_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/agentculture/culture-nodes/internal/artifacts/artifactstest"
	pgstore "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// TestMain provisions the two backends the pod-agnostic proof (see
// integration_test.go) needs, once for the whole package: Postgres and
// MinIO, both ephemeral via Docker unless NODES_TEST_DATABASE_URL is set.
// Only their connection details are kept as package vars -- each test/pod
// that needs a live connection opens its own (see newPod), matching what a
// real replica does.
//
// This file's own tests -- ref_test.go, verify_test.go, router_test.go,
// grepguard_test.go -- do not touch these backends at all and run
// regardless of whether Docker is available.
var (
	testDBURL          string
	testMinIOEndpoint  string
	testMinIOAccessKey string
	testMinIOSecretKey string
	backendsAvailable  bool
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
				"artifacts tests: no NODES_TEST_DATABASE_URL and no usable Docker for postgres (%v); backend-requiring tests will report skipped\n", err)
			return m.Run()
		}
		dbURL = url
		releasePG = stop
	} else {
		// The variable names a server, not a database to share. See
		// pgtest.IsolatedDatabase.
		isolated, drop, err := pgtest.IsolatedDatabase(ctx, dbURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "artifacts tests: %v\n", err)
			return 1
		}
		dbURL = isolated
		releasePG = drop
	}
	if releasePG != nil {
		defer releasePG()
	}

	endpoint, accessKey, secretKey, stopMinIO, err := artifactstest.StartMinIO(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"artifacts tests: no usable Docker for minio (%v); backend-requiring tests will report skipped\n", err)
		return m.Run()
	}
	defer stopMinIO()

	// Apply migrations once via a bootstrap connection; individual pods
	// (see newPod in integration_test.go) open their own connections
	// afterward.
	boot, err := pgstore.Connect(ctx, dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "artifacts tests: connect to %s: %v\n", dbURL, err)
		return 1
	}
	if _, err := boot.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "artifacts tests: initial migrate: %v\n", err)
		boot.Close()
		return 1
	}
	boot.Close()

	testDBURL = dbURL
	testMinIOEndpoint = endpoint
	testMinIOAccessKey = accessKey
	testMinIOSecretKey = secretKey
	backendsAvailable = true

	return m.Run()
}

func requireBackends(t *testing.T) {
	t.Helper()
	if !backendsAvailable {
		t.Skip("no Postgres+MinIO available for this test: set NODES_TEST_DATABASE_URL, or ensure Docker can run postgres:17-alpine and minio/minio")
	}
}
