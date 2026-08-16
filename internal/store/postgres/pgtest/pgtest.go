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
//
// # One database per test binary
//
// Whichever way the server is obtained, every test binary gets its OWN
// database on it (see IsolatedDatabase). That is not a nicety: several of
// this repo's components are deliberately deployment-wide rather than
// namespace-scoped -- the outbox relay drains every pending row it can see
// (internal/events/relay.go), and the scheduler claims every due timer in
// one batch -- so two test binaries pointed at one database silently steal
// each other's rows.
//
// Locally that never showed, because an unset NODES_TEST_DATABASE_URL gives
// each package a private container. CI set the variable to a single shared
// database for `go test ./...`, so ~20 concurrently running packages shared
// one `outbox` and one `timers` table, and the relay/scheduler/engine suites
// failed intermittently (issue #126). Creating a per-binary database keeps
// the two modes behaviourally identical instead of leaving CI on a path no
// developer ever runs.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// DatabaseURLEnv names the environment variable that points the test suite
// at an already-running PostgreSQL server. It holds a server, not a
// database to use directly: see IsolatedDatabase.
const DatabaseURLEnv = "NODES_TEST_DATABASE_URL"

// Run is a TestMain body for a package whose tests need a real, migrated
// PostgreSQL instance. It creates a private database on the server
// NODES_TEST_DATABASE_URL names, or starts an ephemeral postgres:17-alpine
// container otherwise (stopped before Run returns), connects, applies every
// migration via Store.Migrate, calls onReady with the resulting
// *postgres.Store, runs m.Run(), and returns its exit code.
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

	dbURL := os.Getenv(DatabaseURLEnv)
	var release func()

	if dbURL == "" {
		containerURL, stop, err := startDockerPostgres(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"pgtest: no %s and no usable Docker (%v); all tests will report skipped\n", DatabaseURLEnv, err)
			return m.Run()
		}
		dbURL = containerURL
		release = stop
	} else {
		// A server was named, so it is expected to work. Failing to carve
		// out a private database is a hard error, never a skip: skipping
		// here would turn a broken environment into a green run, which is
		// the exact false-green the CI "database-backed tests actually ran"
		// guard exists to catch.
		isolated, drop, err := IsolatedDatabase(ctx, dbURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pgtest: %v\n", err)
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

// IsolatedDatabase treats baseURL as naming a SERVER rather than a database
// to use directly: it creates a fresh, uniquely named database on that
// server and returns a URL for it plus a function that drops it again.
//
// This is what keeps a shared CI PostgreSQL service behaviourally identical
// to the per-package throwaway container a developer gets locally. Test
// binaries running concurrently against one database contaminate each other
// through the components that are deployment-wide by design -- the outbox
// relay drains every pending row, the scheduler claims every due timer --
// and no amount of per-test namespacing helps, because those components
// deliberately ignore namespaces.
//
// A separate database, rather than a separate schema, is deliberate on two
// counts. PostgreSQL advisory locks are scoped to a database, so
// Store.Migrate's lock only stops serialising every package's migration
// against every other one once the databases differ. And the returned URL
// stays an ordinary connection URL, so psql, pg_dump and any other client
// can use it -- a `search_path` query parameter would work for pgx and
// break libpq.
//
// The name is unique per call, not derived deterministically from the
// package, so a rerun never inherits the previous run's rows. (Tests that
// insert fixed primary keys, e.g. internal/worker's registry suite, depend
// on that freshness.)
//
// Requires permission to CREATE DATABASE on the server. That is deliberately
// a hard requirement with a clear error rather than a silent fallback to the
// shared database: one mechanism with one behaviour is the whole point --
// the bug being fixed here was CI quietly running a path no developer ran.
func IsolatedDatabase(ctx context.Context, baseURL string) (dbURL string, drop func(), err error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", nil, fmt.Errorf("parse %s: %w", DatabaseURLEnv, err)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		// pgx also accepts keyword/value DSNs ("host=... dbname=..."), which
		// cannot be rewritten by swapping a URL path. Say so, rather than
		// handing back a mangled string that fails as a connection error.
		return "", nil, fmt.Errorf(
			"%s must be a postgres:// URL so a per-test database can be substituted into it, got %q",
			DatabaseURLEnv, parsed.Scheme)
	}

	name := isolatedDatabaseName()
	quoted := pgx.Identifier{name}.Sanitize()

	admin, err := postgres.Connect(ctx, baseURL)
	if err != nil {
		return "", nil, fmt.Errorf("connect to the server named by %s: %w", DatabaseURLEnv, err)
	}
	_, createErr := admin.Pool().Exec(ctx, "CREATE DATABASE "+quoted)
	admin.Close()
	if createErr != nil {
		return "", nil, fmt.Errorf(
			"create isolated test database %s: %w (the role needs CREATEDB; or unset %s to use a throwaway Docker container instead)",
			name, createErr, DatabaseURLEnv)
	}

	isolated := *parsed
	isolated.Path = "/" + name

	return isolated.String(), func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, connErr := postgres.Connect(dropCtx, baseURL)
		if connErr != nil {
			fmt.Fprintf(os.Stderr, "pgtest: leaked test database %s (reconnect to drop it: %v)\n", name, connErr)
			return
		}
		defer conn.Close()
		// FORCE terminates any connection a subprocess under test still
		// holds; without it a worker binary that outlived its test keeps
		// the database alive forever.
		if _, dropErr := conn.Pool().Exec(dropCtx, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)"); dropErr != nil {
			fmt.Fprintf(os.Stderr, "pgtest: leaked test database %s (%v)\n", name, dropErr)
		}
	}, nil
}

// isolatedDatabaseName builds a per-invocation database name that is unique,
// a legal unquoted PostgreSQL identifier (<= 63 bytes, lower case), and
// traceable back to the package that created it -- os.Args[0] for a test
// binary is ".../<package>.test", which is what shows up in `\l` when a
// crashed run leaks one.
func isolatedDatabaseName() string {
	hint := strings.TrimSuffix(filepath.Base(os.Args[0]), ".test")
	var safe strings.Builder
	for _, r := range strings.ToLower(hint) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			safe.WriteRune(r)
			continue
		}
		safe.WriteRune('_')
	}
	pkg := strings.Trim(safe.String(), "_")
	if len(pkg) > 20 {
		pkg = pkg[:20]
	}
	if pkg == "" {
		pkg = "pkg"
	}
	return "nodes_test_" + pkg + "_" + strings.ToLower(store.NewULID())
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

	containerURL := fmt.Sprintf("postgres://postgres:nodes@127.0.0.1:%s/nodes?sslmode=disable", port)

	if waitErr := waitForPostgres(ctx, containerURL, 45*time.Second); waitErr != nil {
		stopFn()
		return "", nil, fmt.Errorf("postgres container %s did not become ready: %w", name, waitErr)
	}

	return containerURL, stopFn, nil
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
