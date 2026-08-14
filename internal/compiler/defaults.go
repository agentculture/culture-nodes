package compiler

// Compiler defaults and enforced caps.
//
// The PRD fixes some of these outright (safe network default, workspace paths,
// the ledger schema version, provenance) and leaves others — the loop bounds,
// the per-node timeout — to the implementation. The invented numbers are named
// constants rather than literals so a later ADR can move them in one place and
// so a diagnostic can quote them.
const (
	// DefaultMaxDuration bounds a run's wall clock (PRD §9.7).
	DefaultMaxDuration = "1h"
	// DefaultMaxTransitions bounds total edges followed in one run.
	DefaultMaxTransitions = 64
	// DefaultMaxVisitsPerNode bounds how often one node runs in one run.
	DefaultMaxVisitsPerNode = 8
	// DefaultMaxParallelTokens is 1 so parallelism stays opt-in (PRD §9.8):
	// the engine honors splits and joins (issue #43), but a workflow that
	// never declares the limit gets a cap of one active token, which makes
	// any split in it a bound failure at runtime. Adding a `parallel` node
	// must not be enough to acquire concurrency the author never asked for.
	DefaultMaxParallelTokens = 1

	// DefaultLedgerSchemaVersion pins the work-ledger vocabulary a workflow
	// speaks when it does not say.
	DefaultLedgerSchemaVersion = "nodes.culture.dev/ledger/v1alpha1"
	// DefaultMaxRecordsPerNode bounds ledger writes per node run.
	DefaultMaxRecordsPerNode = 100

	// DefaultNodeTimeout applies to the kinds that dispatch work.
	DefaultNodeTimeout = "15m"
	// DefaultApprovalDeadline bounds an unanswered human task (PRD §9.9).
	DefaultApprovalDeadline = "24h"
	// DefaultRetryMaxAttempts is 1 — one attempt, no retry. Retrying by
	// default would silently re-dispatch work that may not be idempotent.
	DefaultRetryMaxAttempts = 1
	// DefaultRetryBackoffNone / Exponential: a node that asked for more than
	// one attempt but no backoff gets exponential, since a tight retry loop
	// against a rate-limited actor is never what the author meant.
	DefaultRetryBackoffNone        = "none"
	DefaultRetryBackoffExponential = "exponential"

	// DefaultNetwork is the safe default for a code operation (PRD §13.7).
	DefaultNetwork = "none"
	// DefaultWorkingDirectory and DefaultAllowedOutputPath mirror the §13.7
	// reference operation.
	DefaultWorkingDirectory  = "/workspace"
	DefaultAllowedOutputPath = "/workspace"
)

// Enforced caps and sanity bounds.
const (
	// RunnerLimitSource names where a cap comes from, so a diagnostic says
	// "this is the runner's limit", not "this is the compiler's opinion".
	// The MVP code runner is an AWS Lambda function invoking a pinned
	// container image (build-plan task t13).
	RunnerLimitSource = "runner: lambda"
	// RunnerMaxTimeoutSeconds is Lambda's maximum function timeout.
	RunnerMaxTimeoutSeconds = 900
	// RunnerMaxPayloadBytes is Lambda's synchronous request/response payload
	// limit (6 MiB). Larger payloads must travel as artifact references.
	RunnerMaxPayloadBytes = 6 * 1024 * 1024
	// MaxRetryAttempts bounds a node's retry policy. Beyond this, a retry
	// policy is a denial-of-service on the actor, not resilience.
	MaxRetryAttempts = 10
)

// intPtr / boolPtr build the pointers the optional-field model uses.
func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

// dispatchesWork reports whether a node kind sends work to an actor or runner,
// and therefore needs a timeout and a retry policy in the normalized IR.
func dispatchesWork(kind string) bool {
	switch kind {
	case KindAgent, KindCode, KindActionHTTP:
		return true
	default:
		return false
	}
}

// effectiveTimeout returns the timeout the runtime would apply to a node:
// authored if present, the default otherwise, empty for kinds that dispatch
// no work. Policy checks use it so an omitted timeout is checked against the
// same value the engine will actually enforce.
func effectiveTimeout(n *node) string {
	if n.Policy != nil && n.Policy.Timeout != "" {
		return n.Policy.Timeout
	}
	if dispatchesWork(n.Kind) {
		return DefaultNodeTimeout
	}
	return ""
}
