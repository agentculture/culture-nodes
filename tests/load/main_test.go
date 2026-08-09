// Package loadtest holds task t18's load measurements for the asynchronous
// runner-dispatch path (internal/worker/runnerasync.go, task t9): what a
// worker actually costs while it is waiting on many runner operations at once.
//
// # The claim under test
//
// PRD spec requirements c17/h12 say that with many concurrent in-flight runner
// operations the worker's memory stays bounded — no per-operation goroutine
// and no per-operation held connection — and that the status-sampling load
// scales with runners × interval rather than with how long an operation runs.
// t9 built the path that way on purpose (park, no goroutine between samples,
// claim-is-reschedule). This package MEASURES it. Nothing here assumes it.
//
// The measurements are structural, not absolute-threshold theatre. The load
// case is always compared against a small CONTROL fleet run through the same
// binary on the same host minutes earlier, because "goroutines did not scale
// with in-flight operations" is a statement about a slope, and a slope needs
// two points. A per-operation goroutine would put ninety more goroutines in
// the 100-operation process than in the 10-operation one; the assertion is
// that the difference is a small constant instead.
//
// # What is real here and what is a stub
//
// Real: the worker (a separate OS process running internal/worker's own Tick
// and SampleRunnerOperations), PostgreSQL, the engine, the compiler, the
// runner-protocol client, and the HTTP hop to the runner.
//
// Stubbed: the runner service itself (stub_test.go) — an in-test HTTP server
// that speaks api/runner-protocol and holds every operation `running` for a
// configurable duration. It is deliberately not headspace: this package needs
// to hold a hundred operations in flight for a controlled length of time,
// which is a property of the stub's clock, not of anything a real container
// runtime would give us.
//
// # Cost and how to run it
//
// The 100-operation case and the sampling-cost case run by default and take
// well under a minute between them against a local PostgreSQL. The
// 1,000-operation case is opt-in:
//
//	NODES_LOAD_1000=1 go test ./tests/load/ -run Thousand -v -timeout 20m
//
// Every test skips (never fails) when no PostgreSQL is reachable, the same way
// tests/fault does: set NODES_TEST_DATABASE_URL, or have Docker able to run
// postgres:17-alpine.
package loadtest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// testStore and testDBURL are shared across every test in this package, set up
// once by TestMain. testStore is nil (and testDBURL empty) only when neither
// NODES_TEST_DATABASE_URL nor a usable Docker install is available;
// requireStore(t) skips in that case.
var (
	testStore     *postgres.Store
	testDBURL     string
	loadWorkerBin string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()

	dbURL := os.Getenv("NODES_TEST_DATABASE_URL")
	var stopContainer func()

	if dbURL == "" {
		url, stop, err := startDockerPostgres(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"load tests: no NODES_TEST_DATABASE_URL and no usable Docker (%v); all tests will report skipped\n", err)
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
		fmt.Fprintf(os.Stderr, "load tests: connect to %s: %v\n", dbURL, err)
		return 1
	}
	defer s.Close()

	if _, err := s.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "load tests: initial migrate: %v\n", err)
		return 1
	}

	binDir, err := os.MkdirTemp("", "nodes-load-worker-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "load tests: create worker build dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(binDir)

	binPath := filepath.Join(binDir, "loadworker")
	buildCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	buildCmd := exec.CommandContext(buildCtx, "go", "build", "-o", binPath,
		"github.com/agentculture/culture-nodes/tests/load/testdata/loadworker")
	if out, buildErr := buildCmd.CombinedOutput(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "load tests: build loadworker binary: %v\n%s\n", buildErr, out)
		return 1
	}

	testDBURL = dbURL
	testStore = s
	loadWorkerBin = binPath
	return m.Run()
}

func requireStore(t *testing.T) *postgres.Store {
	t.Helper()
	if testStore == nil {
		t.Skip("no PostgreSQL available for this test: set NODES_TEST_DATABASE_URL, or ensure Docker can run postgres:17-alpine")
	}
	return testStore
}

// mustNamespace creates a namespace with a ULID-suffixed slug so repeated test
// runs never collide on the namespaces.slug uniqueness constraint. Every
// measured fleet gets its own namespace, which is what makes the parked count
// and the completed-run count clean readings rather than sums over history.
func mustNamespace(t *testing.T, s *postgres.Store, slugPrefix string) postgres.Namespace {
	t.Helper()
	ns, err := s.CreateNamespace(context.Background(), slugPrefix+"-"+store.NewULID(), "Load Test Namespace")
	if err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	return ns
}

// mustRunnerActor registers the producer identity the runner's observed
// evidence is attributed to (ledger_records.origin_actor_id is a real foreign
// key), as a real deployment must.
func mustRunnerActor(t *testing.T, s *postgres.Store, namespaceID string) string {
	t.Helper()
	actorID := "load-runner-" + store.NewULID()
	if _, err := s.Pool().Exec(context.Background(), `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		VALUES ($1, $2, $3, 1, 'runner', 'internal')`, actorID, namespaceID, actorID); err != nil {
		t.Fatalf("mustRunnerActor: %v", err)
	}
	return actorID
}

// compileWorkflow compiles testdata/runner.workflow.yaml once. The same
// compiled workflow seeds every run in a fleet, so a thousand runs cost a
// thousand engine transactions and exactly one compile.
func compileWorkflow(t *testing.T) *compiler.CompiledWorkflow {
	t.Helper()
	path := filepath.Join("testdata", "runner.workflow.yaml")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	cw, diags, err := compiler.Compile(source, compiler.FormatForPath(path))
	if err != nil {
		t.Fatalf("compile %s: %v", path, err)
	}
	for _, d := range diags {
		if d.Level == compiler.LevelError {
			t.Fatalf("compile %s: %s at %s: %s", path, d.Code, d.Path, d.Message)
		}
	}
	return cw
}

// startDockerPostgres mirrors internal/store/postgres/testmain_test.go's helper
// of the same name (see that file for the rationale); duplicated here because
// _test.go helpers in one package are not importable from another.
func startDockerPostgres(ctx context.Context) (dbURL string, stop func(), err error) {
	if _, lookErr := exec.LookPath("docker"); lookErr != nil {
		return "", nil, fmt.Errorf("docker not found on PATH: %w", lookErr)
	}

	name := fmt.Sprintf("nodes-load-test-%d", time.Now().UnixNano())

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

func dockerHostPort(ctx context.Context, name, containerPort string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "port", name, containerPort).Output()
	if err != nil {
		return "", err
	}
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
