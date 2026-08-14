package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/telemetry"
)

// Defaults. Each is a named constant because a deployment tuning the worker
// should be changing something with a name.
const (
	// DefaultClaimBatch is how many work items one tick claims. It is small
	// on purpose: a batch is processed before the next claim, so a large
	// batch behind one slow synchronous actor is latency nobody asked for.
	DefaultClaimBatch = 4
	// DefaultLeaseDuration is how long a claim is held before it can be
	// reclaimed. It only has to outlive one heartbeat interval, because a
	// live worker keeps extending it.
	DefaultLeaseDuration = 60 * time.Second
	// DefaultHeartbeatInterval is how often a synchronous dispatch extends
	// its lease. A third of the lease leaves room for two missed beats.
	DefaultHeartbeatInterval = 20 * time.Second
	// DefaultPollInterval is how long an idle worker waits before claiming
	// again. PostgreSQL is authoritative and the queue is a disposable signal
	// (§12.3), so polling is the correctness-bearing path and a signal, when
	// there is one, is only an optimisation.
	DefaultPollInterval = time.Second
	// DefaultNodeTimeout bounds a dispatch whose node declares no timeout.
	DefaultNodeTimeout = 10 * time.Minute
	// MaxDispatchAttempts is how many times one work item may be dispatched
	// to an actor before the worker stops trying. See budget.go.
	MaxDispatchAttempts = 3
	// attemptIDPrefix echoes §13.1's "att_01J…" shape.
	attemptIDPrefix = "att_"
)

// Options configures a Worker. Every field has a documented default except
// the ones a worker cannot invent for itself.
type Options struct {
	// WorkerID identifies this worker in work_items.lease_owner and in every
	// completion's fencing tuple. Defaults to a fresh ULID prefixed
	// "worker-".
	WorkerID string
	// NamespaceID scopes the callback store and the actor registry.
	// Required.
	NamespaceID string

	// ClaimBatch, LeaseDuration, HeartbeatInterval, PollInterval pace the
	// loop; see the Default* constants.
	ClaimBatch        int
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	PollInterval      time.Duration
	// DefaultTimeout bounds a dispatch for a node with no declared timeout.
	DefaultTimeout time.Duration

	// Registry resolves a node's `uses` reference to an endpoint. Required
	// for agent and action.http nodes.
	Registry Registry
	// Client speaks §13 to actors. Defaults to actors.NewClient().
	Client *actors.Client
	// Signer mints the attempt-scoped callback tokens §13.1 carries.
	// Required for asynchronous actors; a worker without one can still
	// dispatch, but any actor that answers 202 will be told it cannot be
	// accepted, because a callback nobody can authenticate is not a callback.
	Signer *actors.TokenSigner
	// CallbackBaseURL is the control plane's externally reachable base URL,
	// from which §13.1's callback.url is built. Required alongside Signer.
	CallbackBaseURL string

	// Runner, Human, and Waiter are the seams for code, approval, and wait
	// nodes. A nil Runner or Human makes its kind a diagnosed failure rather
	// than a silent success (see seams.go). Waiter is different: a nil value
	// gets the production timer-backed dispatcher (wait.go's
	// TimerWaitDispatcher) wired in by New, because durable waits need
	// nothing deployment-specific — only the store and the clock the worker
	// already holds; set it only to substitute a custom implementation.
	// Human is expected to stay nil in every real deployment: an approval
	// node never produces a work item for this worker to dispatch in the
	// first place (task t6's engine-side park, internal/engine/humantask.go),
	// so there is nothing legitimate for a HumanDispatcher to do — see
	// HumanDispatcher's doc comment in seams.go.
	Runner RunnerDispatcher
	Human  HumanDispatcher
	Waiter WaitDispatcher

	// CodeRunner is the internal/runners seam a `code` node's own operation
	// executes through (see code.go). It is the low-level runners.Runner
	// interface — the same one HookRunner takes — so the headspace bridge and
	// the Lambda adapter plug in unchanged.
	//
	// It is a second, lower-level door to the same kind: when Runner
	// (RunnerDispatcher) is also configured, Runner wins, because a
	// deployment that registered the higher-level seam has already said how
	// it wants code dispatched and this option must not silently override
	// that. With neither configured a code node is a diagnosed failure, as it
	// has always been.
	CodeRunner runners.Runner
	// CodeRunnerName is the logical runner name stamped on every code
	// operation's `runner` field — "headspace", "lambda". An adapter checks
	// it and refuses an operation addressed to a different runner, so it must
	// be the name the configured CodeRunner registers under. Required
	// alongside CodeRunner.
	CodeRunnerName string
	// CodeRunnerActorID is the registered actor identity the runner's
	// observed evidence is attributed to, and it is deliberately NOT the same
	// field as CodeRunnerName: a runner name is a dispatch address ("which
	// adapter executes this"), an actor id is a producer identity ("who is
	// answerable for this observation", PRD §9.5's "identity is not
	// execution"). A deployment running two headspace workers under separate
	// identities has one name and two actor ids. Empty falls back to
	// CodeRunnerName, which is right for a single-runner deployment that
	// registered its actor under that name.
	CodeRunnerActorID string
	// CodeRunnerRevision pins the runner revision stamped on the operation
	// and recorded on the evidence record's origin. An adapter that pins its
	// own contract revision (internal/runners/headspace) refuses an operation
	// whose revision does not match, so this is not decorative.
	CodeRunnerRevision string
	// CodeOutcomes maps a code node's declared outcomes onto its exit-status
	// ports. Nil uses ConventionalCodeOutcomes; see code.go for why this is a
	// convention with an override rather than a schema field.
	CodeOutcomes CodeOutcomeResolver

	// RunnerService configures dispatch to registered runner SERVICES over
	// api/runner-protocol (see runnerasync.go). It is the placement-aware
	// half of code-node dispatch: the same `code` node runs through
	// CodeRunner when its identity is in-process and over the protocol when
	// its identity is a runner service, and the workflow definition says
	// neither.
	//
	// The zero value disables the protocol path, which is why adding it
	// changes nothing for a deployment that has not configured it.
	RunnerService RunnerServiceOptions

	// HookRunner is the internal/runners seam pre_run/post_run code hooks on
	// an agent node execute through (task t14, spec claim c37). It is
	// deliberately the low-level runners.Runner interface, not the
	// higher-level RunnerDispatcher above: a hook is not a `code` node, and
	// nothing about the seam that will one day dispatch a whole `code` node
	// needs to be true of the much smaller thing a hook does — run one typed
	// operation, report one Result. A node that declares a hook when no
	// HookRunner is configured fails the attempt with a "configuration"
	// diagnostic, exactly like an agent node with no Registry.
	HookRunner runners.Runner
	// HookRunnerName is the logical runner name stamped on every hook
	// operation's `runner` field (e.g. "lambda") and, doubling as the
	// producer identity, on the observed evidence a hook's execution
	// appends to the ledger. Required alongside HookRunner.
	HookRunnerName string
	// HookRunnerRevision pins the runner revision stamped on every hook
	// operation. Optional: a deployment that has not adopted revision
	// pinning for its hook runner leaves this empty.
	HookRunnerRevision string

	// Now and NewID are the clock and the identifier factory.
	Now   func() time.Time
	NewID func() string
	// OnError observes a per-item dispatch failure the loop swallowed. It is
	// observability only: a failed dispatch never stops the loop, because the
	// work item's lease will expire and another worker will retry it.
	OnError func(error)

	// Pacing declares the dispatch rates this worker holds itself to (task
	// t10): a global session rate and per-actor rates, enforced at the
	// dispatch site against durable state every worker shares. The zero
	// value declares nothing, which is what every deployment that has not
	// configured pacing gets — no rate rows, no transaction, and the
	// dispatch path exactly as it was. See pacing.go.
	Pacing PacingOptions

	// Telemetry instruments the actor dispatch seam (task t19,
	// internal/worker/dispatch.go's dispatchActor) through
	// internal/telemetry. The zero value, a nil *telemetry.Provider, is a
	// safe no-op — every telemetry.Provider method tolerates a nil
	// receiver — so a Worker built without setting this field (every
	// existing caller, every existing test) behaves exactly as it did
	// before this field existed.
	Telemetry *telemetry.Provider
}

// Worker claims ready work and dispatches it. It is safe for concurrent use;
// a process may run several, and several processes may run one each.
type Worker struct {
	db        *postgres.Store
	engine    *engine.Engine
	callbacks *postgres.CallbackStore
	ledger    *ledger.Ledger
	opts      Options
	specs     *specCache
	decisions *decisionCache
}

// New returns a worker over db and eng.
func New(db *postgres.Store, eng *engine.Engine, opts Options) (*Worker, error) {
	switch {
	case db == nil:
		return nil, errors.New("worker: New requires a store")
	case eng == nil:
		return nil, errors.New("worker: New requires an engine")
	case opts.NamespaceID == "":
		return nil, errors.New("worker: New requires a namespace id")
	}

	if opts.WorkerID == "" {
		opts.WorkerID = "worker-" + idstore.NewULID()
	}
	if opts.ClaimBatch <= 0 {
		opts.ClaimBatch = DefaultClaimBatch
	}
	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = DefaultLeaseDuration
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.DefaultTimeout <= 0 {
		opts.DefaultTimeout = DefaultNodeTimeout
	}
	if opts.Client == nil {
		opts.Client = actors.NewClient()
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.NewID == nil {
		opts.NewID = idstore.NewULID
	}
	if opts.Waiter == nil {
		opts.Waiter = NewTimerWaitDispatcher(db, opts.Now)
	}

	callbacks, err := postgres.NewCallbackStore(db, opts.NamespaceID)
	if err != nil {
		return nil, err
	}
	runtime, err := postgres.NewLedger(db, opts.NamespaceID)
	if err != nil {
		return nil, fmt.Errorf("worker: build ledger runtime: %w", err)
	}
	decisions, err := newDecisionCache()
	if err != nil {
		return nil, err
	}

	return &Worker{
		db:        db,
		engine:    eng,
		callbacks: callbacks,
		ledger:    runtime,
		opts:      opts,
		specs:     newSpecCache(),
		decisions: decisions,
	}, nil
}

// ID reports the identifier this worker leases work under.
func (w *Worker) ID() string { return w.opts.WorkerID }

// Run claims and dispatches work until ctx is done.
//
// It returns ctx.Err() on shutdown and never returns for any other reason: a
// worker that stopped because one dispatch failed would take a whole
// deployment's capacity down over one bad node. Per-item failures are
// reported through Options.OnError and left to the lease to recover, which is
// the §20.4 recovery path for a worker that dies mid-dispatch anyway.
func (w *Worker) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		dispatched, err := w.Tick(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			w.report(err)
		}
		// A tick that found work claims again immediately: a backlog should
		// drain at the speed of dispatch, not at the speed of the poll.
		if dispatched > 0 {
			continue
		}
		if !sleepCtx(ctx, w.opts.PollInterval) {
			return ctx.Err()
		}
	}
}

// Tick claims one batch, dispatches it, and samples any parked runner
// operations that have come due. It is exported so a test can drive exactly
// one pass, and so an operator tool can do a single unit of work without
// starting a loop.
//
// The returned count is how many work ITEMS were dispatched, deliberately not
// counting sampled operations: Run uses it to decide whether to claim again
// immediately, and a sampler that found something is not a backlog to drain.
// A sampling failure is reported rather than returned for the same reason a
// per-item dispatch failure is — every parked operation has a deadline timer
// behind it, so a pass that could not sample is a delay, not a loss.
func (w *Worker) Tick(ctx context.Context) (int, error) {
	claimed, err := w.db.ClaimWork(ctx, w.opts.NamespaceID, w.opts.WorkerID, w.opts.LeaseDuration, w.opts.ClaimBatch)
	if err != nil {
		return 0, fmt.Errorf("worker: claim: %w", err)
	}
	for i := range claimed {
		if err := w.dispatch(ctx, claimed[i]); err != nil {
			w.report(fmt.Errorf("worker: dispatch work %s (node run %s): %w",
				claimed[i].ID, claimed[i].NodeRunID, err))
		}
	}
	if _, err := w.SampleRunnerOperations(ctx); err != nil {
		w.report(err)
	}
	return len(claimed), nil
}

func (w *Worker) report(err error) {
	if err != nil && w.opts.OnError != nil {
		w.opts.OnError(err)
	}
}

// dispatch executes one claimed work item.
//
// Every path through it ends in exactly one of three things: a committed
// completion, a parked (waiting_external) work item, or a returned error that
// leaves the lease to expire. There is no fourth outcome where the item is
// silently dropped while still leased — that would strand the run until the
// lease expired with nothing recorded about why.
func (w *Worker) dispatch(ctx context.Context, claimed postgres.ClaimedWork) error {
	d, err := w.db.LoadDispatch(ctx, claimed.NodeRunID)
	if err != nil {
		return err
	}
	spec, err := w.specs.get(d.WorkflowDigest, d.NormalizedIR)
	if err != nil {
		return err
	}
	node, ok := spec.Nodes[d.NodeID]
	if !ok {
		return w.failAttempt(ctx, claimed, "", engine.StatusFailed, "definition",
			fmt.Sprintf("node run %s names node %q, which the pinned definition %s does not declare",
				d.NodeRunID, d.NodeID, d.WorkflowDigest))
	}

	input, err := resolveNodeInput(ctx, w.sources(d), node.Input)
	if err != nil {
		// An unresolvable binding is a real refusal to dispatch, not a
		// transport hiccup: the actor would be handed data the definition did
		// not ask for. It is recorded as contract_rejected because a declared
		// contract — the input binding — was not satisfiable.
		return w.failAttempt(ctx, claimed, "", engine.StatusContractRejected, string(actors.ClassContract),
			fmt.Sprintf("node %q input binding did not resolve: %v", node.ID, err))
	}

	dc := DispatchContext{
		RunID:     d.RunID,
		NodeRunID: d.NodeRunID,
		TokenID:   d.TokenID,
		NodeID:    d.NodeID,
		AttemptID: attemptIDPrefix + w.opts.NewID(),
		Attempt:   int(claimed.Attempt),
		Input:     input,
		Deadline:  w.deadlineFor(node),
	}

	switch node.Kind {
	case kindAgent, kindActionHTTP:
		return w.dispatchActor(ctx, claimed, d, spec, node, dc)
	case kindDecision:
		return w.dispatchDecision(ctx, claimed, d, spec, node, dc)
	case kindCode:
		// dispatchCode owns both code paths — the in-process CodeRunner and
		// the asynchronous runner protocol — because which one a node takes is
		// a REGISTRY fact it resolves, not a routing decision the dispatcher
		// can make from the node alone. Options.Runner (the higher-level seam)
		// still wins when a deployment registered one: it has already said how
		// it wants code dispatched.
		if w.opts.Runner == nil && (w.opts.CodeRunner != nil || w.runnerServiceConfigured()) {
			return w.dispatchCode(ctx, claimed, d, node, dc)
		}
		return w.dispatchSeam(ctx, claimed, d, node, dc, "code", "runner", func() (SeamResult, error) {
			if w.opts.Runner == nil {
				return SeamResult{}, errNoSeam
			}
			return w.opts.Runner.DispatchCode(ctx, dc, node.Uses, node.Operation)
		})
	case kindApproval:
		return w.dispatchSeam(ctx, claimed, d, node, dc, "approval", "human-task", func() (SeamResult, error) {
			if w.opts.Human == nil {
				return SeamResult{}, errNoSeam
			}
			return w.opts.Human.DispatchApproval(ctx, dc, node.ApproverRef, node.Deadline)
		})
	case kindWait:
		return w.dispatchSeam(ctx, claimed, d, node, dc, "wait", "timer", func() (SeamResult, error) {
			if w.opts.Waiter == nil {
				return SeamResult{}, errNoSeam
			}
			return w.opts.Waiter.DispatchWait(ctx, dc, node.Until)
		})
	case kindParallel:
		return w.dispatchParallel(ctx, claimed)
	case kindJoin:
		return w.dispatchJoin(ctx, claimed, node, dc)
	case kindEnd:
		// An end node produces the workflow result inside the engine's own
		// transition transaction and is never enqueued. Reaching one here
		// means something enqueued work that should not exist, so it is a
		// definition-level failure rather than something to paper over.
		return w.failAttempt(ctx, claimed, "", engine.StatusFailed, "definition",
			fmt.Sprintf("node %q is an end node; end nodes are completed by the engine and never dispatched", node.ID))
	default:
		// An unknown kind stays a loud definition failure, which is a
		// DECISION against the parallel-tokens design's open item O8 rather
		// than an oversight. O8 proposed release-and-retry with backoff so an
		// N-1 worker could not kill a run pinned to a workflow using kinds it
		// has never heard of. Two things sank it:
		//
		//   * it cannot fix the hazard it names. An N-1 worker's behaviour is
		//     fixed in the N-1 binary; only a worker released BEFORE the new
		//     kind could benefit, and the same argument recurs at every
		//     subsequent kind. The mitigation that does work is operational
		//     and already available — do not publish workflows using a new
		//     kind until the rollout completes. Migration 0019 is staged for
		//     exactly that: expand-only, so an N-1 binary that never reads
		//     token_groups or join_arrivals keeps working.
		//   * it trades a loud failure for a silent hang. A genuinely bogus
		//     kind (a hand-built IR, a typo in a pinned definition) would
		//     recycle through the queue forever with nothing terminal to look
		//     at, which is the opposite of what every other refusal in this
		//     dispatcher does.
		return w.failAttempt(ctx, claimed, "", engine.StatusFailed, "definition",
			fmt.Sprintf("node %q declares kind %q, which this worker cannot dispatch", node.ID, node.Kind))
	}
}

// Node kinds, mirroring internal/compiler's vocabulary. They are declared
// again here rather than imported because the compiler's constants are part
// of its authoring surface, and the worker's dependency should be on the IR's
// values, not on that package.
const (
	kindAgent      = "agent"
	kindCode       = "code"
	kindActionHTTP = "action.http"
	kindDecision   = "decision"
	kindApproval   = "approval"
	kindWait       = "wait"
	kindParallel   = "parallel"
	kindJoin       = "join"
	kindEnd        = "end"
)

// errNoSeam marks a kind whose dispatcher is not registered.
var errNoSeam = errors.New("no dispatcher registered")

func (w *Worker) sources(d postgres.Dispatch) bindingSources {
	return bindingSources{
		RunID:    d.RunID,
		RunInput: d.RunInput,
		NodeOutput: func(ctx context.Context, nodeID string) (json.RawMessage, error) {
			return w.db.NodeOutput(ctx, d.RunID, nodeID)
		},
		NodeEvidence: func(ctx context.Context, nodeID string) ([]ledger.Record, error) {
			return w.db.NodeEvidence(ctx, d.RunID, nodeID)
		},
		Projection: func(ctx context.Context, kind ledger.ProjectionKind, subject string) (ledger.Projection, error) {
			return w.ledger.ProjectRun(ctx, d.RunID, kind, subject)
		},
	}
}

func (w *Worker) deadlineFor(node *nodeSpec) time.Time {
	timeout := node.Timeout
	if timeout <= 0 {
		timeout = w.opts.DefaultTimeout
	}
	if timeout <= 0 {
		return time.Time{}
	}
	return w.opts.Now().Add(timeout)
}

// complete fills the fencing tuple from the claim and commits through the
// engine. The tuple is taken from the claim rather than from anything the
// caller passes, so no dispatch path can accidentally complete under a
// different attempt than the one it was leased for.
func (w *Worker) complete(ctx context.Context, claimed postgres.ClaimedWork, req engine.CompletionRequest) (engine.CompletionResult, error) {
	req.WorkID = claimed.ID
	req.WorkerID = w.opts.WorkerID
	req.FencingToken = claimed.FencingToken
	req.Attempt = int(claimed.Attempt)
	result, err := w.engine.CompleteAttempt(ctx, req)
	if err != nil {
		return result, err
	}
	// A completion that reaped sibling branches (issue #43: the losers of an
	// any/quorum barrier, or every live branch when this completion ended the
	// run) has already retired them transactionally. Telling their actors to
	// stop is the best-effort, post-commit half — see branchcancel.go.
	w.propagateBranchCancellations(ctx, result)
	return result, nil
}

// failAttempt records a technical failure with a machine-readable diagnostic.
//
// engine.CompletionRequest has no diagnostic field, and the engine stores
// Output on the attempt row whatever the status is, so the diagnostic goes in
// the output. That is deliberate: an attempt that failed with no recorded
// reason is the single most expensive thing to debug in a durable system, and
// the attempts table is where an operator is already looking.
//
// actorID is the durable attribution the failed attempt is recorded under
// (attempts.actor_id): the resolved actor row id (dc.ActorRowID) for a
// failure after Registry.Resolve, the code-runner actor id for a code-path
// failure, and "" — persisted as NULL — for every site that fires before an
// actor was resolved. A failed dispatch is still that actor's dispatch, and
// per-actor surfaces (retry burn in particular) must not lose it; a
// pre-resolution refusal, conversely, must never guess one.
func (w *Worker) failAttempt(ctx context.Context, claimed postgres.ClaimedWork, actorID string, status engine.TechStatus, class, detail string) error {
	_, err := w.completeTechnicalFailure(ctx, claimed, actorID, status, class, detail, nil, actorTelemetry{})
	return err
}

// actorTelemetry is what an actor's own terminal report contributes to an
// attempt row beyond its outcome: the §13.2 usage block and the provider's
// termination reason. They travel as one parameter because they come from
// one report, and stay separate fields because either can arrive without
// the other — a cancelled or output-capped turn can name its reason while
// holding no parseable usage at all, which is why the reason is not a field
// of the usage block (docs/adr/0009-usage-telemetry-extension.md).
// ContinuationRef is the third such fact (ADR 0010): the handle the actor
// offered for continuing its conversation. It belongs here rather than
// beside the outcome because a completion that records no outcome at all can
// still have been produced by a session that exists and is resumable.
type actorTelemetry struct {
	Usage             *engine.Usage
	TerminationReason *string
	ContinuationRef   *string
	// Preserve is task t25/t26's bridge-reported preserve-on-failure branch
	// (issue #49), nil when the actor reported none — the far more common
	// case, since a bridge only preserves on a genuine technical failure
	// that left workspace changes behind.
	Preserve *engine.Preserve
}

// completeTechnicalFailure is failAttempt's twin for a caller that needs the
// committed CompletionResult afterward (task t14's hook bookkeeping: a
// runner_operations row and a hook's own ledger evidence are both keyed to
// the attempt id this call commits) and that may need to carry a ledger
// delta failAttempt never takes — e.g. the agent's own proposed records when
// a post-run hook's verdict could not be trusted, so a technical failure
// still records what the agent itself claimed.
//
// telemetry is what the actor's own terminal report contributes to the
// failed attempt's row, zero when it reported nothing (issue #32: a failed
// session still burned real tokens, and a technical failure must not cost
// the attempt its accounting). Absent fields persist as NULL — unreported,
// never fabricated zeros.
//
// The returned CompletionResult is the zero value when the completion turned
// out stale (isStale(err)): nothing was committed here, so there is nothing
// for a caller to key follow-up writes to, and the error is nil — a stale
// completion is not a worker malfunction (see isStale).
func (w *Worker) completeTechnicalFailure(
	ctx context.Context, claimed postgres.ClaimedWork, actorID string, status engine.TechStatus, class, detail string, delta []ledger.Record, telemetry actorTelemetry,
) (engine.CompletionResult, error) {
	result, err := w.complete(ctx, claimed, engine.CompletionRequest{
		TechStatus:        status,
		Output:            diagnosticOutput(class, detail),
		LedgerDelta:       delta,
		Usage:             telemetry.Usage,
		TerminationReason: telemetry.TerminationReason,
		ContinuationRef:   telemetry.ContinuationRef,
		Preserve:          telemetry.Preserve,
		ActorID:           actorID,
	})
	if err != nil {
		if isStale(err) {
			return engine.CompletionResult{}, nil
		}
		return engine.CompletionResult{}, err
	}
	return result, nil
}

// diagnosticOutput is the fixed shape a failed attempt's output takes.
func diagnosticOutput(class, detail string) json.RawMessage {
	payload := struct {
		Error struct {
			Class  string `json:"class"`
			Detail string `json:"detail"`
		} `json:"error"`
	}{}
	payload.Error.Class = class
	payload.Error.Detail = detail
	encoded, err := json.Marshal(payload)
	if err != nil {
		return json.RawMessage(`{"error":{"class":"execution"}}`)
	}
	return encoded
}

// isStale reports whether an error means this worker no longer holds the
// claim it was completing under.
//
// A stale completion is not a worker malfunction — §12.4 designs for it — so
// the loop treats it as a handled outcome. Nothing was written (the whole
// §12.5 transaction rolled back), and whoever holds the claim now is
// responsible for the item.
func isStale(err error) bool {
	return errors.Is(err, engine.ErrStaleClaim) ||
		errors.Is(err, engine.ErrTerminalNodeRun) ||
		errors.Is(err, engine.ErrTerminalRun)
}

// sleepCtx sleeps for d or until ctx is done, reporting whether d elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// actorRowID best-effort resolves a node's actor reference to its
// actors-table row id for attempt attribution. Any miss — a registry
// without the capability, an unknown ref, a query error — yields "":
// attribution is worth having, never worth failing a dispatch over.
func (w *Worker) actorRowID(ctx context.Context, ref string) string {
	r, ok := w.opts.Registry.(actorRowIDResolver)
	if !ok {
		return ""
	}
	id, err := r.ActorRowID(ctx, ref)
	if err != nil {
		return ""
	}
	return id
}
