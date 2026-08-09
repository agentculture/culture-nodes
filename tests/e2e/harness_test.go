package e2etest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/scheduler"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// referenceWorkflowPath is the reference workflow this whole package exists
// to prove. It is read from the repository rather than copied into testdata
// so the thing under test and the thing shipped as the example can never
// drift apart.
const referenceWorkflowPath = "../../examples/delivery-loop/workflow.yaml"

// The four agent identities the reference workflow's `uses` references name.
// The registry keys them by identity (worker.actorKeyOf strips the scheme and
// the digest), so these are the actor_key values the actors table needs.
var agentActorKeys = map[string]string{
	"intake": "company/intake",
	"plan":   "company/planner",
	"build":  "company/developer",
	"verify": "company/verifier",
}

// codeRunnerActorKey is the identity the `test` node's runner-observed
// evidence is attributed to.
const codeRunnerActorKey = "headspace/docker"

// callbackSecret is the shared HMAC secret both control-plane incarnations
// sign and verify §13.1 callback tokens with. Every dispatch carries a
// callback block whether or not the actor uses it, so a worker without a
// signer cannot dispatch to an agent at all.
const callbackSecret = "0123456789abcdef0123456789abcdef"

// -----------------------------------------------------------------------
// The scripted agents
// -----------------------------------------------------------------------

// deliveryAgents is one HTTP server standing in for the reference workflow's
// four agents. It speaks the real §13 actor protocol; only the business
// judgement inside is scripted.
//
// The script drives the changes_required loop EXACTLY once:
//
//	intake  →  completed
//	plan    →  completed
//	build   →  completed        (pass 1)
//	test    →  passed           (the runner, not this server)
//	verify  →  changes_required (pass 1)
//	build   →  completed        (pass 2, having read the verification queue)
//	test    →  passed
//	verify  →  passed           (pass 2)
//
// It lives for the whole test, across the simulated process restart, because
// an actor is an external service: restarting the control plane does not
// restart the agents it calls.
type deliveryAgents struct {
	server *httptest.Server
	// verifyRequestsChanges makes the verifier's FIRST answer
	// `changes_required`, driving the verify loop exactly once. Set false to
	// exercise a different loop (see failedtest_test.go, which drives the
	// `test.failed` edge instead and needs the verifier to pass on sight).
	verifyRequestsChanges bool

	mu       sync.Mutex
	actorIDs map[string]string // node id -> registered actors.id
	calls    map[string]int    // node id -> invocation count
	requests []actors.InvocationRequest
	failures []string
}

func newDeliveryAgents(t *testing.T, actorIDs map[string]string) *deliveryAgents {
	t.Helper()
	a := &deliveryAgents{actorIDs: actorIDs, calls: map[string]int{}, verifyRequestsChanges: true}
	a.server = httptest.NewServer(http.HandlerFunc(a.handle))
	t.Cleanup(a.server.Close)
	return a
}

func (a *deliveryAgents) handle(w http.ResponseWriter, r *http.Request) {
	var req actors.InvocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad invocation", http.StatusBadRequest)
		return
	}

	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.calls[req.Node.ID]++
	state := invocationState{
		call:        a.calls[req.Node.ID],
		verifyCalls: a.calls["verify"],
		actorID:     a.actorIDs[req.Node.ID],
	}
	a.mu.Unlock()

	result, err := a.script(req, state)
	if err != nil {
		a.mu.Lock()
		a.failures = append(a.failures, err.Error())
		a.mu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// invocationState is what the script needs to know beyond the request: which
// invocation of THIS node it is, how many times the verifier has spoken so
// far, and which registered actor identity to stamp on the records.
type invocationState struct {
	call        int
	verifyCalls int
	actorID     string
}

// script is the scripted judgement, one branch per node.
func (a *deliveryAgents) script(req actors.InvocationRequest, st invocationState) (actors.InvocationResult, error) {
	call := st.call
	propose := func(recordType ledger.RecordType, data any) ledger.Record {
		payload, _ := json.Marshal(data)
		return ledger.Record{
			RecordType: recordType,
			Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: st.actorID},
			Authority:  ledger.AuthorityProposed,
			Data:       payload,
		}
	}

	switch req.Node.ID {
	case "intake":
		return actors.InvocationResult{
			Outcome: "completed",
			Output:  json.RawMessage(`{"scope":"add a /healthz endpoint"}`),
			LedgerDelta: &actors.LedgerDelta{Records: []ledger.Record{
				propose(ledger.RecordAnnouncement, map[string]any{
					"summary": "Deliver a /healthz endpoint for the delivery-loop reference workflow.",
				}),
				propose(ledger.RecordClaim, map[string]any{
					"statement": "The service currently exposes no health endpoint.",
				}),
				propose(ledger.RecordAssumption, map[string]any{
					"statement": "The existing HTTP router can register a new route without a version bump.",
				}),
				propose(ledger.RecordQuestion, map[string]any{
					"question": "Should /healthz report dependency health as well as process liveness?",
				}),
			}},
		}, nil

	case "plan":
		return actors.InvocationResult{
			Outcome: "completed",
			Output:  json.RawMessage(`{"tasks":[{"id":"task-1","title":"add /healthz"}]}`),
			LedgerDelta: &actors.LedgerDelta{Records: []ledger.Record{
				propose(ledger.RecordTask, map[string]any{
					"title":           "add /healthz",
					"status":          "ready",
					"assurance_state": "unverified",
				}),
				propose(ledger.RecordDecision, map[string]any{
					"decision": "Implement the endpoint in the existing router rather than a sidecar.",
				}),
				propose(ledger.RecordSuccessSignal, map[string]any{
					"kind":   "process_exit",
					"equals": 0,
				}),
			}},
		}, nil

	case "build":
		// Once the verifier has spoken, a later build pass must be able to
		// SEE what it asked for: the input binding hands build the
		// verification queue, and the loop carries its state through the
		// ledger rather than through a variable. Re-entry via `test.failed`
		// happens before the verifier ever runs, so the check is conditioned
		// on the verifier having produced something to find.
		if st.verifyCalls > 0 && !bytes.Contains(req.Input, []byte("changes_required")) {
			return actors.InvocationResult{}, fmt.Errorf(
				"build pass %d ran after %d verification(s) but did not receive one in its input: %s",
				call, st.verifyCalls, req.Input)
		}
		output := fmt.Sprintf(`{"changeSet":{"pass":%d,"files":["healthz.py"]}}`, call)
		return actors.InvocationResult{
			Outcome: "completed",
			Output:  json.RawMessage(output),
			LedgerDelta: &actors.LedgerDelta{Records: []ledger.Record{
				// The completion claim. An agent saying "done" creates a
				// claim, not verified evidence (PRD §10.4) — this record must
				// stay `proposed` for the whole run.
				propose(ledger.RecordClaim, map[string]any{
					"statement": fmt.Sprintf("The /healthz endpoint is implemented (build pass %d).", call),
					"kind":      "completion_claim",
				}),
				propose(ledger.RecordResult, map[string]any{
					"summary": fmt.Sprintf("build pass %d produced a change set", call),
					"pass":    call,
				}),
			}},
		}, nil

	case "verify":
		if call == 1 && a.verifyRequestsChanges {
			return actors.InvocationResult{
				Outcome: "changes_required",
				Output:  json.RawMessage(`{"verdict":"changes_required","note":"the endpoint returns no body"}`),
				LedgerDelta: &actors.LedgerDelta{Records: []ledger.Record{
					propose(ledger.RecordResult, map[string]any{
						"summary": "verification pass 1: changes_required",
						"verdict": "changes_required",
					}),
					propose(ledger.RecordQuestion, map[string]any{
						"question": "Which body shape should /healthz return?",
					}),
				}},
			}, nil
		}
		return actors.InvocationResult{
			Outcome: "passed",
			Output:  json.RawMessage(`{"verdict":"passed"}`),
			LedgerDelta: &actors.LedgerDelta{Records: []ledger.Record{
				propose(ledger.RecordResult, map[string]any{
					"summary": "verification pass 2: passed",
					"verdict": "passed",
				}),
			}},
		}, nil
	}

	return actors.InvocationResult{}, fmt.Errorf("no script for node %q", req.Node.ID)
}

func (a *deliveryAgents) callCount(nodeID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[nodeID]
}

func (a *deliveryAgents) invocations() []actors.InvocationRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]actors.InvocationRequest(nil), a.requests...)
}

func (a *deliveryAgents) scriptFailures() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.failures...)
}

// -----------------------------------------------------------------------
// The scripted code runner
// -----------------------------------------------------------------------

// scriptedRunner is the default e2e's stand-in for the runner boundary: it
// answers exit 0 with a Result shaped exactly the way the real headspace
// bridge shapes one (exit status measured, workspace comparison honestly
// not). live_test.go swaps in the real bridge for the same workflow.
type scriptedRunner struct {
	mu  sync.Mutex
	ops []runners.Operation
}

func (s *scriptedRunner) Execute(_ context.Context, op runners.Operation) (runners.Result, error) {
	s.mu.Lock()
	s.ops = append(s.ops, op)
	s.mu.Unlock()

	zero := 0
	finished := time.Now().UTC()
	return runners.Result{
		OperationID: op.OperationID,
		State:       runners.StateCompleted,
		Exit:        &runners.Exit{Code: &zero},
		Timing: runners.Timing{
			StartedAt:  finished.Add(-40 * time.Millisecond),
			FinishedAt: finished,
			DurationMs: 40,
		},
		Environment: runners.Environment{
			RunnerRevision:    op.RunnerRevision,
			ImageDigest:       op.Execution.ImageDigest,
			PolicyDigest:      "sha256:" + strings.Repeat("d", 64),
			PlatformRequestID: "ws_scripted_" + op.OperationID,
		},
		Changes: runners.Changes{Complete: false},
		Observations: runners.Observations{
			ExitStatus:    runners.Observation{Measured: true, Complete: true, Method: "container_wait_status"},
			ChangedPaths:  runners.Observation{Measured: false, Complete: false},
			Logs:          runners.Observation{Measured: true, Complete: true, Method: "container_log_capture"},
			ResourceUsage: runners.Observation{Measured: false, Complete: false},
			Additional: map[string]runners.Observation{
				"image_digest":        {Measured: true, Complete: true, Method: "container_inspect"},
				"platform_request_id": {Measured: true, Complete: true, Method: "headspace_workspace_id"},
				"duration":            {Measured: true, Complete: true, Method: "runner_clock"},
			},
		},
	}, nil
}

func (s *scriptedRunner) operations() []runners.Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]runners.Operation(nil), s.ops...)
}

var _ runners.Runner = (*scriptedRunner)(nil)

// -----------------------------------------------------------------------
// The control-plane stack
// -----------------------------------------------------------------------

// stack is one incarnation of the control plane: its own database pool, its
// own API server, its own worker, its own scheduler. Building a second one
// after tearing the first down is the whole restart-survival test: nothing
// is carried across but PostgreSQL.
type stack struct {
	t           *testing.T
	db          *postgres.Store
	server      *httptest.Server
	worker      *worker.Worker
	scheduler   *scheduler.Scheduler
	namespaceID string

	cancel     context.CancelFunc
	stopped    chan struct{}
	workerDone chan struct{}
	stopAfter  func() bool

	mu   sync.Mutex
	errs []error
}

type stackConfig struct {
	namespaceID string
	agentsURL   string
	runner      runners.Runner
	// runnerName is the operation's dispatch name; runnerActorID is the
	// registered producer identity its evidence is attributed to. They are
	// separate for the reason worker.Options documents: a runner name is an
	// address, an actor id is who is answerable.
	runnerName    string
	runnerActorID string
	// stopAfter, when set, runs the worker as an explicit Tick loop that
	// stops as soon as the predicate holds — checked BETWEEN dispatches,
	// never during one. It is used for the pre-restart phase only, so the
	// restart point is exactly "after build's first pass" rather than
	// "somewhere around there".
	//
	// Killing a worker mid-dispatch is a different property — lease recovery
	// — already proved end to end by tests/fault's kill -9 test against two
	// real OS processes. This test measures whether a run's STATE survives a
	// clean restart, and a nondeterministic stop point would only make that
	// answer noisier.
	stopAfter func() bool
}

func startStack(t *testing.T, cfg stackConfig) *stack {
	t.Helper()

	db, err := postgres.Connect(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("connect a fresh pool: %v", err)
	}

	// Both incarnations sign and verify callback tokens with the same
	// secret, exactly as two processes of one deployment do: a token minted
	// by the first worker must still verify at the second server, which is
	// the deployment-level half of restart survival.
	signer, err := actors.NewTokenSigner([]byte(callbackSecret))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}

	srv, err := api.NewServer(db, cfg.namespaceID,
		api.WithPollInterval(50*time.Millisecond),
		api.WithCallbackSigner(signer))
	if err != nil {
		db.Close()
		t.Fatalf("api.NewServer: %v", err)
	}
	httpServer := httptest.NewServer(srv.Handler())

	eng, err := postgres.NewEngine(db, cfg.namespaceID, engine.WithRetryDelays(0, 0))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	registry, err := worker.NewDBRegistry(db, cfg.namespaceID)
	if err != nil {
		t.Fatalf("NewDBRegistry: %v", err)
	}

	s := &stack{
		t: t, db: db, server: httpServer, namespaceID: cfg.namespaceID,
		stopped: make(chan struct{}), workerDone: make(chan struct{}),
		stopAfter: cfg.stopAfter,
	}

	wk, err := worker.New(db, eng, worker.Options{
		WorkerID:    "worker-" + idstore.NewULID(),
		NamespaceID: cfg.namespaceID,
		ClaimBatch:  4,
		// A short lease so a work item stranded by a hard stop is reclaimed
		// by the next stack's scheduler within a few seconds.
		LeaseDuration:      3 * time.Second,
		HeartbeatInterval:  time.Second,
		PollInterval:       25 * time.Millisecond,
		Registry:           registry,
		Signer:             signer,
		CallbackBaseURL:    httpServer.URL,
		CodeRunner:         cfg.runner,
		CodeRunnerName:     cfg.runnerName,
		CodeRunnerActorID:  cfg.runnerActorID,
		CodeRunnerRevision: runnerRevisionOf(cfg.runner),
		OnError: func(err error) {
			s.mu.Lock()
			s.errs = append(s.errs, err)
			s.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	s.worker = wk
	s.scheduler = scheduler.New(db, scheduler.Options{TickInterval: 200 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := s.scheduler.Run(ctx); err != nil && ctx.Err() == nil {
			s.mu.Lock()
			s.errs = append(s.errs, fmt.Errorf("scheduler: %w", err))
			s.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		defer close(s.workerDone)
		if cfg.stopAfter != nil {
			s.superviseWorker(ctx)
			return
		}
		if err := wk.Run(ctx); err != nil && ctx.Err() == nil {
			s.mu.Lock()
			s.errs = append(s.errs, fmt.Errorf("worker: %w", err))
			s.mu.Unlock()
		}
	}()
	go func() {
		wg.Wait()
		close(s.stopped)
	}()

	return s
}

// superviseWorker is worker.Run's body with one addition: the stopAfter
// predicate is evaluated at the top of every pass, so the loop can only ever
// stop between dispatches. See stackConfig.stopAfter.
func (s *stack) superviseWorker(ctx context.Context) {
	for {
		if s.stopAfter() {
			return
		}
		if ctx.Err() != nil {
			return
		}
		dispatched, err := s.worker.Tick(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			s.errs = append(s.errs, err)
			s.mu.Unlock()
		}
		if dispatched > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// stop tears the whole stack down: the worker and scheduler goroutines exit,
// the HTTP server closes, and — the part that matters — the database pool is
// closed. Nothing in this process holds any run state afterwards.
func (s *stack) stop() {
	s.cancel()
	<-s.stopped
	s.server.Close()
	s.db.Close()
}

func (s *stack) errors() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.errs...)
}

// runnerRevisionOf reports a revision to pin on the operation. The scripted
// runner has none of its own; the real bridge does.
func runnerRevisionOf(r runners.Runner) string {
	type runnerRevisioner interface{ RunnerRevision() string }
	if rv, ok := r.(runnerRevisioner); ok {
		return rv.RunnerRevision()
	}
	return "sha256:" + strings.Repeat("e", 64)
}

// -----------------------------------------------------------------------
// API client helpers — the same calls the web front and the Python CLI make
// -----------------------------------------------------------------------

func (s *stack) postJSON(path string, body any, out any) int {
	s.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		s.t.Fatalf("encode request: %v", err)
	}
	resp, err := http.Post(s.server.URL+path, "application/json", bytes.NewReader(encoded))
	if err != nil {
		s.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			s.t.Fatalf("decode POST %s response: %v", path, err)
		}
	}
	return resp.StatusCode
}

func (s *stack) getJSON(path string, out any) int {
	s.t.Helper()
	resp, err := http.Get(s.server.URL + path)
	if err != nil {
		s.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			s.t.Fatalf("decode GET %s response: %v", path, err)
		}
	}
	return resp.StatusCode
}

// publishWorkflow POSTs the reference workflow's source and returns the
// pinned digest.
func (s *stack) publishWorkflow(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile(filepath.Clean(referenceWorkflowPath))
	if err != nil {
		t.Fatalf("read %s: %v", referenceWorkflowPath, err)
	}
	var published struct {
		Digest string `json:"digest"`
	}
	status := s.postJSON("/v1alpha1/workflows", map[string]string{
		"format": "yaml",
		"source": string(source),
	}, &published)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("publish workflow: status %d", status)
	}
	if published.Digest == "" {
		t.Fatal("publish workflow returned no digest")
	}
	return published.Digest
}

// createRun POSTs a run and returns its id.
func (s *stack) createRun(t *testing.T, digest string, input json.RawMessage) string {
	t.Helper()
	var created struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	status := s.postJSON("/v1alpha1/runs", map[string]any{
		"workflow_digest": digest,
		"input":           input,
	}, &created)
	if status != http.StatusCreated {
		t.Fatalf("create run: status %d", status)
	}
	return created.ID
}

// runView and its members are the Run-view payload the web front's Run page
// renders from (api/openapi/openapi.yaml's components.schemas.RunView). They
// are declared here, independently of internal/api's own types, so this test
// asserts against the documented wire shape rather than against whatever
// struct that package happens to serialise.
type runView struct {
	Run      runSummaryView `json:"run"`
	Tokens   []tokenView    `json:"tokens"`
	NodeRuns []nodeRunView  `json:"node_runs"`
}

type runSummaryView struct {
	ID             string          `json:"id"`
	WorkflowDigest string          `json:"workflow_digest"`
	State          string          `json:"state"`
	Input          json.RawMessage `json:"input"`
	Output         json.RawMessage `json:"output"`
}

type tokenView struct {
	ID     string `json:"id"`
	NodeID string `json:"node_id"`
	State  string `json:"state"`
}

type nodeRunView struct {
	ID         string        `json:"id"`
	NodeID     string        `json:"node_id"`
	State      string        `json:"state"`
	Outcome    string        `json:"outcome"`
	VisitCount int           `json:"visit_count"`
	Attempts   []attemptView `json:"attempts"`
}

type attemptView struct {
	ID            string          `json:"id"`
	AttemptNumber int             `json:"attempt_number"`
	Status        string          `json:"status"`
	FencingToken  int64           `json:"fencing_token"`
	Result        json.RawMessage `json:"result"`
}

func (s *stack) runView(t *testing.T, runID string) runView {
	t.Helper()
	var view runView
	if status := s.getJSON("/v1alpha1/runs/"+runID, &view); status != http.StatusOK {
		t.Fatalf("GET run: status %d", status)
	}
	return view
}

// waitForRunState polls the API until the run reaches a terminal state.
func (s *stack) waitForTerminal(t *testing.T, runID string, timeout time.Duration) runView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var view runView
	for time.Now().Before(deadline) {
		view = s.runView(t, runID)
		if engine.RunState(view.Run.State).Terminal() {
			return view
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach a terminal state within %s (state %s, worker errors %v)",
		runID, timeout, view.Run.State, s.errors())
	return view
}

// -----------------------------------------------------------------------
// SSE
// -----------------------------------------------------------------------

// sseEvent is one server-sent event as the web front's EventSource sees it.
type sseEvent struct {
	Sequence int64
	Type     string
	Data     json.RawMessage
}

// streamRunEvents consumes the SSE stream from the beginning of the run's
// event log until the terminal event closes it. Resuming from sequence 0 is
// what a browser does on its first connection, and it is also how a client
// that missed a whole process incarnation catches up: the stream is served
// from committed rows, not from anything the server held in memory.
func streamRunEvents(t *testing.T, baseURL, runID string, timeout time.Duration) ([]sseEvent, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/v1alpha1/runs/"+runID+"/events?from=0", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("event stream returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		return nil, fmt.Errorf("event stream content type = %q", ct)
	}

	var (
		out     []sseEvent
		current sseEvent
		scanner = bufio.NewScanner(resp.Body)
	)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if current.Type != "" {
				out = append(out, current)
			}
			current = sseEvent{}
		case strings.HasPrefix(line, "id: "):
			n, convErr := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			if convErr == nil {
				current.Sequence = n
			}
		case strings.HasPrefix(line, "event: "):
			current.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			current.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		}
	}
	return out, scanner.Err()
}

// -----------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------

// registerActors inserts the actors rows the reference workflow's `uses`
// references resolve against, plus the code runner's own identity. Both are
// real obligations: worker.DBRegistry reads endpoint_ref to know where to
// POST, and ledger_records.origin_actor_id is a foreign key, so a producer
// that is not a registered actor cannot write to the ledger at all.
func registerActors(t *testing.T, db *postgres.Store, namespaceID, agentsURL string) (agents map[string]string, runnerID string) {
	t.Helper()
	agents = map[string]string{}
	for nodeID, key := range agentActorKeys {
		id := "actor_" + idstore.NewULID()
		if _, err := db.Pool().Exec(context.Background(), `
			INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref)
			VALUES ($1, $2, $3, 1, 'agent', 'http', $4)
		`, id, namespaceID, key, agentsURL); err != nil {
			t.Fatalf("register actor %s: %v", key, err)
		}
		agents[nodeID] = id
	}

	runnerID = "actor_" + idstore.NewULID()
	if _, err := db.Pool().Exec(context.Background(), `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		VALUES ($1, $2, $3, 1, 'runner', 'internal')
	`, runnerID, namespaceID, codeRunnerActorKey); err != nil {
		t.Fatalf("register code runner actor: %v", err)
	}
	return agents, runnerID
}

// ledgerFor opens a ledger runtime on a store for reading assertions.
func ledgerFor(t *testing.T, db *postgres.Store, namespaceID string) *ledger.Ledger {
	t.Helper()
	l, err := postgres.NewLedger(db, namespaceID)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	return l
}
