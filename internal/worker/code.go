package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Code-node dispatch through the runner boundary (PRD §9.2, §13.7).
//
// This is the third dispatch path in this package, and it deliberately reads
// like the second one. hooks.go already drives a runners.Runner around an
// agent's own attempt: build a typed Operation from the authored code
// operation, execute it, classify the Result, record a runner_operations row.
// A `code` node is the same motion with two differences, and both are the
// point of the node kind existing at all:
//
//  1. the Result is mapped by runners.BuildCompletion rather than by this
//     package's own three-valued hook verdict, because a code node HAS domain
//     outcomes and a hook does not; and
//  2. the observed evidence rides along inside the completion's own ledger
//     delta rather than being appended beside it, because a code node — and
//     only a code node — may declare `ledger.observe` (internal/compiler's
//     checkLedgerDelta). That declaration is the workflow author saying this
//     node's dispatch earns observed authority, and honouring it through the
//     engine's §12.5 transaction is what makes the evidence commit atomically
//     with the transition it justifies.
//
// # The outcome mapping, and why it is a convention
//
// runners.BuildCompletion needs to know which of a node's declared outcomes
// an exit-0 run routes to, and which (if any) a nonzero exit routes to. The
// workflow schema declares no such mapping today: PRD §11.1's reference code
// node names outcomes `passed`/`failed` and an acceptance requirement of
// `process_exit equals 0`, but nothing in the document says in so many words
// that `passed` is the exit-0 port. (Contrast a post_run hook, where
// `on_failure.outcome` states it outright — see hooks.go.)
//
// Rather than invent schema in a wiring task, the mapping lives here as a
// documented convention (ConventionalCodeOutcomes) that a deployment may
// replace wholesale (Options.CodeOutcomes). What it never does is guess: a
// node whose declared outcomes match neither list is REFUSED with a
// diagnostic and never dispatched, because silently picking one of
// `green`/`red` for exit 0 is precisely how a failing test suite ends up
// routed down the happy edge. Closing the schema gap is recorded as open work
// in docs/acceptance.md.
//
// # Mechanical acceptance
//
// Once dispatchCode's completion has committed, evaluateAcceptance
// (acceptance.go) mechanically checks the node's declared
// `acceptance.requires` — when it declares any — against the same Result
// the evidence above was built from, and appends the verdict as a second,
// derived ledger record. See acceptance.go's own doc for why that is a
// second write rather than folded into the completion above, and what it
// deliberately does not yet do.

// codeOperationKind is the runner_operations.operation_kind value a code
// node's own dispatch is recorded under, distinguishing it from the
// pre_run/post_run rows hooks.go writes.
const codeOperationKind = "code"

// CodeOutcomes names the two domain outcomes a code node's exit status routes
// to. Failure may be empty: a node that declares no domain answer for a
// nonzero exit gets a technical failure instead, which is PRD §3.4's other
// half and is exactly what runners.BuildCompletion does with an empty
// FailureOutcome.
type CodeOutcomes struct {
	Success string
	Failure string
}

// CodeOutcomeResolver maps one code node's declared outcomes onto its
// exit-status ports. It is given the node id (for diagnostics) and the
// outcomes the node's contract declares, sorted.
//
// Returning an error refuses the dispatch: the attempt fails with the
// returned message as its diagnostic and the runner is never invoked.
type CodeOutcomeResolver func(nodeID string, declared []string) (CodeOutcomes, error)

// conventionalSuccessNames and conventionalFailureNames are the outcome names
// ConventionalCodeOutcomes recognises, in preference order. `passed`/`failed`
// lead because they are what PRD §11.1's reference code node declares.
var (
	conventionalSuccessNames = []string{"passed", "succeeded", "completed", "ok"}
	conventionalFailureNames = []string{"failed", "failure"}
)

// ConventionalCodeOutcomes is the default CodeOutcomeResolver: it recognises
// the outcome names PRD §11.1 uses and refuses anything else by name.
//
// A success port is required — without one there is no answer to route for a
// run that worked, and runners.BuildCompletion refuses the contract anyway. A
// failure port is optional, and its absence is a real statement: this node
// treats a nonzero exit as a technical failure to retry, not a domain answer.
func ConventionalCodeOutcomes(nodeID string, declared []string) (CodeOutcomes, error) {
	var out CodeOutcomes
	for _, name := range conventionalSuccessNames {
		if slices.Contains(declared, name) {
			out.Success = name
			break
		}
	}
	for _, name := range conventionalFailureNames {
		if slices.Contains(declared, name) {
			out.Failure = name
			break
		}
	}
	if out.Success == "" {
		return CodeOutcomes{}, fmt.Errorf(
			"code node %q declares outcomes %v, none of which this worker recognises as the outcome an "+
				"exit-0 run routes to (it looks for %s); declare one of those, or configure "+
				"worker.Options.CodeOutcomes with a resolver that knows this workflow's vocabulary — "+
				"guessing which port means success is how a failing run gets routed down the passing edge",
			nodeID, declared, strings.Join(conventionalSuccessNames, ", "))
	}
	return out, nil
}

// codeRunnerActorID is the producer identity the runner's observed evidence
// is attributed to. See Options.CodeRunnerActorID for why it is separable
// from the runner's dispatch name.
func (w *Worker) codeRunnerActorID() string {
	if w.opts.CodeRunnerActorID != "" {
		return w.opts.CodeRunnerActorID
	}
	return w.opts.CodeRunnerName
}

// codeOutcomes resolves node's exit-status ports through the configured
// resolver, defaulting to ConventionalCodeOutcomes.
func (w *Worker) codeOutcomes(node *nodeSpec) (CodeOutcomes, error) {
	resolve := w.opts.CodeOutcomes
	if resolve == nil {
		resolve = ConventionalCodeOutcomes
	}
	return resolve(node.ID, node.Outcomes)
}

// buildCodeOperation lowers a code node's authored operation into the
// runner-neutral document a runners.Runner executes (PRD §13.7).
//
// It is buildHookOperation's sibling and differs only where the two things
// genuinely differ: the operation id is the attempt id itself (a code node's
// dispatch IS the attempt, so the attempt id is its natural idempotency key),
// the timeout comes from the node's own declared policy rather than a fixed
// hook budget, and a declared workspace is mounted copy-on-write — the §13.7
// safe default — rather than read-only, because a code node is the thing that
// is allowed to produce output.
func (w *Worker) buildCodeOperation(node *nodeSpec, dc DispatchContext) (runners.Operation, error) {
	if len(node.Operation) == 0 {
		return runners.Operation{}, fmt.Errorf(
			"node %q is a code node but the pinned definition carries no operation to execute", node.ID)
	}
	var op codeOperationSpec
	if err := json.Unmarshal(node.Operation, &op); err != nil {
		return runners.Operation{}, fmt.Errorf("node %q operation could not be decoded: %w", node.ID, err)
	}

	digest, ok := imageDigest(op.Image)
	if !ok {
		return runners.Operation{}, fmt.Errorf(
			"node %q runs image %q, which is not pinned to a digest (registry/name@sha256:<digest>); "+
				"an unpinned image is not an execution environment the runner boundary can dispatch to",
			node.ID, op.Image)
	}

	timeoutSeconds := int(w.opts.DefaultTimeout.Seconds())
	if node.Timeout > 0 {
		timeoutSeconds = int(node.Timeout.Seconds())
	}

	requiresShell := op.RequiresShell
	captureResourceUsage := true
	operation := runners.Operation{
		OperationID:    dc.AttemptID,
		Runner:         w.opts.CodeRunnerName,
		RunnerRevision: w.opts.CodeRunnerRevision,
		Execution: runners.Execution{
			Kind:        runners.ExecutionContainer,
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
			TimeoutSeconds:     timeoutSeconds,
			Network:            runners.NetworkMode(op.Network),
			AllowedOutputPaths: op.AllowedOutputPaths,
		},
		Evidence: runners.EvidenceRequest{
			CaptureExit:          true,
			CaptureLogs:          true,
			CaptureResourceUsage: &captureResourceUsage,
		},
		Context: &runners.Context{
			RunID:     dc.RunID,
			NodeRunID: dc.NodeRunID,
			AttemptID: dc.AttemptID,
		},
	}
	if operation.Policy.Network == "" {
		// The §13.7 safe default, stated rather than left to the adapter.
		operation.Policy.Network = runners.NetworkNone
	}
	if op.WorkspaceRef != "" {
		operation.Workspace = &runners.Workspace{
			SourceRef: op.WorkspaceRef,
			WriteMode: runners.WriteModeCopyOnWrite,
		}
		operation.Evidence.SnapshotBefore = true
		operation.Evidence.SnapshotAfter = true
	}
	return operation, nil
}

// dispatchCode executes a `code` node through Options.CodeRunner.
//
// Every path ends in exactly one committed completion, matching dispatch()'s
// own contract: a refused dispatch, an unmappable contract, and an executed
// operation all commit something the attempts table can be read for.
func (w *Worker) dispatchCode(
	ctx context.Context, claimed postgres.ClaimedWork, d postgres.Dispatch, node *nodeSpec, dc DispatchContext,
) error {
	// Every completion this function commits — failure sites included —
	// carries w.codeRunnerActorID(), the same producer identity the success
	// path below stamps: a code node's dispatch is the runner's work
	// whichever way it ends, and per-actor surfaces must see the failures
	// too. (When even CodeRunnerActorID/CodeRunnerName are unconfigured the
	// helper yields "" and the attempt stays honestly unattributed.)
	if w.opts.CodeRunnerName == "" {
		return w.failAttempt(ctx, claimed, w.codeRunnerActorID(), engine.StatusFailed, "configuration",
			fmt.Sprintf("node %q is a code node and this worker has a code runner but no CodeRunnerName; "+
				"an operation that names no runner is one no adapter will accept", node.ID))
	}

	// The contract is resolved BEFORE anything is dispatched: a node whose
	// outcomes cannot be mapped must not run at all, because there would be
	// no honest way to route whatever it produced.
	outcomes, err := w.codeOutcomes(node)
	if err != nil {
		return w.failAttempt(ctx, claimed, w.codeRunnerActorID(), engine.StatusFailed, "definition", err.Error())
	}

	operation, err := w.buildCodeOperation(node, dc)
	if err != nil {
		return w.failAttempt(ctx, claimed, w.codeRunnerActorID(), engine.StatusFailed, "configuration", err.Error())
	}

	// Placement is a registry fact (api/runner-protocol). When this node's
	// identity is a runner SERVICE the operation goes over the wire and the
	// work item parks — see runnerasync.go for why holding this lease for the
	// operation's duration would be the wrong cost model. The workflow
	// definition says nothing about which of the two happened, which is the
	// whole point.
	if identity, registryName, ok := w.resolveRunnerService(d.WorkflowKey, node); ok {
		return w.dispatchRunnerService(ctx, claimed, d, node, dc, identity, registryName, operation)
	}
	if w.opts.CodeRunner == nil {
		return w.failAttempt(ctx, claimed, w.codeRunnerActorID(), engine.StatusFailed, "configuration",
			fmt.Sprintf("node %q is a code node and this worker has a runner registry but no identity registered "+
				"for %q (nor for %q), and no in-process code runner to fall back on; "+
				"register the node's execution identity before a run can dispatch it",
				node.ID, runners.NodeKey(d.WorkflowKey, node.ID), node.Uses))
	}

	var (
		res     runners.Result
		execErr error
	)
	// The lease is heartbeated for as long as the operation runs; a container
	// that outlives one lease period must not have its work reclaimed
	// underneath it.
	w.withHeartbeat(ctx, claimed, func(hbCtx context.Context) {
		res, execErr = w.opts.CodeRunner.Execute(hbCtx, operation)
	})

	if execErr != nil {
		return w.completeCodeDispatchError(ctx, claimed, d, node, operation, execErr)
	}

	completion, err := runners.BuildCompletion(res, runners.NodeContract{
		NodeID:         node.ID,
		SuccessOutcome: outcomes.Success,
		FailureOutcome: outcomes.Failure,
		ActorID:        w.codeRunnerActorID(),
		ActorRevision:  w.opts.CodeRunnerRevision,
	})
	if err != nil {
		// The result could not be mapped at all. That is a contract problem,
		// not a claim about what the operation did — and the operation row is
		// still recorded below so the raw Result stays inspectable.
		result, cerr := w.completeTechnicalFailure(ctx, claimed, w.codeRunnerActorID(), engine.StatusContractRejected, "runner",
			fmt.Sprintf("node %q result could not be mapped onto a completion: %v", node.ID, err), nil)
		if cerr != nil {
			return cerr
		}
		w.recordRunnerOperation(ctx, d.NamespaceID, result.AttemptID, codeOperationKind, operation, &res, nil)
		return nil
	}

	result, err := w.complete(ctx, claimed, engine.CompletionRequest{
		TechStatus:     completion.TechStatus,
		Outcome:        completion.Outcome,
		Output:         completion.Output,
		LedgerDelta:    completion.LedgerDelta,
		RunnerManifest: completion.RunnerManifest,
		ActorID:        w.codeRunnerActorID(),
	})
	if err != nil {
		if isStale(err) {
			return nil
		}
		return err
	}
	w.recordRunnerOperation(ctx, d.NamespaceID, result.AttemptID, codeOperationKind, operation, &res, nil)
	w.evaluateAcceptance(ctx, node, res, result)
	return nil
}

// completeCodeDispatchError records a refusal the runner could not turn into
// a Result: no execution happened, or the adapter cannot honestly say whether
// one did (runners.Runner's documented error contract). It is always a
// technical status — there is no domain answer to route — and it never
// appends evidence, because there is nothing measured to be evidence of.
func (w *Worker) completeCodeDispatchError(
	ctx context.Context, claimed postgres.ClaimedWork, d postgres.Dispatch, node *nodeSpec,
	operation runners.Operation, execErr error,
) error {
	status := engine.StatusFailed
	var dispatchErr *runners.DispatchError
	if errors.As(execErr, &dispatchErr) {
		status = dispatchErr.TechStatus()
	}
	result, err := w.completeTechnicalFailure(ctx, claimed, w.codeRunnerActorID(), status, "runner",
		fmt.Sprintf("node %q code dispatch was refused by the runner boundary: %v", node.ID, execErr), nil)
	if err != nil {
		return err
	}
	w.recordRunnerOperation(ctx, d.NamespaceID, result.AttemptID, codeOperationKind, operation, nil, execErr)
	return nil
}
