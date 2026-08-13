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
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/telemetry"
)

// dispatchActor invokes a §13 actor for an `agent` or `action.http` node.
//
// Both kinds take one code path, and that is the point: §9.5 says the core
// engine does not branch on provider names, and the same reasoning applies a
// level down. An `agent` and an `action.http` node differ in what contract
// they satisfy and what a reader expects them to do, not in how the control
// plane talks to them. A second, near-identical branch would be the first
// place a provider-specific special case would eventually be added.
func (w *Worker) dispatchActor(
	ctx context.Context,
	claimed postgres.ClaimedWork,
	d postgres.Dispatch,
	spec *workflowSpec,
	node *nodeSpec,
	dc DispatchContext,
) (err error) {
	// Task t19's worker seam: one span and one metric recording per
	// dispatch attempt, wrapping every path through this function —
	// budget/registry/pre_run refusals, a synchronous result, an
	// asynchronous park, and an invocation error alike. node.Uses is the
	// actor *reference* the node names, recorded as the actor id even on a
	// path that never resolves an endpoint: which actor a dispatch was
	// addressed to is exactly the fact worth keeping on a failed dispatch.
	ctx, op := w.opts.Telemetry.Start(ctx, telemetry.SeamWorkerDispatch,
		telemetry.RunID(dc.RunID), telemetry.NodeID(dc.NodeID), telemetry.AttemptID(dc.AttemptID),
		telemetry.ActorID(node.Uses),
	)
	defer func() { op.End(ctx, err == nil) }()

	// The dispatch budget is checked before anything else this function can
	// do — before the registry lookup, before a pre_run hook, and certainly
	// before the actor is invoked — because everything below this line costs
	// something outside the control plane. See budget.go for why the check
	// lives on claimed work rather than in the claim SQL.
	if budgetExhausted(claimed) {
		return w.parkExhausted(ctx, claimed, node, dc)
	}

	if w.opts.Registry == nil {
		return w.failAttempt(ctx, claimed, "", engine.StatusFailed, "configuration",
			"this worker has no actor registry configured, so it cannot resolve an endpoint to invoke")
	}

	// Task t14, spec claim c37, honesty condition h32: a pre-run hook
	// executes through the runner boundary BEFORE the actor is dispatched.
	// Its failure fails the attempt as a technical failure and the agent is
	// never invoked — this is the one branch in this function that can
	// return without ever reaching the actor.
	var preRun *hookRun
	if node.PreRun != nil {
		proceed, run, err := w.runPreRunHook(ctx, claimed, d, node, dc)
		if err != nil {
			return err
		}
		if !proceed {
			return nil
		}
		preRun = run
	}

	endpoint, err := w.opts.Registry.Resolve(ctx, node.Uses)
	if err != nil {
		// An unresolvable actor is a policy/configuration refusal, not a
		// transport failure: retrying the same reference against the same
		// registry will fail the same way, and policy_denied is the status
		// the engine does not retry. The attempt stays unattributed ("" →
		// NULL): nothing was resolved, so there is no actor to charge.
		return w.failAttempt(ctx, claimed, "", engine.StatusPolicyDenied, string(actors.ClassAuthOrPolicy),
			fmt.Sprintf("node %q uses %q, which did not resolve to an endpoint: %v", node.ID, node.Uses, err))
	}

	// Best-effort durable attribution: the actors-table row id this
	// reference resolves to today, recorded on the attempt at completion
	// (attempts.actor_id) so per-actor surfaces can attribute the work —
	// on FAILURE completions from here on just as on success, because a
	// failed dispatch is still this actor's dispatch and the retry-burn
	// measure must see it. A registry that cannot answer (StaticRegistry,
	// a vanished row) yields "" — unattributed, never a dispatch failure.
	dc.ActorRowID = w.actorRowID(ctx, node.Uses)

	req := actors.InvocationRequest{
		ProtocolVersion: actors.ProtocolVersion,
		RunID:           dc.RunID,
		TokenID:         dc.TokenID,
		NodeRunID:       dc.NodeRunID,
		AttemptID:       dc.AttemptID,
		Attempt:         dc.Attempt,
		Workflow:        actors.WorkflowRef{Name: spec.Name, VersionDigest: spec.Digest},
		Node:            actors.NodeRef{ID: node.ID, ContractDigest: node.ContractDigest},
		Input:           dc.Input,
		ContinuationRef: w.priorContinuationRef(ctx, dc),
	}
	if !dc.Deadline.IsZero() {
		deadline := dc.Deadline.UTC()
		req.Deadline = &deadline
	}

	// The callback block is filled in even for a dispatch that turns out to
	// be synchronous, because the actor decides which it is and cannot ask
	// for the block afterwards.
	callback, err := w.callbackFor(dc)
	if err != nil {
		return w.failAttempt(ctx, claimed, dc.ActorRowID, engine.StatusFailed, "configuration", err.Error())
	}
	req.Callback = callback

	// The invocation is bounded by the node's own deadline: a synchronous
	// actor that blows through its declared timeout is a timeout, and the
	// lease must not be extended past it either.
	invokeCtx := ctx
	if !dc.Deadline.IsZero() {
		var cancel context.CancelFunc
		invokeCtx, cancel = context.WithDeadline(ctx, dc.Deadline)
		defer cancel()
	}

	var (
		response  actors.InvocationResponse
		invokeErr error
	)
	// The lease is heartbeated for exactly as long as the call runs. If the
	// heartbeat discovers the lease is gone, it cancels the invocation:
	// continuing would be work this worker could no longer commit.
	w.withHeartbeat(invokeCtx, claimed, func(hbCtx context.Context) {
		response, invokeErr = w.opts.Client.Invoke(hbCtx, endpoint, req)
	})

	if invokeErr != nil {
		return w.completeFromInvocationError(ctx, claimed, d, node, dc, invokeErr, preRun)
	}

	if !response.Async {
		return w.completeFromResult(ctx, claimed, d, node, dc, response.Result, preRun)
	}
	if node.PostRun != nil {
		// See hooks.go's package doc for why async+post_run is refused here
		// rather than run against a callback-delivered result.
		return w.refuseAsyncPostRun(ctx, claimed, d, node, dc, preRun)
	}
	return w.park(ctx, claimed, d, node, dc, response.Accepted)
}

// priorContinuationRef is §13.1's continuation_ref for this dispatch: the
// handle the most recent prior attempt AGAINST THIS ACTOR, IN THIS RUN,
// offered — nil when there is none (task t4,
// docs/adr/0010-continuation-ref-on-request.md).
//
// The scope is narrower than spec claim c3's eventual session key (actor +
// repo + workstream), and narrow on purpose. A workstream outlives a run, so
// keying on one needs the declared transport key all three bridges must
// exclude from their Bound-inputs block (task t5) and the per-key
// serialization that keeps two dispatches from interleaving turns on one
// provider thread (task t6). Neither exists yet, and resuming a conversation
// nothing declared it wanted resumed is worse than paying for a cold one. So
// this reads run + actor, and says so.
//
// Best-effort by construction: a lookup that fails is reported and the
// dispatch proceeds cold. A cold session costs more and is never wrong;
// failing a dispatch because an optimization could not be looked up would be.
// An unattributed dispatch (no resolved actor row id) looks up nothing —
// there is no identity whose conversation this would be.
func (w *Worker) priorContinuationRef(ctx context.Context, dc DispatchContext) *string {
	if dc.ActorRowID == "" {
		return nil
	}
	ref, ok, err := w.db.LatestContinuationRef(ctx, dc.RunID, dc.ActorRowID)
	if err != nil {
		w.report(fmt.Errorf("worker: run %s: look up prior continuation ref for actor %s: %w",
			dc.RunID, dc.ActorRowID, err))
		return nil
	}
	if !ok {
		return nil
	}
	return &ref
}

// callbackFor builds §13.1's callback block: where to POST, and a token that
// authorizes reporting on this attempt and nothing else.
//
// A worker with no signer or no public base URL cannot offer one, and it says
// so rather than sending an empty block. An actor handed a callback URL it
// cannot authenticate against would answer 202 and then discover it has no
// way to report — a failure mode that surfaces minutes later, in the actor's
// logs, instead of here.
func (w *Worker) callbackFor(dc DispatchContext) (actors.Callback, error) {
	if w.opts.Signer == nil || w.opts.CallbackBaseURL == "" {
		return actors.Callback{}, errors.New(
			"this worker offers no callback endpoint (no token signer or no callback base URL configured), " +
				"so it can only dispatch to actors that answer synchronously")
	}
	var (
		token string
		err   error
	)
	if dc.Deadline.IsZero() {
		token, err = w.opts.Signer.Mint(dc.AttemptID)
	} else {
		// A token must not outlive the work it authorizes. When the attempt
		// has a deadline, the token expires with it — plus a small grace so a
		// terminal callback sent right at the deadline is still accepted.
		token, err = w.opts.Signer.MintUntil(dc.AttemptID, dc.Deadline.Add(callbackTokenGrace))
	}
	if err != nil {
		return actors.Callback{}, fmt.Errorf("callback token could not be minted: %w", err)
	}
	return actors.Callback{
		URL:   w.callbackURL(dc.AttemptID),
		Token: token,
	}, nil
}

// callbackTokenGrace is how long a callback token outlives its attempt's
// deadline, so an actor that finished just in time is not refused for a clock
// skew of milliseconds.
const callbackTokenGrace = 2 * time.Minute

func (w *Worker) callbackURL(attemptID string) string {
	base := w.opts.CallbackBaseURL
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	return base + fmt.Sprintf(actors.CallbackEventsPathFormat, attemptID)
}

// completeFromResult commits a §13.2 synchronous result, running the node's
// post_run hook first when it declares one (task t14, spec claim c37).
//
// preRun is the already-executed pre_run hook, nil when the node declares
// none; its evidence is appended after whatever completion this call
// ultimately reports, exactly like the post_run hook's own evidence (see
// appendHookEvidence for why neither is folded into the completion's own
// ledger delta).
func (w *Worker) completeFromResult(
	ctx context.Context, claimed postgres.ClaimedWork, d postgres.Dispatch, node *nodeSpec, dc DispatchContext,
	result *actors.InvocationResult, preRun *hookRun,
) error {
	if result == nil {
		return w.failAttempt(ctx, claimed, dc.ActorRowID, engine.StatusContractRejected, string(actors.ClassContract),
			"actor answered 200 with no result body")
	}

	agentDelta := append([]ledger.Record(nil), result.Records()...)

	var postRun *hookRun
	outcome, output := result.Outcome, result.Output
	rejectAssurance := false

	if node.PostRun != nil {
		post := w.runPostRunHook(ctx, node, dc)
		postRun = &post.run

		if !post.trustworthy {
			// The hook itself could not be trusted to report a verdict at
			// all: an attempt-level technical failure, not a domain answer
			// — silently keeping the agent's own outcome here would be
			// exactly the unenforced-check gap h32 forbids. The agent's own
			// proposed records still ride along; only the routing changes —
			// and so does the result's reported usage (issue #32): the
			// invocation itself succeeded and burned real tokens regardless
			// of what the hook could not verify.
			completion, err := w.completeTechnicalFailure(ctx, claimed, dc.ActorRowID, engine.StatusFailed, hookKindPostRun, post.detail, agentDelta,
				actorTelemetry{
					Usage:             result.Usage.ToEngine(),
					TerminationReason: result.TerminationReason,
					// The invocation itself happened and its session still
					// exists, whatever the hook could not verify about the
					// work — dropping the handle here would re-open the
					// silent drop ADR 0010 closes.
					ContinuationRef: result.ContinuationRef,
				})
			if err != nil {
				return err
			}
			w.appendHookEvidence(ctx, completion, preRun)
			w.appendHookEvidence(ctx, completion, postRun)
			w.recordHookOperations(ctx, d.NamespaceID, completion.AttemptID, preRun, postRun)
			return nil
		}

		if !post.passed {
			if node.PostRun.OnFailure.RejectAssurance {
				// The agent's own outcome still stands; a derived rejection
				// is appended after the completion commits (see
				// appendAssuranceRejection).
				rejectAssurance = true
			} else {
				// A declared outcome: complete with THAT domain outcome
				// instead of the agent's own — the check ran and told us
				// this node's real answer, not the agent's.
				outcome = node.PostRun.OnFailure.Outcome
			}
		}
	}

	completion, err := w.complete(ctx, claimed, engine.CompletionRequest{
		TechStatus: engine.StatusSucceeded,
		Outcome:    outcome,
		// The bridge-measured workspace block rides inside the persisted
		// output so /nodes/<id>/output bindings carry it downstream; absent
		// stays absent, and a measured:false block survives verbatim
		// (issue #33a — actor-reported data, never observed evidence).
		Output:      actors.MergeWorkspaceMeasured(output, result.WorkspaceMeasured),
		LedgerDelta: agentDelta,
		Usage:       result.Usage.ToEngine(),
		// Beside the usage, never inside it: a turn that ended for a
		// knowable reason may have reported no usage block (ADR 0009).
		TerminationReason: result.TerminationReason,
		// The handle §13.2 lets the actor offer for continuing this
		// conversation. It was read off the wire and dropped here before
		// task t4 (spec scope entry s3), which is why every node turn
		// started a cold session.
		ContinuationRef: result.ContinuationRef,
		ActorID:         dc.ActorRowID,
	})
	if err != nil {
		if isStale(err) {
			return nil
		}
		return err
	}

	w.appendHookEvidence(ctx, completion, preRun)
	postEvidence, postEvidenceOK := w.appendHookEvidence(ctx, completion, postRun)
	w.recordHookOperations(ctx, d.NamespaceID, completion.AttemptID, preRun, postRun)
	// Task t17 (issue #37): an agent node's acceptance checks are not
	// mechanically evaluable — no runner-measured Result exists for an agent
	// dispatch — so a routing enforce policy gets the honest floor: routing
	// stayed the agent's own outcome (as committed above) and a derived
	// record states the non-evaluability. See acceptance.go's package doc.
	w.appendUnevaluableAcceptance(ctx, node, completion)
	if rejectAssurance {
		subject := ""
		if postEvidenceOK {
			subject = postEvidence.ID
		}
		w.appendAssuranceRejection(ctx, dc, completion, postRun, subject)
	}
	return nil
}

// completeFromInvocationError records a §13.5-classified failure.
//
// The class is preserved in the attempt's output as well as being mapped onto
// a technical status, because the mapping is lossy on purpose — three classes
// share `failed` — and an operator asking "was it the network or the actor"
// needs the class, not the status.
//
// preRun is threaded through here too: a pre_run hook that passed still
// measured something even though the subsequent invocation itself failed
// technically, and that evidence is not dropped just because the actor never
// answered.
//
// So is the error body's usage block (issue #32, task t5): a bridge whose
// session failed after producing a parseable terminal result attaches the
// §13.2 usage to its 500 body, and the failed attempt persists that burn —
// failures get retried, so their burn compounds, and the rollups must see
// it. A result-less crash or timeout carries no block and the attempt's
// usage stays NULL (the h24 narrowing, stated in migrations/README.md's
// 0012 entry).
func (w *Worker) completeFromInvocationError(
	ctx context.Context, claimed postgres.ClaimedWork, d postgres.Dispatch, node *nodeSpec, dc DispatchContext,
	invokeErr error, preRun *hookRun,
) error {
	class, ok := actors.ClassOf(invokeErr)
	if !ok {
		class = actors.ClassExecution
	}
	completion, err := w.completeTechnicalFailure(ctx, claimed, dc.ActorRowID, actors.TechStatusFor(class), string(class),
		fmt.Sprintf("node %q invocation failed: %v", node.ID, invokeErr),
		nil, actorTelemetry{
			Usage: actors.UsageOf(invokeErr).ToEngine(),
			// The provider's reason for ending the turn, which an error
			// body can carry with no usage block at all (ADR 0009).
			TerminationReason: actors.TerminationReasonOf(invokeErr),
		})
	if err != nil {
		return err
	}
	w.appendHookEvidence(ctx, completion, preRun)
	w.recordHookOperations(ctx, d.NamespaceID, completion.AttemptID, preRun, nil)
	return nil
}

// park is §12.6: an asynchronous acceptance releases worker capacity.
//
// After this commits, nothing in this process holds anything about the
// invocation. The work item is in `waiting` (invisible to ClaimWork and to
// ReclaimExpired), the node run is `waiting_external`, the durable
// actor_invocations row carries the fencing tuple a callback will need, and a
// deadline timer is the only thing that will ever wake it if the actor never
// reports. The worker may exit immediately afterwards with no loss.
func (w *Worker) park(
	ctx context.Context,
	claimed postgres.ClaimedWork,
	d postgres.Dispatch,
	node *nodeSpec,
	dc DispatchContext,
	accepted *actors.AsyncAccepted,
) error {
	if accepted == nil {
		return w.failAttempt(ctx, claimed, dc.ActorRowID, engine.StatusContractRejected, string(actors.ClassContract),
			"actor answered 202 with no acceptance body")
	}

	in := postgres.StartAsyncWaitInput{
		WorkID:                claimed.ID,
		WorkerID:              w.opts.WorkerID,
		FencingToken:          claimed.FencingToken,
		Attempt:               int(claimed.Attempt),
		NamespaceID:           d.NamespaceID,
		RunID:                 dc.RunID,
		NodeRunID:             dc.NodeRunID,
		TokenID:               dc.TokenID,
		NodeID:                node.ID,
		AttemptID:             dc.AttemptID,
		ActorRef:              node.Uses,
		ActorID:               dc.ActorRowID,
		InvocationID:          accepted.InvocationID,
		HeartbeatAfterSeconds: accepted.HeartbeatAfterSeconds,
		SupportsCancellation:  accepted.SupportsCancellation,
		Deadline:              w.asyncDeadline(dc, accepted),
	}
	if err := w.db.StartAsyncWait(ctx, in); err != nil {
		if isStale(err) {
			// The claim went while the actor was accepting. Nothing was
			// written; whoever holds the item now will dispatch it again, and
			// the actor's eventual callback will be refused as late — which
			// is exactly §13.4's designed behaviour.
			return nil
		}
		return err
	}
	return nil
}

// asyncDeadline is when the wait must be given up on.
//
// The node's own deadline wins when it declares one: a workflow author who
// said "this node has 20 minutes" meant it regardless of what the actor
// promised about heartbeats. Otherwise the actor's declared heartbeat
// interval is used, multiplied by a tolerance so a single missed beat is not
// fatal. An actor that declared neither gets no timer, and the wait is then
// genuinely open-ended — which is recorded rather than hidden, because it is
// the deployment's choice to run such an actor.
func (w *Worker) asyncDeadline(dc DispatchContext, accepted *actors.AsyncAccepted) time.Time {
	if !dc.Deadline.IsZero() {
		return dc.Deadline
	}
	if accepted.HeartbeatAfterSeconds > 0 {
		return w.opts.Now().Add(time.Duration(accepted.HeartbeatAfterSeconds) * time.Second * missedHeartbeatTolerance)
	}
	return time.Time{}
}

// missedHeartbeatTolerance is how many heartbeat intervals may pass before
// the invocation is considered overdue.
const missedHeartbeatTolerance = 3

// dispatchDecision evaluates a decision node in-process (§9.2, §11.2).
func (w *Worker) dispatchDecision(
	ctx context.Context,
	claimed postgres.ClaimedWork,
	d postgres.Dispatch,
	spec *workflowSpec,
	node *nodeSpec,
	dc DispatchContext,
) error {
	outcome, matched, err := w.decisions.evaluateDecision(spec.Digest, node, d.RunInput, dc.Input)
	if err != nil {
		return w.failAttempt(ctx, claimed, "", engine.StatusFailed, "decision", err.Error())
	}
	if !matched {
		return w.failAttempt(ctx, claimed, "", engine.StatusFailed, "decision",
			fmt.Sprintf("no select port of decision node %q matched; the workflow declares no answer for this data", node.ID))
	}

	// The recorded output is the payload the ports were evaluated against, so
	// a downstream /nodes/<decision>/output binding reads the same document
	// the decision was made from. See decision.go for why that is the honest
	// meaning of a decision node's "output".
	_, err = w.complete(ctx, claimed, engine.CompletionRequest{
		TechStatus: engine.StatusSucceeded,
		Outcome:    outcome,
		Output:     dc.Input,
	})
	if err != nil && !isStale(err) {
		return err
	}
	return nil
}

// dispatchSeam runs a registered seam, or diagnoses its absence.
func (w *Worker) dispatchSeam(
	ctx context.Context,
	claimed postgres.ClaimedWork,
	d postgres.Dispatch,
	node *nodeSpec,
	dc DispatchContext,
	kind string,
	capability string,
	run func() (SeamResult, error),
) error {
	var (
		result  SeamResult
		seamErr error
	)
	w.withHeartbeat(ctx, claimed, func(context.Context) {
		result, seamErr = run()
	})

	if errors.Is(seamErr, errNoSeam) {
		// The diagnostic names what is actually missing, per kind. It used to
		// say all three seams "land in later build-plan tasks", which has
		// stopped being true one kind at a time: `code` now has two
		// implementations of its own (code.go's in-process runner and
		// runnerasync.go's runner protocol), and `approval` is served
		// engine-side, so a node reaching this branch for either is a
		// CONFIGURATION gap rather than an unbuilt feature. Saying "not yet
		// implemented" for something that is implemented but unconfigured
		// sends an operator to the build plan instead of to their own config.
		return w.failAttempt(ctx, claimed, "", engine.StatusFailed, "not_implemented",
			fmt.Sprintf("node %q is a %s node and this worker has no %s dispatcher registered; %s",
				dc.NodeID, kind, capability, seamRemedy(kind)))
	}
	if seamErr != nil {
		return w.failAttempt(ctx, claimed, "", engine.StatusFailed, kind,
			fmt.Sprintf("node %q %s dispatch failed: %v", dc.NodeID, kind, seamErr))
	}

	if result.Async {
		// A wait seam's async answer parks on a durable TIMER, not on an
		// actor invocation: there is no external party to record, no
		// callback to fence, and — critically — no deadline timer, because a
		// wait must be allowed to be away for exactly its declared time.
		if kind == kindWait {
			return w.parkWait(ctx, claimed, d, dc, result)
		}
		// Any other seam that took ownership parks the work item exactly as
		// a §13.3 acceptance does: same durable record, same fencing tuple,
		// same released capacity. The seam's own handle stands in for the
		// actor's invocation id.
		// Seam dispatches (code/runner paths) attribute at their own
		// completion sites; no actor row resolution happened here, so
		// dc.ActorRowID is still "" and park records no attribution.
		return w.park(ctx, claimed, d, node, dc, &actors.AsyncAccepted{
			InvocationID:          result.AsyncRef,
			HeartbeatAfterSeconds: 0,
		})
	}

	var delta []ledger.Record
	if len(result.LedgerDelta) > 0 {
		var records struct {
			Records []ledger.Record `json:"records"`
		}
		if err := json.Unmarshal(result.LedgerDelta, &records); err != nil {
			return w.failAttempt(ctx, claimed, "", engine.StatusContractRejected, string(actors.ClassContract),
				fmt.Sprintf("node %q %s dispatch proposed a ledger delta that is not a records document: %v",
					dc.NodeID, kind, err))
		}
		delta = records.Records
	}

	_, err := w.complete(ctx, claimed, engine.CompletionRequest{
		TechStatus:  result.TechStatus,
		Outcome:     result.Outcome,
		Output:      result.Output,
		LedgerDelta: delta,
	})
	if err != nil && !isStale(err) {
		return err
	}
	return nil
}

// seamRemedy names the configuration a missing seam actually needs, so the
// diagnostic points at the knob rather than at the roadmap.
func seamRemedy(kind string) string {
	switch kind {
	case kindCode:
		return "configure Options.CodeRunner for an in-process runner, or Options.RunnerService with a " +
			"registered runner-service identity for this node (api/runner-protocol), or Options.Runner " +
			"to take over code dispatch entirely"
	case kindApproval:
		return "configure Options.Human; approval nodes are otherwise resolved engine-side and never " +
			"reach this dispatcher"
	case kindWait:
		return "durable wait dispatch defaults to the timer-backed dispatcher (worker.New wires " +
			"TimerWaitDispatcher when Options.Waiter is nil), so this branch should be unreachable; " +
			"a custom build that cleared the seam after construction must restore it"
	default:
		return "no dispatcher is registered for this kind"
	}
}

// withHeartbeat runs fn while extending the claim's lease.
//
// The heartbeat is what makes a long synchronous dispatch safe: §12.4's lease
// exists so a dead worker's work is reclaimed, and a live worker that stopped
// renewing would have its in-flight work reclaimed underneath it. When the
// renewal is refused — meaning the lease has already gone to someone else —
// the context handed to fn is cancelled, because anything fn produces after
// that point cannot be committed anyway.
func (w *Worker) withHeartbeat(ctx context.Context, claimed postgres.ClaimedWork, fn func(context.Context)) {
	hbCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(w.opts.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
			}
			err := w.db.ExtendLease(hbCtx, claimed.ID, w.opts.WorkerID, claimed.FencingToken, w.opts.LeaseDuration)
			if err == nil {
				continue
			}
			if errors.Is(err, postgres.ErrStaleClaim) {
				w.report(fmt.Errorf("worker: lease on work %s was lost mid-dispatch; abandoning the attempt", claimed.ID))
				cancel()
				return
			}
			// A transient database error is not proof the lease is gone. Keep
			// trying until it is, or until the dispatch ends.
			w.report(fmt.Errorf("worker: heartbeat for work %s: %w", claimed.ID, err))
		}
	}()

	fn(hbCtx)
	cancel()
	<-done
}
