package runners

import "fmt"

// Mechanical acceptance evaluation (PRD §10.10; docs/acceptance.md
// criterion 14).
//
// A node's `acceptance.requires` block names checks the compiler validates
// only by kind (internal/compiler/ledger.go's checkAcceptance warns on an
// unrecognised one but never rejects it) — the schema itself says so:
// "Check payloads stay open until the check registry lands with the
// compiler; only `kind` is required"
// (schemas/workflow/workflow.schema.json's #/$defs/acceptance). This file is
// that registry's first slice: the check kinds this runtime can evaluate
// against the same runners.Result a code node's own observed evidence is
// built from (dispatch.go's buildEvidence), so a check reads the identical
// honest facts the evidence record does — never a second, looser copy of
// them assembled some other way.
//
// Two kinds are mechanically evaluable today: process_exit and
// workspace_diff, because both have a direct, already-modeled field on
// Result (Exit.Code and Changes.Complete) alongside an Observations entry
// that says whether the runner actually measured it. The other seven kinds
// PRD §10.10 and internal/compiler/vocabulary.go's acceptanceKinds name
// (schema_valid, artifact_digest, dependency_delta, parity_fixtures,
// changed_paths_within_policy, claims_confirmed, no_blocking_questions) need
// inputs this package does not have to hand (a schema ref, a second digest
// to compare against, ledger projections, ...) and are deliberately left
// unevaluated rather than guessed at — see evaluateOne's default case.
//
// # Never a fabricated pass
//
// A requirement this package cannot evaluate is reported Evaluated: false,
// Passed: false — an unevaluated requirement is not a met one. The same
// discipline applies to a requirement whose kind IS known: process_exit and
// workspace_diff both refuse to answer at all when the underlying
// Observation says the fact was not directly measured, rather than trusting
// a self-reported value the runner boundary itself would not treat as
// evidence (see internal/ledger's producer/authority matrix — "container
// output cannot grant itself observed authority", docs/acceptance.md
// criterion 13). mapStatus in dispatch.go routes a node's own domain outcome
// on the exit code alone, unmeasured or not, because a function's own report
// of what it did is still a real domain answer to route; acceptance is a
// different question — whether that answer is honestly *verified* — and
// answering it from a fact nobody measured would be exactly the fabrication
// the runner boundary exists to prevent.
//
// # Never changes routing
//
// Evaluating requires never changes a node's technical status or domain
// outcome. PRD's own ground rule keeps those two concerns separate from
// ledger assurance (§10.6): the worker records a mechanical verdict as a
// derived ledger record alongside the evidence it verifies (see
// internal/worker/acceptance.go), and nothing about the graph's routing
// reads it.

// AcceptanceCheckKind names one requirement kind this package knows how to
// evaluate mechanically.
type AcceptanceCheckKind string

const (
	// CheckProcessExit compares the operation's reported exit code against
	// requirement["equals"], and only when Observations.ExitStatus.Measured
	// is true.
	CheckProcessExit AcceptanceCheckKind = "process_exit"
	// CheckWorkspaceDiff compares the operation's Changes.Complete flag
	// against requirement["complete"], and only when
	// Observations.ChangedPaths.Measured is true.
	CheckWorkspaceDiff AcceptanceCheckKind = "workspace_diff"
)

// MechanicallyEvaluableChecks is the set of acceptance.requires kinds this
// package can evaluate today. It is a strict subset of
// internal/compiler/vocabulary.go's acceptanceKinds; the two lists are kept
// in sync by hand (see this file's package doc for why they cannot import
// one another) and this package's own test asserts the set against it by
// value.
var MechanicallyEvaluableChecks = map[AcceptanceCheckKind]bool{
	CheckProcessExit:   true,
	CheckWorkspaceDiff: true,
}

// AcceptanceCheckResult is one requirement's mechanical verdict.
type AcceptanceCheckResult struct {
	// Kind is the requirement's own declared kind, copied verbatim (even
	// when empty or unrecognised) so a reader can see exactly what was
	// asked for.
	Kind string `json:"kind"`
	// Evaluated is false when this package could not mechanically answer
	// the requirement at all — an unrecognised kind, a malformed payload,
	// or an honestly unmeasured fact. Passed is always false when Evaluated
	// is false: an unevaluated requirement is not a met one.
	Evaluated bool `json:"evaluated"`
	Passed    bool `json:"passed"`
	// Reason states, in one sentence, why: what was compared, or why it
	// could not be.
	Reason string `json:"reason"`
}

// AcceptanceVerdict is the mechanical outcome of a node's whole
// acceptance.requires list against one runner Result.
type AcceptanceVerdict struct {
	// Passed is true only when requires is non-empty and every requirement
	// in it was both evaluated and passed — or when requires is empty,
	// which is vacuously satisfied.
	Passed bool                    `json:"passed"`
	Checks []AcceptanceCheckResult `json:"checks"`
}

// EvaluateAcceptance mechanically evaluates requires against res. It is a
// pure function: the same requires and res always produce the same
// AcceptanceVerdict, which is what lets a caller record the verdict as a
// deterministic derived fact rather than a live judgment call.
func EvaluateAcceptance(requires []map[string]any, res Result) AcceptanceVerdict {
	verdict := AcceptanceVerdict{Passed: true, Checks: make([]AcceptanceCheckResult, 0, len(requires))}
	for _, requirement := range requires {
		result := evaluateOne(requirement, res)
		verdict.Checks = append(verdict.Checks, result)
		if !result.Evaluated || !result.Passed {
			verdict.Passed = false
		}
	}
	return verdict
}

func evaluateOne(requirement map[string]any, res Result) AcceptanceCheckResult {
	kind, _ := requirement["kind"].(string)
	switch AcceptanceCheckKind(kind) {
	case CheckProcessExit:
		return evaluateProcessExit(requirement, res)
	case CheckWorkspaceDiff:
		return evaluateWorkspaceDiff(requirement, res)
	default:
		return AcceptanceCheckResult{
			Kind:      kind,
			Evaluated: false,
			Passed:    false,
			Reason:    fmt.Sprintf("this build has no mechanical evaluator for acceptance check kind %q yet", kind),
		}
	}
}

func evaluateProcessExit(requirement map[string]any, res Result) AcceptanceCheckResult {
	kind := string(CheckProcessExit)

	// JSON numbers decode as float64 through encoding/json's map[string]any
	// path, which is how this payload always arrives (the compiled IR is
	// JSON, decoded generically — see internal/worker/ir.go's acceptanceSpec).
	wantFloat, ok := requirement["equals"].(float64)
	if !ok {
		return AcceptanceCheckResult{Kind: kind, Evaluated: false, Passed: false,
			Reason: `process_exit requires an "equals" number naming the expected exit code; none was declared`}
	}
	want := int(wantFloat)

	if !res.Observations.ExitStatus.Measured {
		return AcceptanceCheckResult{Kind: kind, Evaluated: false, Passed: false,
			Reason: "the operation's exit status observation says it was not directly measured; " +
				"a self-reported exit code is not evidence this check may verify against"}
	}
	code, ok := res.ExitCode()
	if !ok {
		return AcceptanceCheckResult{Kind: kind, Evaluated: false, Passed: false,
			Reason: "the operation declared its exit status measured but reported no exit code"}
	}
	if code == want {
		return AcceptanceCheckResult{Kind: kind, Evaluated: true, Passed: true,
			Reason: fmt.Sprintf("the measured exit code %d equals the required %d", code, want)}
	}
	return AcceptanceCheckResult{Kind: kind, Evaluated: true, Passed: false,
		Reason: fmt.Sprintf("the measured exit code %d does not equal the required %d", code, want)}
}

func evaluateWorkspaceDiff(requirement map[string]any, res Result) AcceptanceCheckResult {
	kind := string(CheckWorkspaceDiff)

	want, ok := requirement["complete"].(bool)
	if !ok {
		return AcceptanceCheckResult{Kind: kind, Evaluated: false, Passed: false,
			Reason: `workspace_diff requires a "complete" boolean naming the required completeness; none was declared`}
	}

	if !res.Observations.ChangedPaths.Measured {
		return AcceptanceCheckResult{Kind: kind, Evaluated: false, Passed: false,
			Reason: "the operation's changed-paths observation says workspace changes were not directly measured " +
				"(this is the real headspace-cli 0.11.0 shape today, docs/acceptance.md criterion 12)"}
	}
	got := res.Changes.Complete
	if got == want {
		return AcceptanceCheckResult{Kind: kind, Evaluated: true, Passed: true,
			Reason: fmt.Sprintf("the measured workspace-diff completeness %v matches the required %v", got, want)}
	}
	return AcceptanceCheckResult{Kind: kind, Evaluated: true, Passed: false,
		Reason: fmt.Sprintf("the measured workspace-diff completeness %v does not match the required %v", got, want)}
}
