package postgres_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// testStore is shared across every test in this package. It is nil only
// when neither NODES_TEST_DATABASE_URL nor a usable Docker install is
// available; individual tests call requireStore(t), which t.Skip()s in
// that case (see the package doc below for why TestMain itself cannot
// call t.Skip).
var testStore *postgres.Store

// TestMain provisions one PostgreSQL instance for the whole package: either
// NODES_TEST_DATABASE_URL if set, or an ephemeral `docker run -d --rm
// postgres:17-alpine` container otherwise. It runs Migrate once so every
// test starts from the same fully-migrated schema, then hands off to
// individual tests, each of which creates its own uniquely-keyed fixture
// rows (see mustNamespace) so tests never collide with each other.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()

	dbURL := os.Getenv(pgtest.DatabaseURLEnv)
	var release func()

	if dbURL == "" {
		url, stop, err := startDockerPostgres(ctx)
		if err != nil {
			// Neither an explicit test database nor a usable Docker
			// install is available. Run the suite anyway: testStore stays
			// nil, and every test skips itself via requireStore(t) --
			// t.Skip, not a silent pass, so `go test` output says clearly
			// why nothing ran.
			fmt.Fprintf(os.Stderr,
				"postgres tests: no NODES_TEST_DATABASE_URL and no usable Docker (%v); all tests will report skipped\n", err)
			return m.Run()
		}
		dbURL = url
		release = stop
	} else {
		// The variable names a server, not a database to share: carve out
		// a private one, exactly as pgtest.Run does for every other
		// postgres-backed package. See pgtest.IsolatedDatabase.
		isolated, drop, err := pgtest.IsolatedDatabase(ctx, dbURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "postgres tests: %v\n", err)
			return 1
		}
		dbURL = isolated
		release = drop
	}
	if release != nil {
		defer release()
	}

	s, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres tests: connect to %s: %v\n", dbURL, err)
		return 1
	}
	defer s.Close()

	if _, err := s.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "postgres tests: initial migrate: %v\n", err)
		return 1
	}

	testStore = s
	return m.Run()
}

func requireStore(t *testing.T) *postgres.Store {
	t.Helper()
	if testStore == nil {
		t.Skip("no PostgreSQL available for this test: set NODES_TEST_DATABASE_URL, or ensure Docker can run postgres:17-alpine")
	}
	return testStore
}

// mustNamespace creates a namespace with a ULID-suffixed slug so parallel
// and repeated test runs never collide on the namespaces.slug uniqueness
// constraint.
func mustNamespace(t *testing.T, s *postgres.Store, slugPrefix string) postgres.Namespace {
	t.Helper()
	ns, err := s.CreateNamespace(context.Background(), slugPrefix+"-"+store.NewULID(), "Test Namespace")
	if err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	return ns
}

// startDockerPostgres starts postgres:17-alpine detached with Docker
// choosing the host port (avoids collisions with anything already
// listening), waits for it to accept connections, and returns its
// connection URL and a stop function. It returns a non-nil error --
// without touching *testing.T -- when Docker is not usable at all, so the
// caller decides whether that should skip or fail the suite.
func startDockerPostgres(ctx context.Context) (dbURL string, stop func(), err error) {
	if _, lookErr := exec.LookPath("docker"); lookErr != nil {
		return "", nil, fmt.Errorf("docker not found on PATH: %w", lookErr)
	}

	name := fmt.Sprintf("nodes-store-test-%d", time.Now().UnixNano())

	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	runCmd := exec.CommandContext(runCtx, "docker", "run", "-d", "--rm",
		"--name", name,
		"-e", "POSTGRES_PASSWORD=nodes",
		"-e", "POSTGRES_DB=nodes",
		"-p", "5432",
		"postgres:17-alpine",
	)
	if out, runErr := runCmd.CombinedOutput(); runErr != nil {
		return "", nil, fmt.Errorf("docker run postgres:17-alpine: %w (%s)", runErr, strings.TrimSpace(string(out)))
	}

	stopFn := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = exec.CommandContext(stopCtx, "docker", "stop", name).Run()
	}

	port, portErr := dockerHostPort(ctx, name, "5432/tcp")
	if portErr != nil {
		stopFn()
		return "", nil, fmt.Errorf("docker port %s: %w", name, portErr)
	}

	url := fmt.Sprintf("postgres://postgres:nodes@127.0.0.1:%s/nodes?sslmode=disable", port)

	if waitErr := waitForPostgres(ctx, url, 45*time.Second); waitErr != nil {
		stopFn()
		return "", nil, fmt.Errorf("postgres container %s did not become ready: %w", name, waitErr)
	}

	return url, stopFn, nil
}

// dockerHostPort returns the host port Docker mapped to containerPort
// (e.g. "5432/tcp") on the named container.
func dockerHostPort(ctx context.Context, name, containerPort string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "port", name, containerPort).Output()
	if err != nil {
		return "", err
	}
	// Output looks like "0.0.0.0:54321\n[::]:54321\n"; take the port after
	// the last colon on the first line.
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	idx := strings.LastIndex(line, ":")
	if idx == -1 || idx == len(line)-1 {
		return "", fmt.Errorf("unexpected `docker port` output: %q", string(out))
	}
	return line[idx+1:], nil
}

func waitForPostgres(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		connectCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		s, err := postgres.Connect(connectCtx, url)
		cancel()
		if err == nil {
			s.Close()
			return nil
		}
		lastErr = err
		time.Sleep(300 * time.Millisecond)
	}
	return lastErr
}
