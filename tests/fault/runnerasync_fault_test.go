package faulttest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Task t9, acceptance criterion 3: a worker killed mid-operation strands
// nothing.
//
// This is the criterion the rest of the design exists to make true, and it is
// worth being precise about what "strands nothing" means here, because it is a
// stronger statement than the lease-recovery case
// TestFaultKilledWorkerReclaimedBySurvivor proves:
//
//   - There is no lease to recover. The victim parked the work item before it
//     died, so the item is in 'waiting' — invisible to ClaimWork AND to
//     ReclaimExpired. The survivor does not wait for a lease to expire; it
//     reads the parked operation out of runner_invocations and samples it.
//     Recovery is therefore bounded by the SAMPLING interval, not by lease
//     expiry.
//   - The operation itself is untouched by the death. It kept running in the
//     runner's own process the whole time, and the survivor learns its result
//     from the same status endpoint the victim would have read. The runner
//     never sees a second dispatch — proven below by the service's own
//     dispatch counter — so a worker crash is not an at-least-twice execution.
//   - Nothing about the handoff is special-cased. The survivor is configured
//     identically to the victim and runs the same worker loop; it has no idea
//     it is a survivor.

const (
	faultRunnerSecretRef = "runner/fault/execute-token"
	faultRunnerSecret    = "fault-execute-token-not-the-ref"
	faultRunnerRef       = "runner://headspace/docker@sha256:5555555555555555555555555555555555555555555555555555555555555555"
	faultRunnerDigest    = "sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de"
	faultRunnerName      = "headspace"
	// faultSamplePoll is the sampler's cadence, and the first half of the
	// recovery bound stated below.
	faultSamplePoll = 250 * time.Millisecond
)

// faultRunnerService is a real HTTP runner service speaking
// api/runner-protocol, shared by both worker processes over a real socket. It
// stays `running` until finish() is called, which is what lets the test kill
// the victim at a point where the outcome is genuinely not yet knowable.
type faultRunnerService struct {
	mu         sync.Mutex
	dispatched map[string]runners.Operation
	terminal   map[string]*runners.Result
	dispatches int
	statuses   int
}

func newFaultRunnerService() *faultRunnerService {
	return &faultRunnerService{
		dispatched: map[string]runners.Operation{},
		terminal:   map[string]*runners.Result{},
	}
}

func (f *faultRunnerService) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+faultRunnerSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == runners.OperationsPath:
			var op runners.Operation
			if err := json.NewDecoder(r.Body).Decode(&op); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.dispatches++
			f.dispatched[op.OperationID] = op
			f.mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(runners.Acceptance{
				OperationID:            op.OperationID,
				StatusRetentionSeconds: 86400,
			})

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, runners.OperationsPath+"/"):
			id := strings.TrimPrefix(r.URL.Path, runners.OperationsPath+"/")
			f.mu.Lock()
			f.statuses++
			_, known := f.dispatched[id]
			result := f.terminal[id]
			f.mu.Unlock()
			if !known {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			envelope := runners.OperationStatus{OperationID: id, State: runners.StateRunning}
			if result != nil {
				envelope.State = result.State
				envelope.Result = result
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(envelope)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// finish makes every dispatched operation report a completed result.
func (f *faultRunnerService) finish() {
	f.mu.Lock()
	defer f.mu.Unlock()
	exit := 0
	for id, op := range f.dispatched {
		finished := time.Now().UTC()
		f.terminal[id] = &runners.Result{
			OperationID: id,
			State:       runners.StateCompleted,
			Exit:        &runners.Exit{Code: &exit},
			Timing: runners.Timing{
				StartedAt: finished.Add(-time.Second), FinishedAt: finished, DurationMs: 1000,
			},
			Environment: runners.Environment{
				ImageDigest:  op.Execution.ImageDigest,
				PolicyDigest: "sha256:" + strings.Repeat("c", 64),
			},
			Observations: runners.Observations{
				ExitStatus: runners.Observation{Measured: true, Complete: true, Method: "container_wait_status"},
			},
		}
	}
}

func (f *faultRunnerService) counts() (dispatches, statuses int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dispatches, f.statuses
}

// samplerBinOnce builds tests/fault/testdata/runnersampler once per test
// binary. It is built here rather than in TestMain so this task adds a fixture
// without changing the harness t7's own tests share.
var (
	samplerBinOnce sync.Once
	samplerBinPath string
	samplerBinErr  error
)

func runnerSamplerBinary(t *testing.T) string {
	t.Helper()
	samplerBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "nodes-fault-runnersampler-")
		if err != nil {
			samplerBinErr = err
			return
		}
		path := filepath.Join(dir, "runnersampler")
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "build", "-o", path,
			"github.com/agentculture/culture-nodes/tests/fault/testdata/runnersampler")
		if out, err := cmd.CombinedOutput(); err != nil {
			samplerBinErr = fmt.Errorf("build runnersampler: %w\n%s", err, out)
			return
		}
		samplerBinPath = path
	})
	if samplerBinErr != nil {
		t.Fatalf("%v", samplerBinErr)
	}
	return samplerBinPath
}

// startSampler execs one copy of the sampler binary as a real OS process.
func startSampler(t *testing.T, env map[string]string) *workerHandle {
	t.Helper()
	cmd := exec.Command(runnerSamplerBinary(t))
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sampler %s: %v", env["SAMPLER_WORKER_ID"], err)
	}
	h := &workerHandle{cmd: cmd, id: env["SAMPLER_WORKER_ID"], out: &out}
	t.Cleanup(func() {
		if h.cmd.Process != nil {
			_ = h.cmd.Process.Kill()
		}
	})
	return h
}

// A worker killed after parking a runner operation strands nothing: the
// surviving worker's own sampler picks the parked operation up and commits its
// result, within one sampling interval plus five seconds.
//
// The five-second slack mirrors TestFaultKilledWorkerReclaimedBySurvivor's own
// bound and exists for the same reason: on a loaded host the survivor's next
// pass can legitimately be a little late without the recovery promise being
// violated. What the bound is measured against is the moment the runner's
// operation actually became answerable — not the kill — because before that
// there is nothing for any worker, surviving or otherwise, to learn.
func TestFaultKilledWorkerParkedRunnerOperationResumedBySurvivor(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	service := newFaultRunnerService()
	server := httptest.NewServer(service.handler())
	defer server.Close()

	ns := mustNamespace(t, s, "fault-runner-async")
	actorID := mustRunnerActor(t, s, ns.ID)
	runID := mustRunnerRun(t, s, ns.ID)

	env := map[string]string{
		"SAMPLER_DB_URL":          testDBURL,
		"SAMPLER_NAMESPACE_ID":    ns.ID,
		"SAMPLER_RUNNER_REF":      faultRunnerRef,
		"SAMPLER_RUNNER_ENDPOINT": server.URL,
		"SAMPLER_RUNNER_DIGEST":   faultRunnerDigest,
		"SAMPLER_SECRET_REF":      faultRunnerSecretRef,
		"SAMPLER_SECRET":          faultRunnerSecret,
		"SAMPLER_ACTOR_ID":        actorID,
		"SAMPLER_RUNNER_NAME":     faultRunnerName,
		"SAMPLER_POLL_MS":         fmt.Sprintf("%d", faultSamplePoll.Milliseconds()),
		"SAMPLER_IDLE_TIMEOUT_MS": "20000",
	}

	// The victim dispatches the code node, parks it, and then hangs so the
	// kill is guaranteed to land mid-operation: after the runner accepted the
	// work, before any status was ever sampled.
	flagFile := filepath.Join(t.TempDir(), "parked.flag")
	victimEnv := map[string]string{}
	for k, v := range env {
		victimEnv[k] = v
	}
	victimEnv["SAMPLER_WORKER_ID"] = "fault-runner-victim"
	victimEnv["SAMPLER_PARKED_FLAG_FILE"] = flagFile
	victimEnv["SAMPLER_HANG_AFTER_PARK"] = "1"

	victim := startSampler(t, victimEnv)
	waitForFlagFile(t, flagFile, 30*time.Second)

	// Everything the completion will need is already durable, and the victim
	// holds no lease on any of it.
	assertParkedAndUnleased(t, s, ns.ID, runID, victim)

	if err := victim.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill victim: %v", err)
	}
	_ = victim.wait(t, 10*time.Second) // "signal: killed" is expected

	if dispatches, statuses := service.counts(); dispatches != 1 || statuses != 0 {
		t.Fatalf("runner saw %d dispatches and %d status reads before the kill, want 1 and 0 "+
			"(the dispatch must not have waited for the outcome)\n--- victim ---\n%s",
			dispatches, statuses, victim.out.String())
	}

	// The survivor starts only after the kill, for the same start-order-race
	// reason TestFaultKilledWorkerReclaimedBySurvivor documents.
	survivor := startSampler(t, mergeEnv(env, map[string]string{"SAMPLER_WORKER_ID": "fault-runner-survivor"}))

	// It must be sampling the parked operation already, before there is
	// anything to learn: that is "resumes tracking after handoff".
	waitForStatusSamples(t, service, 1, 15*time.Second, survivor)

	// Now the operation finishes on the runner's side. The bound starts here,
	// because this is the first instant the outcome is knowable at all.
	answerableAt := time.Now()
	service.finish()
	recoveryBound := faultSamplePoll + 5*time.Second
	waitForRunState(t, s, runID, "completed", answerableAt.Add(recoveryBound), survivor)

	// The survivor committed it, and the operation was executed exactly once:
	// a worker crash is not an at-least-twice execution.
	if dispatches, _ := service.counts(); dispatches != 1 {
		t.Fatalf("the runner received %d dispatches, want exactly 1: a killed worker must not cause a re-execution",
			dispatches)
	}

	opState, workerID := waitForRetiredRunnerOperation(t, s, ns.ID, 15*time.Second, survivor)
	if opState != postgres.RunnerOperationCompleted {
		t.Fatalf("runner operation state = %q, want %q\n--- survivor ---\n%s",
			opState, postgres.RunnerOperationCompleted, survivor.out.String())
	}
	// The fencing tuple still names the DEAD worker, and that is correct: the
	// authority to commit came from the tuple recorded at dispatch, not from
	// whoever happened to be alive to read the status.
	if workerID != "fault-runner-victim" {
		t.Errorf("recorded worker_id = %q, want the victim that dispatched it", workerID)
	}

	var attempts int
	if err := s.Pool().QueryRow(ctx, `
		SELECT count(*) FROM attempts AS a
		JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1`, runID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want exactly 1\n--- survivor ---\n%s", attempts, survivor.out.String())
	}
}

// assertParkedAndUnleased proves the state a killed worker leaves behind is
// complete: the item is parked with no owner and no expiry, and the durable
// record carries the fencing tuple a later sample will commit under.
func assertParkedAndUnleased(t *testing.T, s *postgres.Store, namespaceID, runID string, victim *workerHandle) {
	t.Helper()
	var state string
	var owner, expires any
	if err := s.Pool().QueryRow(context.Background(), `
		SELECT wi.state, wi.lease_owner, wi.lease_expires_at
		FROM work_items AS wi JOIN node_runs AS nr ON nr.id = wi.node_run_id
		WHERE nr.run_id = $1`, runID).Scan(&state, &owner, &expires); err != nil {
		t.Fatalf("read work item: %v\n--- victim ---\n%s", err, victim.out.String())
	}
	if state != postgres.WaitingWorkState || owner != nil || expires != nil {
		t.Fatalf("work item = (%q, owner=%v, expires=%v), want parked with no lease at all",
			state, owner, expires)
	}

	var opCount, timerCount int
	if err := s.Pool().QueryRow(context.Background(), `
		SELECT count(*), count(deadline_timer_id)
		FROM runner_invocations WHERE namespace_id = $1 AND state = 'waiting_external'`,
		namespaceID).Scan(&opCount, &timerCount); err != nil {
		t.Fatalf("read runner operations: %v", err)
	}
	if opCount != 1 || timerCount != 1 {
		t.Fatalf("parked runner operations = %d (with %d deadline timers), want 1 and 1", opCount, timerCount)
	}
}

// waitForStatusSamples waits until the runner service has served at least want
// status reads.
func waitForStatusSamples(t *testing.T, service *faultRunnerService, want int, timeout time.Duration, h *workerHandle) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, statuses := service.counts(); statuses >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, statuses := service.counts()
	t.Fatalf("the surviving worker made %d status samples in %s, want at least %d: it never resumed tracking the "+
		"parked operation\n--- survivor ---\n%s", statuses, timeout, want, h.out.String())
}

// waitForRetiredRunnerOperation returns the operation row's state and recorded
// worker id once that row has left 'waiting_external', and fails if it never
// does.
//
// It waits rather than reading once because retiring the row is deliberately
// NOT part of the completion's transaction. commitRunnerTerminal
// (internal/worker/runnerasync.go) re-leases the parked item, commits the
// attempt through the engine's own §12.5 transaction, and only then retires
// the operation with a statement of its own — the actor path's
// CloseInvocation sits in exactly the same place, which is why
// internal/actors names a `close_invocation` stage that can fail AFTER
// workflow state has already moved.
//
// So there is a real window, one round trip wide, in which the run already
// reads `completed` and the operation row still reads `waiting_external`, and
// the run-status poll above returns the instant the engine's commit becomes
// visible — i.e. at the START of that window. Reading the row immediately
// afterwards lands inside it whenever the poll happens to catch the commit
// early; measured here, forcing that (polling the run status every 1ms
// instead of every 50ms) reproduces it 8 times out of 8.
//
// Waiting costs the assertion nothing. The caller still requires the state to
// be `completed` specifically, so an operation retired as `superseded` —
// committed by some other path — or never retired at all still fails, and the
// recovery BOUND is asserted where it belongs, on the run reaching
// `completed` (waitForRunState).
func waitForRetiredRunnerOperation(
	t *testing.T, s *postgres.Store, namespaceID string, timeout time.Duration, h *workerHandle,
) (state, workerID string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if err := s.Pool().QueryRow(context.Background(),
			`SELECT state, worker_id FROM runner_invocations WHERE namespace_id = $1`, namespaceID,
		).Scan(&state, &workerID); err != nil {
			t.Fatalf("read runner operation: %v", err)
		}
		if state != postgres.RunnerOperationWaiting {
			return state, workerID
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner operation is still %q after %s: the surviving worker never retired it"+
				"\n--- survivor ---\n%s", state, timeout, h.out.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitForRunState polls the run's status against a hard deadline, which is
// where the recovery bound is actually asserted.
func waitForRunState(t *testing.T, s *postgres.Store, runID, want string, deadline time.Time, h *workerHandle) {
	t.Helper()
	var got string
	for {
		if err := s.Pool().QueryRow(context.Background(),
			`SELECT status FROM runs WHERE id = $1`, runID).Scan(&got); err != nil {
			t.Fatalf("read run status: %v", err)
		}
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run status = %q, want %q within the recovery bound: the surviving worker did not commit the "+
				"parked operation's result in time\n--- survivor ---\n%s", got, want, h.out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mergeEnv(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// mustRunnerActor registers the producer identity the runner's observed
// evidence is attributed to (ledger_records.origin_actor_id is a real foreign
// key), as a real deployment must.
func mustRunnerActor(t *testing.T, s *postgres.Store, namespaceID string) string {
	t.Helper()
	actorID := "fault-runner-" + store.NewULID()
	if _, err := s.Pool().Exec(context.Background(), `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		VALUES ($1, $2, $3, 1, 'runner', 'internal')`, actorID, namespaceID, actorID); err != nil {
		t.Fatalf("mustRunnerActor: %v", err)
	}
	return actorID
}

// mustRunnerRun compiles testdata/runner.workflow.yaml and creates one run
// through the REAL engine, so the work item the victim claims is one the
// engine produced rather than one this test hand-wrote.
func mustRunnerRun(t *testing.T, s *postgres.Store, namespaceID string) string {
	t.Helper()
	ctx := context.Background()

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

	eng, err := postgres.NewEngine(s, namespaceID, engine.WithRetryDelays(0, 0))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	run, err := eng.CreateRun(ctx, cw, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run.ID
}
