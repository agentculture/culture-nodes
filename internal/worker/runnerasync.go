package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Asynchronous runner dispatch: park, sample, commit (task t9; PRD §12.6,
// §13.7, api/runner-protocol).
//
// This is the fourth dispatch path in this package, and it is the one that
// makes a `code` node cost the same to wait on as an `agent` node does.
// code.go's CodeRunner.Execute holds the lease — and heartbeats it — for the
// whole duration of the operation, which is right for an in-process adapter
// whose work happens inside this process anyway. It is wrong for a runner
// SERVICE: the operation runs somewhere else, possibly for ten minutes, and a
// worker that held a lease and a goroutine for it would make the runtime's cost
// scale with how long work takes rather than with how many runners exist.
//
// So the shape is the asynchronous actor's shape (dispatch.go's park,
// internal/actors' commitTerminal), with one difference that shows up
// everywhere: an actor REPORTS back, a runner does not. Completion is learned
// only by an authenticated status read this runtime initiates. That turns the
// resume trigger from "an inbound event arrives" into "something has to ask",
// and the sampler below is that something.
//
// The life cycle, end to end:
//
//	dispatchRunnerService  POST the operation, get a 202, park the work item
//	                       (StartRunnerWait) — lease released, no goroutine,
//	                       nothing held anywhere
//	SampleRunnerOperations claim the due operations from the table, read each
//	                       one's status once, commit the terminal ones
//	HandleRunnerCallback   an optional notification: bring the next sample
//	                       forward, and do nothing else, ever
//
// Between two samples there is no goroutine, no open connection, no
// transaction and no lease anywhere in this process that relates to the
// operation. Everything that will be needed to commit its result lives in the
// runner_invocations row — which is what lets a worker be killed mid-operation
// without stranding anything: the operation keeps running in its own process,
// and whichever worker is alive at the next sampling round picks it up.

// Runner-service dispatch defaults. Each is named because a deployment tuning
// sampling behaviour should be changing something with a name.
const (
	// DefaultRunnerSampleInterval is how often a parked operation is sampled
	// when its runner declared no preference of its own. Sampling load scales
	// with runners × interval, never with operation duration, so this is a
	// latency knob rather than a capacity one.
	DefaultRunnerSampleInterval = runners.DefaultPollInterval
	// DefaultRunnerSampleBatch bounds how many operations one sampler pass
	// handles. A pass samples serially, so the batch is what stops one pass
	// from taking longer than the interval it runs on.
	DefaultRunnerSampleBatch = 16
	// runnerResumeLease is how long the work item is re-leased while a
	// terminal result commits. It only has to outlive one CompleteAttempt
	// transaction.
	runnerResumeLease = actors.DefaultResumeLease
	// runnerCallbackTokenGrace lets a callback arriving right at the
	// attempt's deadline still be accepted. A callback commits nothing, so
	// this grace can never extend what a runner is allowed to change.
	runnerCallbackTokenGrace = callbackTokenGrace
)

// Diagnostic event types this path appends to a run's audit log. They mirror
// internal/actors' callback diagnostics, because the situations are the same
// ones — "nothing happened" is only trustworthy if it is written down.
const (
	// TypeRunnerSampleFailed records a status sample that learned nothing: a
	// forgotten operation, a throttle, a transport failure. It is a
	// diagnostic, never evidence.
	TypeRunnerSampleFailed = "dev.culture.nodes.runner.sample-failed"
	// TypeRunnerCompletionLate records a terminal status that arrived after
	// its attempt was replaced, cancelled, or already completed — the
	// runner-side twin of §13.4's late diagnostic, and the only trace a
	// losing duplicate leaves.
	TypeRunnerCompletionLate = "dev.culture.nodes.runner.completion-late"
)

// runnerOperationKind is the runner_operations.operation_kind value an async
// service dispatch's evidence row is recorded under. It is deliberately the
// same value code.go uses for the in-process path: the row means the same
// thing either way — this typed operation was sent to a runner and this is
// what came back — and a reader should not have to know which transport
// carried it.
const runnerOperationKind = codeOperationKind

// RunnerHooks injects test-only interleaving points that would otherwise
// require killing a real process at an exact instant. The zero value injects
// nothing; production callers never set these fields.
//
// It mirrors internal/scheduler.Hooks, which exists for the same reason.
type RunnerHooks struct {
	// BeforeCommit, if set, is called once a sample has read a TERMINAL
	// status and before that result is committed — the exact instant at which
	// a second sampler's commit must be able to win the race. See
	// runnerasync_test.go's racing-completion test.
	BeforeCommit func(attemptID string)
}

// RunnerServiceOptions configures dispatch to registered runner services over
// api/runner-protocol.
//
// It is one nested struct rather than six fields on Options because it is one
// decision: either this deployment dispatches code to runner services, in
// which case it needs all of this, or it does not, in which case Options stays
// as it was.
type RunnerServiceOptions struct {
	// Registry is the allowlist of execution identities, and the routing
	// decision: a code node whose name resolves to a ServiceIdentity takes
	// this asynchronous path, and one that resolves to a FunctionIdentity (or
	// to nothing) takes code.go's in-process CodeRunner path. Nil disables
	// the protocol path entirely.
	Registry *runners.FunctionRegistry
	// Client speaks the protocol. Required alongside Registry — a registry
	// with no client resolves identities it cannot dispatch to.
	Client *runners.ProtocolClient
	// PollInterval is the sampling cadence for an operation whose runner
	// asked for nothing. A runner's own poll_after_seconds wins when it
	// declares one: sampling faster than a runner asked for is load it said
	// it did not want. Defaults to DefaultRunnerSampleInterval.
	PollInterval time.Duration
	// SampleBatch bounds one sampler pass. Defaults to
	// DefaultRunnerSampleBatch.
	SampleBatch int
	// DisableCallback suppresses the optional completion callback, which is
	// otherwise offered whenever the worker has both a Signer and a
	// CallbackBaseURL.
	//
	// Suppressing it costs latency and nothing else, which is the point of the
	// callback being a hint rather than a delivery: a deployment with no
	// externally reachable URL — or one that would simply rather not publish
	// an ingress — is fully correct on polling alone.
	DisableCallback bool
	// Hooks injects test-only interleaving points (see RunnerHooks).
	Hooks RunnerHooks
}

func (o RunnerServiceOptions) configured() bool { return o.Registry != nil && o.Client != nil }

func (o RunnerServiceOptions) pollInterval() time.Duration {
	if o.PollInterval > 0 {
		return o.PollInterval
	}
	return DefaultRunnerSampleInterval
}

func (o RunnerServiceOptions) sampleBatch() int {
	if o.SampleBatch > 0 {
		return o.SampleBatch
	}
	return DefaultRunnerSampleBatch
}

// runnerServiceConfigured reports whether this worker can dispatch a code node
// over the runner protocol at all.
func (w *Worker) runnerServiceConfigured() bool { return w.opts.RunnerService.configured() }

// resolveRunnerService decides whether a code node's placement is a runner
// service, and returns the identity when it is.
//
// The lookup order is most-specific-first, and it is a real feature rather
// than a convenience: a deployment that wants ONE node of ONE workflow to run
// somewhere else registers runners.NodeKey(workflow, node) and changes nothing
// else, while a deployment that places every node using the same runner
// registers the node's `uses` reference once. Both are the placement-unaware
// property the protocol document describes — the workflow says what to run,
// the registry says where.
//
// A name that resolves to a managed FUNCTION identity is not this path's, and
// says so by returning false: code.go's in-process adapter dispatches those,
// and the registry's own Kind check is what keeps one name from meaning both.
func (w *Worker) resolveRunnerService(workflowKey string, node *nodeSpec) (runners.ServiceIdentity, string, bool) {
	if !w.runnerServiceConfigured() {
		return runners.ServiceIdentity{}, "", false
	}
	registry := w.opts.RunnerService.Registry
	for _, name := range []string{runners.NodeKey(workflowKey, node.ID), node.Uses} {
		if name == "" {
			continue
		}
		if kind, ok := registry.Kind(name); !ok || kind != runners.IdentityService {
			continue
		}
		identity, err := registry.ResolveService(name)
		if err != nil {
			// Kind said service and Resolve disagreed, which can only happen
			// if the registry changed between the two reads. Treat it as not
			// ours rather than guessing.
			continue
		}
		return identity, name, true
	}
	return runners.ServiceIdentity{}, "", false
}

// dispatchRunnerService submits a code node's operation to a runner service
// and parks the work item on the acceptance.
//
// The whole function is deliberately short-lived. It sends one bounded HTTP
// request, and whatever the answer is, this worker is finished with the
// operation before it returns: either the attempt failed here (a refused
// dispatch, which produced no execution and therefore no result), or the work
// item is parked and nothing in this process refers to it any more.
//
// It takes no heartbeat goroutine, unlike every synchronous dispatch in this
// package. That is not an oversight: the protocol obliges a runner to answer
// 202 quickly, the client bounds the request well inside one lease period, and
// a heartbeat here would be machinery for a case the contract forbids.
func (w *Worker) dispatchRunnerService(
	ctx context.Context,
	claimed postgres.ClaimedWork,
	d postgres.Dispatch,
	node *nodeSpec,
	dc DispatchContext,
	identity runners.ServiceIdentity,
	registryName string,
	operation runners.Operation,
) error {
	callback, err := w.runnerCallbackFor(dc, operation.OperationID)
	if err != nil {
		return w.failAttempt(ctx, claimed, w.codeRunnerActorID(), engine.StatusFailed, "configuration", err.Error())
	}

	accepted, err := w.opts.RunnerService.Client.Dispatch(ctx, identity, operation, callback)
	if err != nil {
		// No execution happened, so there is no Result and none is invented.
		// completeCodeDispatchError records the refusal's own classification
		// and the operation that was refused.
		return w.completeCodeDispatchError(ctx, claimed, d, node, operation, err)
	}

	// The sampling cadence: the runner's own request when it made one, this
	// worker's configured interval otherwise. The first sample is scheduled
	// one interval out rather than immediately — a runner that just answered
	// 202 has not started yet, and asking it straight away is a request whose
	// answer is known.
	interval := w.runnerSampleInterval(accepted.PollAfterSeconds)
	deadline := dc.Deadline
	if deadline.IsZero() {
		// A parked operation with no deadline is one nothing will ever
		// unstick. The worker's own default node timeout already applies
		// (deadlineFor), so this is a belt-and-braces floor rather than a
		// policy of its own.
		deadline = w.opts.Now().Add(w.opts.DefaultTimeout)
	}

	in := postgres.StartRunnerWaitInput{
		WorkID:                 claimed.ID,
		WorkerID:               w.opts.WorkerID,
		FencingToken:           claimed.FencingToken,
		Attempt:                int(claimed.Attempt),
		NamespaceID:            d.NamespaceID,
		RunID:                  dc.RunID,
		NodeRunID:              dc.NodeRunID,
		TokenID:                dc.TokenID,
		NodeID:                 node.ID,
		AttemptID:              dc.AttemptID,
		RunnerRef:              registryName,
		Endpoint:               identity.Endpoint,
		OperationID:            operation.OperationID,
		PollAfterSeconds:       accepted.PollAfterSeconds,
		StatusRetentionSeconds: accepted.StatusRetentionSeconds,
		SupportsCancellation:   accepted.SupportsCancellation,
		SupportsCallback:       accepted.SupportsCallback,
		NextPollAt:             w.opts.Now().Add(interval),
		Deadline:               deadline,
	}
	if err := w.db.StartRunnerWait(ctx, in); err != nil {
		if isStale(err) {
			// The claim went while the runner was accepting. Nothing was
			// written; whoever holds the item now will dispatch it again, and
			// this operation's eventual result is simply never sampled — the
			// runner's own status retention expires and the operation is
			// garbage. That is the honest outcome: this worker no longer has
			// the authority to commit anything about it.
			return nil
		}
		return err
	}
	return nil
}

// runnerCallbackFor builds the optional callback offer.
//
// A worker with no signer, no public base URL, or an explicit opt-out offers
// nothing — and that is a fully conformant deployment, because polling alone is
// sufficient. This is the one place the two async paths deliberately differ
// from each other: dispatchActor treats a missing callback as a hard
// configuration failure, because an ASYNC ACTOR has no other way to report and
// would be stranded. A runner has another way; it is the primary way.
func (w *Worker) runnerCallbackFor(dc DispatchContext, operationID string) (runners.CallbackOffer, error) {
	if w.opts.RunnerService.DisableCallback || w.opts.Signer == nil || w.opts.CallbackBaseURL == "" {
		return runners.CallbackOffer{}, nil
	}
	var (
		token string
		err   error
	)
	if dc.Deadline.IsZero() {
		token, err = w.opts.Signer.Mint(dc.AttemptID)
	} else {
		token, err = w.opts.Signer.MintUntil(dc.AttemptID, dc.Deadline.Add(runnerCallbackTokenGrace))
	}
	if err != nil {
		return runners.CallbackOffer{}, fmt.Errorf("runner callback token could not be minted: %w", err)
	}
	base := w.opts.CallbackBaseURL
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	return runners.CallbackOffer{
		URL:   base + fmt.Sprintf(runners.CallbackPathFormat, operationID),
		Token: token,
	}, nil
}

// runnerSampleInterval is how long until the next sample: the runner's own
// requested minimum when it declared one, this worker's configured cadence
// otherwise. The runner's request is a floor, not a ceiling — a runtime that
// sampled faster than a runner asked for would be generating load the runner
// said it did not want.
func (w *Worker) runnerSampleInterval(pollAfterSeconds int) time.Duration {
	interval := w.opts.RunnerService.pollInterval()
	if requested := time.Duration(pollAfterSeconds) * time.Second; requested > interval {
		return requested
	}
	return interval
}

// SampleRunnerOperations performs one sampling pass and returns how many
// parked operations it sampled.
//
// This is the whole of the runtime's polling responsibility, and its shape is
// the acceptance criterion: it wakes, reads the due operations out of durable
// state, samples each one exactly once, commits the terminal ones, and
// returns. It starts no goroutines, holds nothing between calls, and knows
// nothing about any operation it did not just read from the table — so two
// workers running it, or one worker running it after another was killed, are
// the same thing from the store's point of view.
//
// A per-operation failure never fails the pass: the operation stays parked,
// its diagnostic is recorded, and the next pass tries again — until the
// attempt's own deadline timer decides the wait has gone on long enough.
func (w *Worker) SampleRunnerOperations(ctx context.Context) (int, error) {
	if !w.runnerServiceConfigured() {
		return 0, nil
	}
	interval := w.opts.RunnerService.pollInterval()
	// Claiming advances next_poll_at by one interval, which doubles as the
	// crash guard: if this pass dies between the claim and the commit, the
	// operation simply becomes due again one interval later and whichever
	// worker is alive then picks it up. Nothing has to notice the death.
	//
	// The claim uses this worker's BASE interval for the whole batch, and the
	// per-operation schedule that honours a runner's own poll_after_seconds is
	// written afterwards by RescheduleRunnerPoll — which wins, because it
	// happens second. The one visible consequence: a pass that dies mid-sample
	// leaves an operation due again after the base interval even if its runner
	// asked for a longer one, so a crash can cost that runner one early
	// sample. Sampling one operation once sooner than asked, once per worker
	// death, is a better trade than a crash guard whose backoff is read from
	// the row being claimed.
	due, err := w.db.ClaimDueRunnerOperations(ctx, w.opts.NamespaceID, w.opts.Now(), w.opts.RunnerService.sampleBatch(), interval)
	if err != nil {
		return 0, fmt.Errorf("worker: claim due runner operations: %w", err)
	}

	for i := range due {
		if err := w.sampleRunnerOperation(ctx, due[i]); err != nil {
			w.report(fmt.Errorf("worker: sample runner operation %s (attempt %s): %w",
				due[i].OperationID, due[i].AttemptID, err))
		}
	}
	return len(due), nil
}

// sampleRunnerOperation reads one parked operation's status and acts on it.
//
// Three outcomes, and the middle one is the interesting one:
//
//   - a terminal status commits (commitRunnerTerminal);
//   - a NON-terminal status reschedules, recording what the runner said it was
//     doing;
//   - a dispatch error also reschedules, recording the classification. It is
//     never read as an outcome. A 404 in particular — a runner that forgot an
//     operation it accepted — is `runner_unavailable`, not a completion:
//     reading silence as a result would put an unmeasured claim in the ledger,
//     and the attempt's deadline timer is the honest thing that eventually
//     ends the wait.
func (w *Worker) sampleRunnerOperation(ctx context.Context, op postgres.RunnerOperation) error {
	identity, err := w.identityForSample(op)
	if err != nil {
		w.recordSampleFailure(ctx, op, err)
		return nil
	}

	status, err := w.opts.RunnerService.Client.Status(ctx, identity, op.OperationID)
	if err != nil {
		w.recordSampleFailure(ctx, op, err)
		return nil
	}

	if !status.Terminal() {
		return w.db.RescheduleRunnerPoll(ctx, op.NamespaceID, op.AttemptID,
			w.nextSampleAt(op), string(status.State), "")
	}
	return w.commitRunnerTerminal(ctx, op, *status.Result)
}

// identityForSample re-resolves the runner a parked operation was dispatched
// to, through the registry rather than from the row.
//
// The row records the endpoint, but the REGISTRY is the allowlist, and this is
// where that distinction pays: de-registering a runner service stops this
// runtime talking to it on the very next sample, instead of leaving parked rows
// that keep reaching an endpoint the operator has revoked. The cost is that a
// registry rebuild that drops a name strands its in-flight operations — which
// is correct, and is exactly what the deadline timer is for.
func (w *Worker) identityForSample(op postgres.RunnerOperation) (runners.ServiceIdentity, error) {
	identity, err := w.opts.RunnerService.Registry.ResolveService(op.RunnerRef)
	if err != nil {
		return runners.ServiceIdentity{}, err
	}
	if identity.Endpoint != op.Endpoint {
		// The name still resolves, but to somewhere else. Sampling the new
		// endpoint for an operation dispatched to the old one would read a
		// status this attempt never created.
		return runners.ServiceIdentity{}, fmt.Errorf(
			"runner %q now points at %s but operation %s was dispatched to %s; "+
				"a repointed runner's status is not this attempt's status",
			op.RunnerRef, identity.Endpoint, op.OperationID, op.Endpoint)
	}
	return identity, nil
}

// nextSampleAt is when a still-running operation should be read again.
func (w *Worker) nextSampleAt(op postgres.RunnerOperation) time.Time {
	return w.opts.Now().Add(w.runnerSampleInterval(op.PollAfterSeconds))
}

// recordSampleFailure reschedules an operation whose sample learned nothing
// and records why, both on the row (for "why is this wait not progressing")
// and, once, in the run's audit log.
//
// It never fails the attempt. A failing sample is not a failing operation: the
// operation may well be running perfectly on the other side of a network
// partition. Only the deadline timer is allowed to end the wait.
func (w *Worker) recordSampleFailure(ctx context.Context, op postgres.RunnerOperation, sampleErr error) {
	detail := sampleErr.Error()
	if err := w.db.RescheduleRunnerPoll(ctx, op.NamespaceID, op.AttemptID, w.nextSampleAt(op), "", detail); err != nil {
		w.report(fmt.Errorf("worker: reschedule runner operation %s: %w", op.AttemptID, err))
	}
	w.recordRunnerDiagnostic(ctx, op, TypeRunnerSampleFailed, detail)
}

// recordRunnerDiagnostic appends one audit line about an operation,
// best-effort. A failure to write an audit line must not turn a correctly
// handled sample into an error somebody retries; the row's own
// last_sample_error already carries the fact.
func (w *Worker) recordRunnerDiagnostic(ctx context.Context, op postgres.RunnerOperation, eventType, detail string) {
	data := map[string]any{
		"run_id":       op.RunID,
		"node_run_id":  op.NodeRunID,
		"node_id":      op.NodeID,
		"attempt_id":   op.AttemptID,
		"operation_id": op.OperationID,
		"runner_ref":   op.RunnerRef,
		"poll_count":   op.PollCount,
	}
	if detail != "" {
		data["detail"] = detail
	}
	if err := w.callbacks.AppendRunEvent(ctx, op.NamespaceID, op.RunID, eventType, data); err != nil {
		w.report(fmt.Errorf("worker: append %s event for attempt %s: %w", eventType, op.AttemptID, err))
	}
}

// commitRunnerTerminal turns a terminal status into a committed attempt.
//
// It is internal/actors.commitTerminal's two-step shape, for the same reason
// and with the same guarantee: re-lease the parked work item under the fencing
// tuple recorded at dispatch, then commit through the engine's own §12.5
// transaction. Both steps are fenced, and that is what makes duplicate and
// racing reports harmless without any dedup bookkeeping of their own:
//
//   - Two samplers that both read the same terminal status both get here. The
//     first re-leases the item out of 'waiting'; the second's UPDATE matches
//     nothing, because the row is no longer parked under that tuple. The
//     loser records a late diagnostic and commits nothing.
//   - A sample racing a deadline timer resolves the same way, in whichever
//     direction the race went.
//   - A completion for an attempt that was reclaimed and re-run resolves the
//     same way too: the reclaim bumped the fencing token, so the tuple this
//     row recorded no longer matches anything.
//
// There is no "have I already committed this" flag anywhere in this path, and
// deliberately so. A flag is a second source of truth that can disagree with
// the work item; the fencing tuple cannot, because it IS the work item.
func (w *Worker) commitRunnerTerminal(ctx context.Context, op postgres.RunnerOperation, result runners.Result) error {
	node, err := w.nodeForOperation(ctx, op)
	if err != nil {
		return err
	}

	completion, buildErr := w.runnerCompletionFor(node, result)

	if hook := w.opts.RunnerService.Hooks.BeforeCommit; hook != nil {
		hook(op.AttemptID)
	}

	inv := op.PendingInvocation()
	if err := w.callbacks.ResumeWaitingWork(ctx, inv, runnerResumeLease); err != nil {
		if errors.Is(err, engine.ErrStaleClaim) {
			return w.recordLateRunnerCompletion(ctx, op, fmt.Sprintf(
				"attempt %s is no longer parked under fencing token %d attempt %d; "+
					"the work was reclaimed, cancelled, or already completed by another report",
				op.AttemptID, op.FencingToken, op.Attempt))
		}
		return err
	}

	req := engine.CompletionRequest{
		WorkID:       op.WorkID,
		WorkerID:     op.WorkerID,
		FencingToken: op.FencingToken,
		Attempt:      op.Attempt,
	}
	if buildErr != nil {
		// The result could not be mapped onto this node's contract at all.
		// That is a contract problem, not a claim about what the operation
		// did — and the raw Result is still recorded below, so it stays
		// inspectable. The attribution is the same code-runner identity the
		// mappable branch stamps: whichever way the mapping went, this was
		// that runner's operation.
		req.TechStatus = engine.StatusContractRejected
		req.Output = diagnosticOutput("runner",
			fmt.Sprintf("node %q result could not be mapped onto a completion: %v", op.NodeID, buildErr), nil)
		req.ActorID = w.codeRunnerActorID()
	} else {
		req.TechStatus = completion.TechStatus
		req.Outcome = completion.Outcome
		req.Output = completion.Output
		req.LedgerDelta = completion.LedgerDelta
		req.RunnerManifest = completion.RunnerManifest
		req.ActorID = w.codeRunnerActorID()
	}

	// Task t17 (issue #37): the same pre-routing acceptance evaluation
	// code.go applies on an in-process dispatch — a node's enforce policy
	// must mean the same thing whichever transport carried the operation.
	var eval *acceptanceEvaluation
	if buildErr == nil && node != nil {
		if eval = evaluateAcceptance(node, result); eval != nil {
			req.TechStatus, req.Outcome = eval.apply(req.TechStatus, req.Outcome)
		}
	}

	committed, err := w.engine.CompleteAttempt(ctx, req)
	if err != nil {
		if isStale(err) {
			// The engine's own fenced guard refused: the resume won its race
			// but something newer committed before this call reached the
			// engine. Nothing was written — the whole §12.5 transaction rolled
			// back — so there is nothing to undo.
			return w.recordLateRunnerCompletion(ctx, op,
				fmt.Sprintf("the engine refused the late completion of attempt %s: %v", op.AttemptID, err))
		}
		return err
	}
	// Same post-commit, best-effort step w.complete performs: a resumed
	// runner completion can be the arrival that fires an any/quorum barrier
	// and reaps the group's losing branches (issue #43, branchcancel.go).
	w.propagateBranchCancellations(ctx, committed)

	if err := w.db.CloseRunnerOperation(ctx, op.NamespaceID, op.AttemptID, postgres.RunnerOperationCompleted); err != nil {
		return err
	}

	// The evidence half of the life cycle: the typed operation that was sent
	// and the result that came back, recorded against the attempt that just
	// committed — the same row code.go writes for an in-process dispatch.
	operation, opErr := w.operationForRecord(op, node)
	if opErr == nil {
		w.recordRunnerOperation(ctx, op.NamespaceID, committed.AttemptID, runnerOperationKind, operation, &result, nil)
	}
	w.appendAcceptanceVerdict(ctx, node, eval, committed)
	if buildErr == nil {
		w.evaluateSuccessSignals(ctx, result, committed)
	}
	return nil
}

// runnerCompletionFor maps a Result onto this node's contract. It is code.go's
// mapping, reached from the sampler instead of from the dispatch — the same
// convention resolver, so a node's exit-status ports mean the same thing
// whichever transport carried the operation.
func (w *Worker) runnerCompletionFor(node *nodeSpec, result runners.Result) (runners.Completion, error) {
	if node == nil {
		return runners.Completion{}, errors.New("the pinned definition no longer declares this node")
	}
	outcomes, err := w.codeOutcomes(node)
	if err != nil {
		return runners.Completion{}, err
	}
	return runners.BuildCompletion(result, runners.NodeContract{
		NodeID:         node.ID,
		SuccessOutcome: outcomes.Success,
		FailureOutcome: outcomes.Failure,
		// The gate vocabulary's exit-code map rides here too — dropping it
		// made a parked gate's exit 2 a technical failure instead of
		// measurement_incomplete, while the same node completing
		// synchronously routed correctly (found live by the t8 demo; the
		// sync path in code.go has always set it).
		ExitCodeOutcomes: outcomes.ByExitCode,
		ActorID:          w.codeRunnerActorID(),
		ActorRevision:    w.opts.CodeRunnerRevision,
	})
}

// nodeForOperation reloads the pinned node a parked operation belongs to.
//
// It re-reads the definition rather than carrying it in memory because there
// is no memory to carry it in: §12.6 means the worker that dispatched this
// operation may be a different process, or may no longer exist. The definition
// is pinned by digest, so what comes back here is the same document the
// dispatch built the operation from.
func (w *Worker) nodeForOperation(ctx context.Context, op postgres.RunnerOperation) (*nodeSpec, error) {
	d, err := w.db.LoadDispatch(ctx, op.NodeRunID)
	if err != nil {
		return nil, err
	}
	spec, err := w.specs.get(d.WorkflowDigest, d.NormalizedIR)
	if err != nil {
		return nil, err
	}
	return spec.Nodes[op.NodeID], nil
}

// operationForRecord rebuilds the typed operation for the evidence row. It is
// rebuilt from the same pinned definition and the same attempt id the dispatch
// used, so it is the document that was sent, not an approximation of it.
func (w *Worker) operationForRecord(op postgres.RunnerOperation, node *nodeSpec) (runners.Operation, error) {
	if node == nil {
		return runners.Operation{}, errors.New("worker: no pinned node for this operation")
	}
	operation, err := w.buildCodeOperation(node, DispatchContext{
		RunID:     op.RunID,
		NodeRunID: op.NodeRunID,
		NodeID:    op.NodeID,
		AttemptID: op.AttemptID,
	})
	if err != nil {
		w.report(fmt.Errorf("worker: rebuild operation for evidence row (attempt %s): %w", op.AttemptID, err))
		return runners.Operation{}, err
	}
	return operation, nil
}

// recordLateRunnerCompletion is the losing duplicate's only trace: an audit
// line and a closed record. It deliberately returns no error — the protocol
// behaved exactly as designed, and a duplicate that is expected traffic must
// not surface as a worker malfunction.
func (w *Worker) recordLateRunnerCompletion(ctx context.Context, op postgres.RunnerOperation, detail string) error {
	w.recordRunnerDiagnostic(ctx, op, TypeRunnerCompletionLate, detail)
	return w.db.CloseRunnerOperation(ctx, op.NamespaceID, op.AttemptID, postgres.RunnerOperationSuperseded)
}

// HandleRunnerCallback ingests one optional completion notification and
// reports whether it brought a sample forward.
//
// Read what this function does NOT do: it does not look at the notification's
// `state`, it does not record an outcome, and it cannot reach the engine from
// here even in principle. Its entire effect is TightenRunnerPoll — the next
// authenticated status sample happens sooner. That is what makes "a callback
// only tightens latency" a structural fact rather than a promise: there is no
// code path from a notification to a committed result.
//
// The token is still verified, and a forged one is still refused, for the same
// reason the actor ingress refuses one: an unauthenticated stranger should not
// be able to make this runtime generate load against a runner service.
func (w *Worker) HandleRunnerCallback(ctx context.Context, attemptToken string, note runners.CallbackNotification) (bool, error) {
	switch {
	case !w.runnerServiceConfigured():
		return false, errors.New("worker: this worker dispatches no runner operations, so it accepts no runner callbacks")
	case w.opts.Signer == nil:
		return false, errors.New("worker: this worker has no token signer, so it cannot verify a runner callback")
	case note.OperationID == "":
		return false, errors.New("worker: a runner callback names no operation")
	}

	attemptID, err := w.opts.Signer.Verify(attemptToken)
	if err != nil {
		return false, err
	}

	op, err := w.db.RunnerOperation(ctx, w.opts.NamespaceID, attemptID)
	if err != nil {
		return false, err
	}
	if op.OperationID != note.OperationID {
		// The token authorises reporting on this attempt; the body names some
		// other operation. Refusing costs a caller nothing (its own operation
		// is polled regardless) and closes the one shape of confusion this
		// endpoint could otherwise be used to create.
		return false, fmt.Errorf("worker: the callback token authorises attempt %s (operation %s), not operation %s",
			attemptID, op.OperationID, note.OperationID)
	}

	return w.db.TightenRunnerPoll(ctx, w.opts.NamespaceID, attemptID, w.opts.Now())
}

// runnerCallbackResponse is the body the callback endpoint returns.
type runnerCallbackResponse struct {
	OperationID string `json:"operation_id,omitempty"`
	Disposition string `json:"disposition,omitempty"`
	Error       string `json:"error,omitempty"`
}

// maxRunnerCallbackBodyBytes bounds a notification body. The document is two
// fields; anything larger is not a notification.
const maxRunnerCallbackBodyBytes = 16 << 10

// NewRunnerCallbackHandler returns the http.Handler for the URL a dispatch
// offers as Nodes-Callback-Url.
//
// It is a plain handler rather than a server so the API process can mount it
// on whatever mux it already has. The status codes are chosen for a runner's
// retry logic, and they are all cheap on purpose — a runner that retries a
// notification forever costs this runtime a token verification and one row
// update, never a state change.
func NewRunnerCallbackHandler(w *Worker) http.Handler {
	return &runnerCallbackHandler{worker: w}
}

type runnerCallbackHandler struct{ worker *Worker }

func (h *runnerCallbackHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeRunnerCallbackJSON(rw, http.StatusMethodNotAllowed,
			runnerCallbackResponse{Error: "runner callbacks are POSTed"})
		return
	}
	token, ok := runnerBearerToken(r)
	if !ok {
		writeRunnerCallbackJSON(rw, http.StatusUnauthorized,
			runnerCallbackResponse{Error: "an attempt-scoped bearer token is required"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRunnerCallbackBodyBytes))
	if err != nil {
		writeRunnerCallbackJSON(rw, http.StatusBadRequest,
			runnerCallbackResponse{Error: "callback body could not be read"})
		return
	}
	var note runners.CallbackNotification
	if err := json.Unmarshal(body, &note); err != nil {
		writeRunnerCallbackJSON(rw, http.StatusBadRequest,
			runnerCallbackResponse{Error: "callback body is not a runner-protocol notification: " + err.Error()})
		return
	}

	tightened, err := h.worker.HandleRunnerCallback(r.Context(), token, note)
	switch {
	case err == nil:
		disposition := "already_scheduled"
		if tightened {
			disposition = "sample_advanced"
		}
		writeRunnerCallbackJSON(rw, http.StatusAccepted,
			runnerCallbackResponse{OperationID: note.OperationID, Disposition: disposition})
	case errors.Is(err, actors.ErrToken):
		writeRunnerCallbackJSON(rw, http.StatusUnauthorized, runnerCallbackResponse{Error: err.Error()})
	default:
		// Everything else — an unknown attempt, a mismatched operation — is a
		// 404 rather than a 500: the runtime is fine, this notification just
		// does not correspond to anything it is waiting on. A runner that
		// retries it will get the same answer, which is the correct signal to
		// stop.
		writeRunnerCallbackJSON(rw, http.StatusNotFound, runnerCallbackResponse{Error: err.Error()})
	}
}

func runnerBearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

func writeRunnerCallbackJSON(rw http.ResponseWriter, status int, body runnerCallbackResponse) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(body)
}
