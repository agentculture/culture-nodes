package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Pre-run/post-run code hooks around an agent node's own actor dispatch
// (task t14, spec claim c37, honesty condition h32).
//
// pre_run executes through the runner boundary BEFORE the actor is
// dispatched: a pre-run failure fails the attempt as a technical failure,
// retryable per the node's own policy, and the agent is never invoked. Every
// other dispatch path in this package invokes an actor first and reports on
// what it did; this is the one place that can refuse to invoke it at all.
//
// post_run executes AFTER a synchronous actor returns: exit 0 lets the
// agent's own outcome stand (with the hook's evidence appended alongside
// it); a nonzero exit routes per the node's declared on_failure — a
// declared outcome, or a derived assurance rejection appended alongside the
// agent's own outcome. Neither ever silently passes an unenforced check
// (h32).
//
// A hook's evidence is never folded into engine.CompletionRequest.LedgerDelta
// — see appendHookEvidence below for why: the node's own declared
// ledger.observe permission (PRD §10.7) cannot cover it, because an agent
// node can never declare observe at all (internal/compiler/ledger.go's
// checkLedgerDelta — only a `code` node dispatches through the boundary that
// earns that authority, and a hook is not a code node's own dispatch). A
// hook's evidence is the runner boundary's own observation of dispatching
// around the agent, made by the worker on the engine's behalf, so it is
// appended directly through the ledger, exactly like the derived
// assurance-rejection record it sits alongside.
//
// Async (202) agents and post_run: HandleCallback (internal/actors) commits
// a terminal callback event through engine.CompleteAttempt with no knowledge
// of a workflow's compiled node definitions, hook operations, or the
// internal/runners seam a hook dispatches through — it is deliberately a
// small, protocol-only package (see its own doc comment), and today it is
// not even wired into the running API server (internal/api/server.go mounts
// no callback handler yet; only tests construct one directly). Threading
// hook execution into that path would mean either growing CallbackDeps into
// something IR- and runner-aware, or duplicating the worker's IR/spec
// loading inside a package that currently owns none of it — both real,
// cross-cutting changes well past this slice. Rather than ship the
// alternative — a node run that completes from a callback with post_run
// having silently never run — an agent node that declares post_run and
// answers asynchronously is refused at the point the worker would otherwise
// park it (refuseAsyncPostRun below), with a diagnostic that says exactly
// why. This is not a compile-time refusal: whether a given actor answers
// synchronously or asynchronously is the actor's own runtime choice (PRD
// §9.5's "supported sync/async behavior"), invisible to the compiler, so
// "at the earliest point the split is knowable" is dispatch time, not
// authoring time.

// The hook kinds. Each doubles as the runner_operations.operation_kind value
// and the diagnostic class a failed hook is recorded under.
const (
	hookKindPreRun  = "pre_run"
	hookKindPostRun = "post_run"
)

// DefaultHookTimeoutSeconds bounds a hook operation's execution. A hook
// carries no policy override of its own in this schema slice — task t14
// reuses a code node's operation shape verbatim (schemas/workflow's
// #/$defs/codeOperation) and adds none — so every hook runs under one fixed
// budget rather than inheriting the agent node's own dispatch timeout: the
// two are different budgets for different things (see
// internal/compiler/policy.go's checkHookOperations for the compile-time
// half of this reasoning). It is comfortably under RunnerMaxTimeoutSeconds.
const DefaultHookTimeoutSeconds = 300

// hookRun is what executing one hook produced: the operation that was built
// (even when dispatch never actually happened, so it can still be recorded
// as the runner_operations request), the runner's Result when one was
// produced, and the observed evidence built from it.
type hookRun struct {
	operation runners.Operation
	result    *runners.Result
	record    *ledger.Record
	manifest  *ledger.RunnerManifest
	// dispatchErr is set when Execute refused the operation or could not
	// honestly report on it — no Result was produced at all (see
	// runners.Runner's documented error contract).
	dispatchErr error
}

// hookVerdict is what a hook's execution proved.
type hookVerdict int

const (
	// hookPassed: the operation ran to completion and exited zero.
	hookPassed hookVerdict = iota
	// hookCheckFailed: the operation ran to completion and exited nonzero —
	// an honest verdict the hook is entitled to report.
	hookCheckFailed
	// hookUntrustworthy: the operation did not produce an honest pass/fail —
	// no exit code, a timeout, a cancellation, or a policy/contract refusal.
	// Treating this as a pass, or as a reported failure the operation never
	// actually asserted, would be exactly the fabrication the runner
	// boundary exists to prevent — so it is always a technical failure of
	// the attempt, never routed through post_run's on_failure.
	hookUntrustworthy
)

func classifyHookResult(res runners.Result) hookVerdict {
	if res.State != runners.StateCompleted {
		return hookUntrustworthy
	}
	code, ok := res.ExitCode()
	if !ok {
		return hookUntrustworthy
	}
	if code == 0 {
		return hookPassed
	}
	return hookCheckFailed
}

// imageDigest extracts the pinned digest from a "registry/name@sha256:<hex>"
// reference — the same pin internal/compiler's checkOperationPolicy already
// warns about at compile time when it is missing. A hook operation without
// one cannot be dispatched: pinning is what makes "the runner boundary
// executes only immutable content" true at runtime, not merely at authoring
// time.
func imageDigest(image string) (string, bool) {
	_, digest, ok := strings.Cut(image, "@")
	if !ok || !strings.HasPrefix(digest, "sha256:") {
		return "", false
	}
	return digest, true
}

// buildHookOperation lowers a hook's authored operation into the
// runner-neutral document internal/runners.Runner executes (PRD §13.7).
func (w *Worker) buildHookOperation(kind string, op codeOperationSpec, dc DispatchContext) (runners.Operation, error) {
	digest, ok := imageDigest(op.Image)
	if !ok {
		return runners.Operation{}, fmt.Errorf(
			"%s hook image %q is not pinned to a digest (registry/name@sha256:<digest>); "+
				"an unpinned image is not an execution environment the runner boundary can dispatch to",
			kind, op.Image)
	}

	requiresShell := op.RequiresShell
	operation := runners.Operation{
		OperationID:    dc.AttemptID + ":" + kind,
		Runner:         w.opts.HookRunnerName,
		RunnerRevision: w.opts.HookRunnerRevision,
		Execution: runners.Execution{
			Kind:        runners.ExecutionFunction,
			ImageRef:    op.Image,
			ImageDigest: digest,
		},
		Command: runners.Command{
			Argv:             op.Argv,
			WorkingDirectory: op.WorkingDirectory,
			EnvironmentRefs:  op.EnvironmentRefs,
			RequiresShell:    &requiresShell,
		},
		Policy: runners.Policy{
			TimeoutSeconds:     DefaultHookTimeoutSeconds,
			Network:            runners.NetworkMode(op.Network),
			AllowedOutputPaths: op.AllowedOutputPaths,
		},
		// SnapshotBefore/SnapshotAfter make every hook dispatch a standing
		// request for the runner boundary's own workspace comparison (task
		// t12, spec claim c15, honesty condition h10) — the standard,
		// reusable post_run "workspace-snapshot" pattern is exactly this: no
		// special-cased operation kind, just a hook operation whose runner
		// honours the request. A runner that cannot compare the workspace
		// says so honestly in Observations.ChangedPaths (headspace-cli
		// 0.11.0 and the Lambda adapter both always report it unmeasured
		// today — see their own package docs) rather than this call site
		// having to know which runners can. buildEvidence (internal/runners/
		// dispatch.go) surfaces changed_paths, snapshot_digest, and
		// artifact_refs into the evidence record only when that observation
		// says measured, so a runner that gains snapshot support starts
		// producing this evidence through this exact seam with no worker
		// change at all.
		Evidence: runners.EvidenceRequest{
			CaptureExit:    true,
			CaptureLogs:    true,
			SnapshotBefore: true,
			SnapshotAfter:  true,
		},
		Context: &runners.Context{
			RunID:     dc.RunID,
			NodeRunID: dc.NodeRunID,
			AttemptID: dc.AttemptID,
		},
	}
	if op.WorkspaceRef != "" {
		operation.Workspace = &runners.Workspace{
			SourceRef: op.WorkspaceRef,
			WriteMode: runners.WriteModeReadOnly,
		}
	}
	return operation, nil
}

// executeHook builds and dispatches one hook operation through the
// configured HookRunner (internal/runners.Runner), and returns everything a
// caller needs to decide what happened and to record it.
func (w *Worker) executeHook(ctx context.Context, kind string, op codeOperationSpec, dc DispatchContext) hookRun {
	if w.opts.HookRunner == nil {
		return hookRun{dispatchErr: fmt.Errorf(
			"this worker has no hook runner configured, so it cannot execute the %s hook", kind)}
	}
	if w.opts.HookRunnerName == "" {
		return hookRun{dispatchErr: fmt.Errorf(
			"this worker has no hook runner name configured, so it cannot name a runner on the %s hook's operation", kind)}
	}

	operation, err := w.buildHookOperation(kind, op, dc)
	if err != nil {
		return hookRun{dispatchErr: err}
	}

	res, err := w.opts.HookRunner.Execute(ctx, operation)
	if err != nil {
		return hookRun{operation: operation, dispatchErr: err}
	}

	run := hookRun{operation: operation, result: &res}
	if record, manifest, ok := w.buildHookEvidence(res); ok {
		run.record = &record
		run.manifest = &manifest
	}
	return run
}

// hookEvidenceSuccessSentinel is the SuccessOutcome runners.BuildCompletion
// requires. It is never read: a hook has no domain outcome of its own (a
// post-run hook's failure routing is on_failure, authored separately from
// the operation) — hooks are classified by classifyHookResult, not by
// BuildCompletion's outcome mapping. Only its evidence-record construction
// is reused here.
const hookEvidenceSuccessSentinel = "hook_ran"

// buildHookEvidence turns a hook's Result into one observed evidence record
// plus the manifest that authorizes it, reusing internal/runners'
// evidence-building logic (BuildCompletion) rather than duplicating it. ok is
// false only when the mapping genuinely produced nothing to record.
func (w *Worker) buildHookEvidence(res runners.Result) (ledger.Record, ledger.RunnerManifest, bool) {
	completion, err := runners.BuildCompletion(res, runners.NodeContract{
		NodeID:         res.OperationID,
		SuccessOutcome: hookEvidenceSuccessSentinel,
		ActorID:        w.opts.HookRunnerName,
		ActorRevision:  w.opts.HookRunnerRevision,
	})
	if err != nil || len(completion.LedgerDelta) == 0 {
		return ledger.Record{}, ledger.RunnerManifest{}, false
	}
	manifest := ledger.RunnerManifest{}
	if completion.RunnerManifest != nil {
		manifest = *completion.RunnerManifest
	}
	return completion.LedgerDelta[0], manifest, true
}

// runPreRunHook executes node's pre_run hook (h32's first clause).
//
// proceed is false exactly when the attempt has already been completed (or
// a completion was attempted and turned out stale) — the caller must return
// nil immediately without ever invoking the actor. proceed is true only when
// the hook passed, in which case run is what the eventual final completion
// must append evidence for.
func (w *Worker) runPreRunHook(
	ctx context.Context, claimed postgres.ClaimedWork, d postgres.Dispatch, node *nodeSpec, dc DispatchContext,
) (proceed bool, run *hookRun, err error) {
	executed := w.executeHook(ctx, hookKindPreRun, node.PreRun.Operation, dc)

	verdict := hookUntrustworthy
	if executed.dispatchErr == nil && executed.result != nil {
		verdict = classifyHookResult(*executed.result)
	}

	if executed.dispatchErr == nil && verdict == hookPassed {
		return true, &executed, nil
	}

	detail := fmt.Sprintf("node %q pre_run hook did not pass", dc.NodeID)
	switch {
	case executed.dispatchErr != nil:
		detail = fmt.Sprintf("node %q pre_run hook could not be dispatched: %v", dc.NodeID, executed.dispatchErr)
	case verdict == hookCheckFailed:
		code, _ := executed.result.ExitCode()
		detail = fmt.Sprintf("node %q pre_run hook exited %d; the agent is not invoked", dc.NodeID, code)
	case executed.result != nil:
		detail = fmt.Sprintf("node %q pre_run hook ended %s without an honest pass/fail verdict; the agent is not invoked",
			dc.NodeID, executed.result.State)
	}

	// A pre-run hook executes BEFORE Registry.Resolve (dispatchActor), so
	// this failure predates any actor resolution and stays unattributed.
	completion, cerr := w.completeTechnicalFailure(ctx, claimed, "", engine.StatusFailed, hookKindPreRun, detail, nil, actorTelemetry{})
	if cerr != nil {
		return false, nil, cerr
	}
	w.appendHookEvidence(ctx, completion, &executed)
	w.recordHookOperations(ctx, d.NamespaceID, completion.AttemptID, &executed, nil)
	return false, nil, nil
}

// postRunOutcome is what running node's post_run hook decided.
type postRunOutcome struct {
	// trustworthy is false when the hook itself could not be dispatched or
	// did not produce an honest pass/fail verdict (hookUntrustworthy) — an
	// attempt-level technical failure, never routed through on_failure.
	trustworthy bool
	detail      string
	// passed is only meaningful when trustworthy is true.
	passed bool
	run    hookRun
}

// runPostRunHook executes node's post_run hook and classifies the result.
// It does not decide what happens next — routing on_failure, or letting the
// agent's own outcome stand — that is completeFromResult's call, because it
// also has to know what the agent itself reported.
func (w *Worker) runPostRunHook(ctx context.Context, node *nodeSpec, dc DispatchContext) postRunOutcome {
	run := w.executeHook(ctx, hookKindPostRun, node.PostRun.Operation, dc)
	out := postRunOutcome{run: run}

	if run.dispatchErr != nil {
		out.detail = fmt.Sprintf("node %q post_run hook could not be dispatched: %v", dc.NodeID, run.dispatchErr)
		return out
	}

	switch classifyHookResult(*run.result) {
	case hookPassed:
		out.trustworthy = true
		out.passed = true
	case hookCheckFailed:
		out.trustworthy = true
		out.passed = false
	default:
		out.detail = fmt.Sprintf("node %q post_run hook ended %s without an honest pass/fail verdict",
			dc.NodeID, run.result.State)
	}
	return out
}

// refuseAsyncPostRun is the async+post_run boundary (see this file's package
// doc): an agent node that declares post_run and answers 202 is refused
// rather than parked, because this build has no way to run post_run against
// a result that arrives later through the callback path.
func (w *Worker) refuseAsyncPostRun(
	ctx context.Context, claimed postgres.ClaimedWork, d postgres.Dispatch, node *nodeSpec, dc DispatchContext, preRun *hookRun,
) error {
	completion, err := w.completeTechnicalFailure(ctx, claimed, dc.ActorRowID, engine.StatusContractRejected, hookKindPostRun,
		fmt.Sprintf(
			"node %q declares post_run and the actor answered asynchronously (202); this build cannot run a "+
				"post-run hook against a callback-delivered result, so the async acceptance is refused rather than "+
				"silently skipping the hook",
			dc.NodeID), nil, actorTelemetry{})
	if err != nil {
		return err
	}
	w.appendHookEvidence(ctx, completion, preRun)
	w.recordHookOperations(ctx, d.NamespaceID, completion.AttemptID, preRun, nil)
	return nil
}

// appendHookEvidence appends one hook's observed evidence directly to the
// ledger, stamped against the attempt the main completion just committed. It
// returns the stored record (with its assigned id) and whether one was
// appended at all — run may be nil, or may carry no evidence (a dispatch
// refusal produced none to report).
//
// This is deliberately NOT folded into engine.CompletionRequest.LedgerDelta:
// see this file's package doc for why the node-declared ledger.observe gate
// can never cover it. A failure to append is reported through
// Options.OnError, never allowed to unwind the already-committed completion.
func (w *Worker) appendHookEvidence(ctx context.Context, completion engine.CompletionResult, run *hookRun) (ledger.Record, bool) {
	if run == nil || run.record == nil || completion.AttemptID == "" {
		return ledger.Record{}, false
	}

	record := *run.record
	record.RunID = completion.RunID
	record.NodeRunID = ledger.NullableID(completion.NodeRunID)
	record.AttemptID = ledger.NullableID(completion.AttemptID)

	var opts []ledger.AppendOption
	if run.manifest != nil {
		opts = append(opts, ledger.WithRunnerManifest(*run.manifest))
	}
	appended, err := w.ledger.Append(ctx, record, opts...)
	if err != nil {
		w.report(fmt.Errorf("worker: append hook evidence for attempt %s: %w", completion.AttemptID, err))
		return ledger.Record{}, false
	}
	return appended, true
}

// recordHookOperations stores runner_operations rows for whichever hooks ran
// on this attempt, keyed to the attempt they ran against. It is best-effort
// observability, called only after a completion has actually committed
// (attemptID != ""): a failure to record it is reported through
// Options.OnError, never allowed to turn an already-committed completion
// into an error the worker retries forever.
func (w *Worker) recordHookOperations(ctx context.Context, namespaceID, attemptID string, preRun, postRun *hookRun) {
	if attemptID == "" {
		// A stale completion: nothing was committed here, so there is
		// nothing to key a runner_operations row to. Whoever holds the
		// claim now dispatched (or will dispatch) their own hook runs.
		return
	}
	if preRun != nil {
		w.recordHookOperation(ctx, namespaceID, attemptID, hookKindPreRun, *preRun)
	}
	if postRun != nil {
		w.recordHookOperation(ctx, namespaceID, attemptID, hookKindPostRun, *postRun)
	}
}

func (w *Worker) recordHookOperation(ctx context.Context, namespaceID, attemptID, kind string, run hookRun) {
	w.recordRunnerOperation(ctx, namespaceID, attemptID, kind, run.operation, run.result, run.dispatchErr)
}

// recordRunnerOperation stores one runner_operations row for any dispatch
// through the runner boundary — a pre_run/post_run hook (kind "pre_run" /
// "post_run") or a code node's own operation (kind "code", see code.go).
//
// It is shared rather than duplicated because the row means the same thing in
// both cases: this exact typed operation was sent to a runner, under this
// policy digest, and this is what came back. attemptID keys it to the attempt
// the completion committed; an empty one means nothing committed and there is
// nothing to key a row to.
func (w *Worker) recordRunnerOperation(
	ctx context.Context, namespaceID, attemptID, kind string,
	operation runners.Operation, result *runners.Result, dispatchErr error,
) {
	if attemptID == "" || operation.OperationID == "" {
		// Either the completion turned out stale (nothing committed), or the
		// dispatch never reached a real operation at all (no runner
		// configured, no runner name, an unpinned image) — a local
		// configuration refusal, not a runner operation. The diagnostic on
		// the attempt's own output already names it.
		return
	}

	request, err := json.Marshal(operation)
	if err != nil {
		w.report(fmt.Errorf("worker: encode %s operation for runner_operations: %w", kind, err))
		return
	}
	policyDigest, err := contracts.DigestValue(operation)
	if err != nil {
		w.report(fmt.Errorf("worker: digest %s operation for runner_operations: %w", kind, err))
		return
	}

	in := postgres.InsertRunnerOperationInput{
		ID:            "runnerop_" + idstore.NewULID(),
		NamespaceID:   namespaceID,
		AttemptID:     attemptID,
		OperationKind: kind,
		PolicyDigest:  policyDigest,
		Request:       request,
	}
	switch {
	case dispatchErr != nil:
		in.Status = "dispatch_failed"
	case result != nil:
		encoded, err := json.Marshal(result)
		if err != nil {
			w.report(fmt.Errorf("worker: encode %s result for runner_operations: %w", kind, err))
			return
		}
		in.Result = encoded
		in.Status = string(result.State)
		in.CompletedAt = result.Timing.FinishedAt
	default:
		in.Status = "unknown"
	}

	if _, err := w.db.InsertRunnerOperation(ctx, in); err != nil {
		w.report(fmt.Errorf("worker: record %s operation for attempt %s: %w", kind, attemptID, err))
	}
}

// appendAssuranceRejection is post_run's reject_assurance path (h32's second
// clause): the agent's own domain outcome still stands — it is not
// overwritten or hidden — but a derived record disputing it is appended so
// the rejection is never silent. It is origin validator, authority derived:
// PRD §10.4 lets a deterministic producer create derived records, which is
// exactly what this is — a mechanical consequence of the hook's own exit
// code, not a human decision reachable only through a review transaction.
// subjectID is the post_run hook's own evidence record id (as returned by
// appendHookEvidence), empty when the hook produced no evidence to point at.
//
// This is deliberately a second, non-transactional write after the
// completion that already committed: engine.CompleteAttempt's own delta
// preparation refuses a derived-authority record outright (prepareRecord's
// default case — "derived belongs to deterministic producers" is the
// *ledger runtime's* province, not a worker-supplied completion delta), so a
// derived record can only be appended through the ledger directly. A failure
// here is reported through Options.OnError, never allowed to unwind the
// agent's already-committed outcome.
func (w *Worker) appendAssuranceRejection(ctx context.Context, dc DispatchContext, completion engine.CompletionResult, postRun *hookRun, subjectID string) {
	detail := map[string]any{
		"verdict":      "reject",
		"reason":       fmt.Sprintf("node %q post_run hook reported a failing check against outcome %q", dc.NodeID, completion.Outcome),
		"hook":         hookKindPostRun,
		"node_outcome": completion.Outcome,
	}
	if postRun != nil && postRun.result != nil {
		if code, ok := postRun.result.ExitCode(); ok {
			detail["hook_exit_code"] = code
		}
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		w.report(fmt.Errorf("worker: encode assurance rejection payload for attempt %s: %w", completion.AttemptID, err))
		return
	}

	var provenance []string
	if subjectID != "" {
		provenance = []string{subjectID}
	}

	record := ledger.Record{
		RecordType: ledger.RecordReview,
		RunID:      completion.RunID,
		NodeRunID:  ledger.NullableID(completion.NodeRunID),
		AttemptID:  ledger.NullableID(completion.AttemptID),
		Origin: ledger.Origin{
			Kind:    ledger.OriginValidator,
			ActorID: w.opts.HookRunnerName,
		},
		Authority:      ledger.AuthorityDerived,
		SubjectRef:     ledger.NullableID(subjectID),
		Data:           payload,
		ProvenanceRefs: provenance,
	}
	if _, err := w.ledger.Append(ctx, record); err != nil {
		w.report(fmt.Errorf("worker: append assurance rejection for attempt %s: %w", completion.AttemptID, err))
	}
}
