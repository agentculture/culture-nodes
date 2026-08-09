// Package faulttest holds t7's process-level fault tests for work claiming
// (docs/initial-design/culture-nodes-prd-spec.md §12.4, §20.4): real OS
// worker processes racing, crashing, and recovering against one real
// PostgreSQL instance, proving what internal/store/postgres/claiming_test.go
// already proves with goroutines actually holds under process-level
// concurrency and an actual SIGKILL. See the doc comment atop
// internal/store/postgres/claiming.go for the full §12.4 invariant list
// and the test-to-§20.4-recovery-matrix-row mapping; claiming_fault_test.go
// carries the short version next to each test.
//
// This file provisions the shared fixtures every test in the package
// needs: one ephemeral PostgreSQL instance (mirroring
// internal/store/postgres's TestMain pattern -- duplicated here rather than
// imported, since Go test helpers in another package's _test.go files are
// not importable) and one compiled copy of testdata/worker, the throwaway
// binary the scenario tests exec as separate OS processes.
package faulttest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// testStore and testDBURL are shared across every test in this package,
// set up once by TestMain. testStore is nil (and testDBURL empty) only
// when neither NODES_TEST_DATABASE_URL nor a usable Docker install is
// available; requireStore(t) skips in that case.
var (
	testStore     *postgres.Store
	testDBURL     string
	workerBinPath string
)

// resultsTableDDL creates the fault tests' own results table -- a
// test-only construct, not a product migration. It stands in for the
// domain-level "effective completion" record a real ledger append (t8)
// would eventually be: work_id's PRIMARY KEY proves no work item is ever
// completed twice (the "two workers, no double commit" scenario), and
// UNIQUE(node_run_id, attempt) proves a duplicated signal -- two
// independent work_items rows for the same logical unit of work -- yields
// exactly one effective completion even though both work items reach
// work_items.state = 'completed' (technical status is not domain outcome;
// see repo CLAUDE.md).
const resultsTableDDL = `
CREATE TABLE IF NOT EXISTS test_work_results (
    work_id      TEXT PRIMARY KEY REFERENCES work_items (id),
    node_run_id  TEXT NOT NULL REFERENCES node_runs (id),
    attempt      INTEGER NOT NULL,
    completed_by TEXT NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (node_run_id, attempt)
)`

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
				"fault tests: no NODES_TEST_DATABASE_URL and no usable Docker (%v); all tests will report skipped\n", err)
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
		fmt.Fprintf(os.Stderr, "fault tests: connect to %s: %v\n", dbURL, err)
		return 1
	}
	defer s.Close()

	if _, err := s.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "fault tests: initial migrate: %v\n", err)
		return 1
	}
	if _, err := s.Pool().Exec(ctx, resultsTableDDL); err != nil {
		fmt.Fprintf(os.Stderr, "fault tests: create test_work_results: %v\n", err)
		return 1
	}

	binDir, err := os.MkdirTemp("", "nodes-fault-worker-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fault tests: create worker build dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(binDir)

	binPath := filepath.Join(binDir, "worker")
	buildCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	buildCmd := exec.CommandContext(buildCtx, "go", "build", "-o", binPath,
		"github.com/agentculture/culture-nodes/tests/fault/testdata/worker")
	if out, buildErr := buildCmd.CombinedOutput(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "fault tests: build worker binary: %v\n%s\n", buildErr, out)
		return 1
	}

	testDBURL = dbURL
	testStore = s
	workerBinPath = binPath
	return m.Run()
}

func requireStore(t *testing.T) *postgres.Store {
	t.Helper()
	if testStore == nil {
		t.Skip("no PostgreSQL available for this test: set NODES_TEST_DATABASE_URL, or ensure Docker can run postgres:17-alpine")
	}
	return testStore
}

// mustNamespace creates a namespace with a ULID-suffixed slug so repeated
// test runs never collide on the namespaces.slug uniqueness constraint.
func mustNamespace(t *testing.T, s *postgres.Store, slugPrefix string) postgres.Namespace {
	t.Helper()
	ns, err := s.CreateNamespace(context.Background(), slugPrefix+"-"+store.NewULID(), "Fault Test Namespace")
	if err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	return ns
}

// mustRun creates the workflow_version + run fixture chain a node_run
// (and, transitively, a work_items row) requires. runs/node_runs have no
// typed Store methods yet (t9 owns those); raw SQL via s.Pool() is the
// same escape hatch internal/store/postgres/ledger_test.go's
// insertTestLedgerRecord uses.
func mustRun(t *testing.T, s *postgres.Store, namespaceID string) string {
	t.Helper()
	ctx := context.Background()

	wv, err := s.CreateWorkflowVersion(ctx, postgres.CreateWorkflowVersionInput{
		NamespaceID:   namespaceID,
		WorkflowKey:   "fault-test-workflow-" + store.NewULID(),
		Version:       1,
		SourceFormat:  "yaml",
		Source:        "entrypoint: intake\n",
		ContentDigest: "sha256:" + store.NewULID(),
	})
	if err != nil {
		t.Fatalf("mustRun: CreateWorkflowVersion: %v", err)
	}

	runID := store.NewULID()
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO runs (id, namespace_id, workflow_version_id) VALUES ($1, $2, $3)`,
		runID, namespaceID, wv.ID,
	); err != nil {
		t.Fatalf("mustRun: insert run: %v", err)
	}
	return runID
}

// mustNodeRun inserts one node_run against runID.
func mustNodeRun(t *testing.T, s *postgres.Store, namespaceID, runID string) string {
	t.Helper()
	nodeRunID := store.NewULID()
	if _, err := s.Pool().Exec(context.Background(),
		`INSERT INTO node_runs (id, namespace_id, run_id, node_key) VALUES ($1, $2, $3, 'intake')`,
		nodeRunID, namespaceID, runID,
	); err != nil {
		t.Fatalf("mustNodeRun: insert node_run: %v", err)
	}
	return nodeRunID
}

// mustEnqueue enqueues one ready work item for nodeRunID.
func mustEnqueue(t *testing.T, s *postgres.Store, namespaceID, nodeRunID string) {
	t.Helper()
	if err := s.EnqueueWork(context.Background(), postgres.WorkItem{
		NamespaceID: namespaceID,
		NodeRunID:   nodeRunID,
	}); err != nil {
		t.Fatalf("EnqueueWork: %v", err)
	}
}

// waitForFlagFile polls for path to exist, for coordinating a SIGKILL to
// "after this worker has claimed something, before it can complete
// anything" without a fixed guess at timing.
func waitForFlagFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("flag file %s was not created within %s (worker never claimed anything)", path, timeout)
}

// waitForCompletedCount polls work_items until namespaceID has exactly
// want rows in state 'completed', or fails the test once timeout elapses.
// Recovery tests assert against database state on a deadline this way
// rather than waiting for a worker process to exit, because a worker's own
// idle timeout (how long it keeps polling for more work before quitting)
// is deliberately independent of how quickly recovery actually happened.
func waitForCompletedCount(t *testing.T, s *postgres.Store, namespaceID string, want int, timeout time.Duration) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	var got int
	for {
		if err := s.Pool().QueryRow(ctx,
			`SELECT count(*) FROM work_items WHERE namespace_id = $1 AND state = 'completed'`, namespaceID,
		).Scan(&got); err != nil {
			t.Fatalf("count completed work_items: %v", err)
		}
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed work_items = %d after %s, want %d (recovery did not happen within the deadline)", got, timeout, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// workerHandle is a running (or finished) copy of testdata/worker.
type workerHandle struct {
	cmd *exec.Cmd
	id  string
	out *strings.Builder
}

// workerConfig configures one exec'd copy of testdata/worker. See that
// package's doc comment for what each field becomes as an environment
// variable.
type workerConfig struct {
	namespaceID     string
	workerID        string
	leaseSeconds    float64
	limit           int
	workMS          int
	idleTimeoutMS   int
	claimedFlagFile string
}

// startWorker execs one copy of the compiled worker binary as a real,
// separate OS process against the shared ephemeral Postgres, configured
// entirely through environment variables (see testdata/worker/main.go's
// doc comment). It registers a t.Cleanup that force-kills the process if
// the test ends before the worker has, so a failing test never leaks a
// runaway worker process.
func startWorker(t *testing.T, cfg workerConfig) *workerHandle {
	t.Helper()

	cmd := exec.Command(workerBinPath)
	cmd.Env = append(os.Environ(),
		"WORKER_DB_URL="+testDBURL,
		"WORKER_ID="+cfg.workerID,
		"WORKER_NAMESPACE_ID="+cfg.namespaceID,
		fmt.Sprintf("WORKER_LEASE_SECONDS=%f", cfg.leaseSeconds),
		fmt.Sprintf("WORKER_LIMIT=%d", cfg.limit),
		fmt.Sprintf("WORKER_WORK_MS=%d", cfg.workMS),
		fmt.Sprintf("WORKER_IDLE_TIMEOUT_MS=%d", cfg.idleTimeoutMS),
	)
	if cfg.claimedFlagFile != "" {
		cmd.Env = append(cmd.Env, "WORKER_CLAIMED_FLAG_FILE="+cfg.claimedFlagFile)
	}

	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start worker %s: %v", cfg.workerID, err)
	}

	h := &workerHandle{cmd: cmd, id: cfg.workerID, out: &out}
	t.Cleanup(func() {
		if h.cmd.Process != nil {
			_ = h.cmd.Process.Kill()
		}
	})
	return h
}

// wait blocks until the worker process exits or timeout elapses (killing
// it in the latter case), returning the process's exit error, if any.
func (h *workerHandle) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		if h.cmd.Process != nil {
			_ = h.cmd.Process.Kill()
		}
		<-done
		return fmt.Errorf("worker %s did not exit within %s", h.id, timeout)
	}
}

// startDockerPostgres mirrors internal/store/postgres/testmain_test.go's
// helper of the same name (see that file for the rationale); duplicated
// here because _test.go helpers in one package are not importable from
// another.
func startDockerPostgres(ctx context.Context) (dbURL string, stop func(), err error) {
	if _, lookErr := exec.LookPath("docker"); lookErr != nil {
		return "", nil, fmt.Errorf("docker not found on PATH: %w", lookErr)
	}

	name := fmt.Sprintf("nodes-fault-test-%d", time.Now().UnixNano())

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

// waitForReclaim polls until at least one work item in the namespace has
// been claimed a second time (fencing_token >= 2: the victim's claim was 1,
// so any higher token proves a post-expiry reclaim) or completed, failing at
// deadline. This is the §20.4 "lease expires; another worker claims" moment
// itself, as distinct from the reclaimed work then finishing (see
// waitForCompletedCount's separate liveness bound at its call site).
func waitForReclaim(t *testing.T, s *postgres.Store, namespaceID string, deadline time.Time) {
	t.Helper()
	ctx := context.Background()
	for {
		var reclaimed int
		if err := s.Pool().QueryRow(ctx,
			`SELECT count(*) FROM work_items
			  WHERE namespace_id = $1
			    AND (fencing_token >= 2 OR state = 'completed')`, namespaceID,
		).Scan(&reclaimed); err != nil {
			t.Fatalf("count reclaimed work_items: %v", err)
		}
		if reclaimed > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no victim-held work item was reclaimed before the lease-expiry+5s bound (h19/§20.4)")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
