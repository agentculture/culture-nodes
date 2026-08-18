package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/agentculture/culture-nodes/internal/contracts"
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
// Before dispatchCode commits a completion, evaluateAcceptance
// (acceptance.go) mechanically checks the node's declared
// `acceptance.requires` — when it declares any — against the same Result
// the evidence above was built from, and the node's `acceptance.enforce`
// policy may convert the completion to contract_rejected or re-route it to
// a declared domain outcome (task t17, issue #37). The verdict is then
// appended as a second, derived ledger record after the completion commits.
// See acceptance.go's own doc for the policy semantics, the honest floor
// for unevaluable checks, and why the record is a second write rather than
// folded into the completion's own delta.

// codeOperationKind is the runner_operations.operation_kind value a code
// node's own dispatch is recorded under, distinguishing it from the
// pre_run/post_run rows hooks.go writes.
const codeOperationKind = "code"

// maxCodeInputEnvBytes bounds the canonical JSON this worker will lower into
// a code operation's NODES_INPUT_JSON environment value (issue #170).
//
// The number is not invented: Linux's execve(2) caps a single argv/envp
// element at MAX_ARG_STRLEN, 32 pages — 128 KiB on every page size this
// runs on — and a runner that hands NODES_INPUT_JSON=<value> to a subprocess
// hits that ceiling directly (verified against headspace-cli 0.11.0, whose
// own `--env`/`--env-file` flags document no size limit of their own; the
// kernel is the only real ceiling in the path). This worker refuses well
// below it rather than at it: a resolved input document is graph-handoff
// data, not a bulk payload — a node that needs to move something larger
// belongs on workspaceRef, not an input binding — so a dispatch that would
// have failed opaquely inside the runner subprocess with a bare E2BIG is
// instead refused here, at dispatch, with a diagnostic naming the actual
// byte count.
const maxCodeInputEnvBytes = 64 * 1024

// codeOperationInput canonicalizes a code node's resolved input document for
// NODES_INPUT_JSON, or reports that this dispatch carries nothing worth
// forwarding.
//
// resolveNodeInput (bindings.go) returns the literal `{}` for every node
// that declares no input binding — the overwhelming default across today's
// workflows — and this helper treats that value, like a genuinely absent
// one, as "nothing to forward": that is what keeps every such dispatch
// byte-identical to the operation this worker built before NODES_INPUT_JSON
// existed. A binding that resolves to anything else, including one that
// happens to also resolve to an empty object, is forwarded.
func codeOperationInput(input json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(input)) == 0 {
		return nil, nil
	}
	canonical, err := contracts.CanonicalJSON(input)
	if err != nil {
		return nil, fmt.Errorf("resolved input could not be canonicalized: %w", err)
	}
	if bytes.Equal(canonical, []byte("{}")) {
		return nil, nil
	}
	if len(canonical) > maxCodeInputEnvBytes {
		return nil, fmt.Errorf(
			"resolved input is %d bytes, over the %d-byte limit this worker forwards as %s "+
				"(Linux's execve(2) MAX_ARG_STRLEN bounds a single environment value to 128 KiB; "+
				"move data this size through a workspace ref instead of an input binding)",
			len(canonical), maxCodeInputEnvBytes, runners.EnvInputJSON)
	}
	return canonical, nil
}

// CodeOutcomes names the domain outcomes a code node's exit status routes to.
// Failure may be empty: a node that declares no domain answer for a nonzero
// exit gets a technical failure instead, which is PRD §3.4's other half and is
// exactly what runners.BuildCompletion does with an empty FailureOutcome.
type CodeOutcomes struct {
	Success string
	Failure string
	// ByExitCode routes individual exit codes to individual outcomes, ahead
	// of the pair above. Empty for the two-port convention; the merge gate's
	// three-valued vocabulary (below) is what needs it.
	ByExitCode map[int]string
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

// gateExitCodes is the merge gate's published exit-status contract (task t16,
// issue #101): three domain answers, one exit code each.
//
// # Why a third answer exists at all
//
// A gate that can only say pass/fail has to fold "I could not measure this"
// into one of them, and both foldings are lies with consequences. Folded into
// the passing edge it is the empty-scan false green — a lane with no Go
// toolchain reports nothing and the merge looks verified. Folded into the
// failing edge it manufactures a defect: a repair gets dispatched for a
// threshold nobody measured. `measurement_incomplete` is the answer that is
// neither, and giving it its own edge is what lets a workflow author send it
// somewhere a person looks.
//
// The exit codes are the contract between the gate program and this table.
// They are published here, in scripts/merge-gate.py's own doc, and in
// examples/merge-gate/README.md, and none of the three may drift alone.
var gateExitCodes = map[string]int{
	"gates_passed":           0,
	"changes_required":       1,
	"measurement_incomplete": 2,
}

// gateOutcomeNames is gateExitCodes' key set as a stable, readable list for
// diagnostics.
var gateOutcomeNames = []string{"gates_passed", "changes_required", "measurement_incomplete"}

// ConventionalCodeOutcomes is the default CodeOutcomeResolver: it recognises
// the outcome names PRD §11.1 uses, plus the merge gate's three-valued
// vocabulary, and refuses anything else by name.
//
// A success port is required — without one there is no answer to route for a
// run that worked, and runners.BuildCompletion refuses the contract anyway. A
// failure port is optional, and its absence is a real statement: this node
// treats a nonzero exit as a technical failure to retry, not a domain answer.
func ConventionalCodeOutcomes(nodeID string, declared []string) (CodeOutcomes, error) {
	if gate, isGate, err := gateCodeOutcomes(nodeID, declared); isGate {
		return gate, err
	}

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

// gateCodeOutcomes resolves the gate vocabulary, and reports whether the node
// was asking for it at all.
//
// isGate is true as soon as the node declares ANY of the three names, not only
// when it declares all of them — which is what makes the partial declaration a
// refusal rather than a silent fall-through to the pass/fail pair. A node
// declaring `gates_passed` and `changes_required` but not
// `measurement_incomplete` has no edge for "I could not measure this", so the
// answer would have to be routed as one of the other two. Refusing before
// dispatch is the only honest option: the alternative is a graph that cannot
// express the outcome its own instrument will eventually produce.
//
// Mixing the vocabulary with the pass/fail pair is refused for the neighbouring
// reason: exit 1 would be claimed by both `changes_required` and `failed`, and
// this file's whole discipline is that it never guesses which port means what.
func gateCodeOutcomes(nodeID string, declared []string) (CodeOutcomes, bool, error) {
	present := 0
	for _, name := range declared {
		if _, ok := gateExitCodes[name]; ok {
			present++
		}
	}
	if present == 0 {
		return CodeOutcomes{}, false, nil
	}

	refuse := func(detail string) (CodeOutcomes, bool, error) {
		return CodeOutcomes{}, true, fmt.Errorf(
			"code node %q declares outcomes %v, which this worker reads as the merge-gate vocabulary "+
				"(%s), but %s; declare all three and nothing else, or configure "+
				"worker.Options.CodeOutcomes with a resolver that knows this workflow's vocabulary",
			nodeID, declared, strings.Join(gateOutcomeNames, ", "), detail)
	}

	if present != len(declared) {
		return refuse("it also declares outcomes that are not part of it, leaving no single answer for " +
			"which exit code belongs to which port")
	}
	if present != len(gateExitCodes) {
		missing := make([]string, 0, len(gateExitCodes))
		for _, name := range gateOutcomeNames {
			if !slices.Contains(declared, name) {
				missing = append(missing, name)
			}
		}
		return refuse(fmt.Sprintf(
			"it is missing %s — a gate with no edge for an outcome its instrument can produce would have to "+
				"route that outcome down one of the others", strings.Join(missing, ", ")))
	}

	byExitCode := make(map[int]string, len(gateExitCodes))
	for name, code := range gateExitCodes {
		byExitCode[code] = name
	}
	// Success is the exit-0 port. Failure stays EMPTY on purpose: the gate
	// publishes exactly three exit codes, so a fourth is the instrument
	// crashing, and a crash is a technical failure with no domain answer —
	// never a threshold miss nobody measured.
	return CodeOutcomes{Success: "gates_passed", ByExitCode: byExitCode}, true, nil
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

	// The node's resolved §11.2 input document, lowered into the operation
	// so ContextEnvironment can forward it as NODES_INPUT_JSON alongside the
	// run/node-run/attempt ids below (issue #170). dc.Input has already
	// survived worker.go's own refusal: an unresolvable binding never
	// reaches here at all.
	resolvedInput, err := codeOperationInput(dc.Input)
	if err != nil {
		return runners.Operation{}, fmt.Errorf("node %q %w", node.ID, err)
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
		Input: resolvedInput,
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
		NodeID:           node.ID,
		SuccessOutcome:   outcomes.Success,
		FailureOutcome:   outcomes.Failure,
		ExitCodeOutcomes: outcomes.ByExitCode,
		ActorID:          w.codeRunnerActorID(),
		ActorRevision:    w.opts.CodeRunnerRevision,
	})
	if err != nil {
		// The result could not be mapped at all. That is a contract problem,
		// not a claim about what the operation did — and the operation row is
		// still recorded below so the raw Result stays inspectable.
		result, cerr := w.completeTechnicalFailure(ctx, claimed, w.codeRunnerActorID(), engine.StatusContractRejected, "runner",
			fmt.Sprintf("node %q result could not be mapped onto a completion: %v", node.ID, err), nil, actorTelemetry{})
		if cerr != nil {
			return cerr
		}
		w.recordRunnerOperation(ctx, d.NamespaceID, result.AttemptID, codeOperationKind, operation, &res, nil)
		return nil
	}

	// Task t17 (issue #37): the node's declared acceptance checks are
	// evaluated BEFORE routing, against the same Result the completion's own
	// evidence was built from, and the enforce policy may convert or
	// re-route the completion (see acceptance.go's package doc).
	techStatus, outcome := completion.TechStatus, completion.Outcome
	eval := evaluateAcceptance(node, res)
	if eval != nil {
		techStatus, outcome = eval.apply(techStatus, outcome)
	}

	result, err := w.complete(ctx, claimed, engine.CompletionRequest{
		TechStatus:     techStatus,
		Outcome:        outcome,
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
	w.appendAcceptanceVerdict(ctx, node, eval, result)
	w.evaluateSuccessSignals(ctx, res, result)
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
		fmt.Sprintf("node %q code dispatch was refused by the runner boundary: %v", node.ID, execErr), nil, actorTelemetry{})
	if err != nil {
		return err
	}
	w.recordRunnerOperation(ctx, d.NamespaceID, result.AttemptID, codeOperationKind, operation, nil, execErr)
	return nil
}
