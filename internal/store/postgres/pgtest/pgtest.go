// Package pgtest provisions a real, fully-migrated PostgreSQL instance for
// test packages that need one -- internal/store/postgres itself, plus later
// tasks' store subpackages (internal/queue/postgres, internal/events, ...)
// that read or write the same tables. It exists so every such package's
// TestMain shares one implementation of "resolve NODES_TEST_DATABASE_URL, or
// start an ephemeral `docker run -d --rm postgres:17-alpine`, migrate, hand
// off" instead of re-deriving it.
//
// internal/store/postgres/testmain_test.go predates this package (task t6)
// and is intentionally left as its own copy of the same pattern rather than
// refactored to depend on it -- see docs/skill-sources.md-style provenance
// notes in this repo's task history (t10) for why: a _test.go file cannot be
// imported by another package, so sharing would require either promoting
// that file's logic out of _test.go (a change to an existing, already
// shipped file) or having every package duplicate it. This package is the
// non-duplicating answer for every *new* postgres-backed test package;
// internal/store/postgres's own suite keeps its original, untouched copy.
package pgtest

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
)

// Run is a TestMain body for a package whose tests need a real, migrated
// PostgreSQL instance. It resolves NODES_TEST_DATABASE_URL, or starts an
// ephemeral postgres:17-alpine container otherwise (stopped before Run
// returns), connects, applies every migration via Store.Migrate, calls
// onReady with the resulting *postgres.Store, runs m.Run(), and returns its
// exit code.
//
// If neither an explicit test database nor a usable Docker install is
// available, onReady is never called -- so a package that stashes the store
// in a package variable inside onReady sees that variable stay nil -- and
// m.Run() still executes. Individual tests are expected to skip themselves
// via RequireStore, so `go test` reports "skipped" for those tests rather
// than a silent pass or a hard suite failure. This mirrors
// internal/store/postgres/testmain_test.go's run() exactly.
func Run(m *testing.M, onReady func(*postgres.Store)) int {
	ctx := context.Background()

	dbURL := os.Getenv("NODES_TEST_DATABASE_URL")
	var stopContainer func()

	if dbURL == "" {
		url, stop, err := startDockerPostgres(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"pgtest: no NODES_TEST_DATABASE_URL and no usable Docker (%v); all tests will report skipped\n", err)
			return m.Run()
		}
		dbURL = url
		stopContainer = stop
	}
	if stopContainer != nil {
		defer stopContainer()
	}

	s, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: connect to %s: %v\n", dbURL, err)
		return 1
	}
	defer s.Close()

	if _, err := s.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: initial migrate: %v\n", err)
		return 1
	}

	onReady(s)
	return m.Run()
}

// RequireStore is the per-test skip helper: it t.Skips with a standard
// message when s is nil (the "no Postgres available" case Run leaves
// behind), and otherwise returns s unchanged.
func RequireStore(t *testing.T, s *postgres.Store) *postgres.Store {
	t.Helper()
	if s == nil {
		t.Skip("no PostgreSQL available for this test: set NODES_TEST_DATABASE_URL, or ensure Docker can run postgres:17-alpine")
	}
	return s
}

// MustNamespace creates a namespace with a ULID-suffixed slug so parallel
// and repeated test runs never collide on the namespaces.slug uniqueness
// constraint -- the same convention internal/store/postgres's own tests use.
func MustNamespace(t *testing.T, s *postgres.Store, slugPrefix string) postgres.Namespace {
	t.Helper()
	ns, err := s.CreateNamespace(context.Background(), slugPrefix+"-"+store.NewULID(), "Test Namespace")
	if err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	return ns
}

// startDockerPostgres starts postgres:17-alpine detached with Docker
// choosing the host port, waits for it to accept connections, and returns
// its connection URL and a stop function. It returns a non-nil error --
// without touching *testing.T -- when Docker is not usable at all, so the
// caller decides whether that should skip or fail the suite.
func startDockerPostgres(ctx context.Context) (dbURL string, stop func(), err error) {
	if _, lookErr := exec.LookPath("docker"); lookErr != nil {
		return "", nil, fmt.Errorf("docker not found on PATH: %w", lookErr)
	}

	name := fmt.Sprintf("nodes-pgtest-%d", time.Now().UnixNano())

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
