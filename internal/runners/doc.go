// Package runners is the code-execution boundary: the control plane
// describes a typed Operation, a Runner executes it behind an enforced
// policy, and a structured Result comes back carrying — per observation —
// whether the runner directly measured the fact and whether the measurement
// covered the whole declared scope.
//
// Nothing here executes code. There is no shell, no script, no Docker
// socket, and no exec of any kind in this package or its adapters' control
// paths (PRD §13.7, §16.4). A wrapper that checks policy and then hands an
// unrestricted shell to another process does not satisfy the boundary, so
// the boundary is the Runner interface and the only thing that crosses it is
// a document.
//
// # Layers
//
//   - Operation and Result are Go mirrors of schemas/runner/{operation,
//     result}.schema.json. The schemas are canonical; these structs exist so
//     Go code can build and read those documents without hand-rolling maps.
//     schema_test.go proves the mirror is lossless in both directions.
//   - Runner is the one-method seam every adapter implements.
//   - FunctionRegistry is the registry-pinned dispatch allowlist (spec claim
//     c41): an adapter resolves the operation's declared identity here, and a
//     name that was never registered is refused before any call leaves the
//     process.
//   - RenderWorkerIAMPolicy turns that same registry into the worker role's
//     IAM policy, so "the worker may invoke only registered functions" is one
//     fact with two renderings rather than two lists that can drift.
//   - BuildCompletion is the worker seam: Result in, engine completion out
//     (technical status, domain outcome, output, ledger delta, and the
//     manifest that authorizes the delta).
//
// # Adapters
//
// internal/runners/lambda is the first adapter and the only package here
// allowed to import an AWS SDK (spec claim c17 / task t17's depguard rule).
// PRD §13.7 names headspace-cli as the reference runner pattern; §19.2
// explicitly allows a non-Docker adapter that implements the same typed
// operation and evidence contract, which is what the Lambda adapter is
// (spec claim c25 — a recorded, deliberate deviation from §13.7's
// headspace-first ordering, not silent drift).
//
// # Honesty
//
// Isolation is not truth. A Result is trustworthy because the operation was
// typed, the policy enforced, the inputs immutable, and each observation
// attributed. An adapter that cannot observe a fact says so
// ({measured:false, complete:false}) — it never fabricates the value and
// never omits the declaration to imply completeness.
package runners
