package worker_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// The gate vocabulary (task t16, issue #101).
//
// A merge gate expressed as a `code` node declares three domain outcomes, so
// ConventionalCodeOutcomes has to know a third answer exists. These tests pin
// the convention itself: which names, which exit codes, and — the half that
// matters — that a node declaring PART of the vocabulary is refused rather
// than half-mapped.

func gateOutcomeNames() []string {
	names := []string{"gates_passed", "changes_required", "measurement_incomplete"}
	sort.Strings(names)
	return names
}

// TestGateVocabularyMapsThreeExitCodes is the headline: a code node declaring
// the gate's three outcomes is dispatchable, and each outcome has its own exit
// code rather than sharing "nonzero".
func TestGateVocabularyMapsThreeExitCodes(t *testing.T) {
	outcomes, err := worker.ConventionalCodeOutcomes("merge-gate", gateOutcomeNames())
	if err != nil {
		t.Fatalf("ConventionalCodeOutcomes: %v", err)
	}
	if outcomes.Success != "gates_passed" {
		t.Errorf("Success = %q, want gates_passed", outcomes.Success)
	}
	if outcomes.Failure != "" {
		t.Errorf("Failure = %q, want empty: an exit code the gate never publishes is a crashed "+
			"instrument, not a domain answer", outcomes.Failure)
	}
	want := map[int]string{0: "gates_passed", 1: "changes_required", 2: "measurement_incomplete"}
	for code, outcome := range want {
		if outcomes.ByExitCode[code] != outcome {
			t.Errorf("ByExitCode[%d] = %q, want %q", code, outcomes.ByExitCode[code], outcome)
		}
	}
	if len(outcomes.ByExitCode) != len(want) {
		t.Errorf("ByExitCode = %v, want exactly %v", outcomes.ByExitCode, want)
	}
}

// TestPartialGateVocabularyIsRefused: declaring `gates_passed` without
// `measurement_incomplete` is a graph that CANNOT express "I could not
// measure this", and the only place that answer could then go is one of the
// other two edges. Refusing the dispatch is the whole point — a half-declared
// gate would route an unmeasured suite down a real edge.
func TestPartialGateVocabularyIsRefused(t *testing.T) {
	for _, declared := range [][]string{
		{"changes_required", "gates_passed"},
		{"gates_passed"},
		{"changes_required", "measurement_incomplete"},
	} {
		_, err := worker.ConventionalCodeOutcomes("merge-gate", declared)
		if err == nil {
			t.Errorf("ConventionalCodeOutcomes(%v) accepted a partial gate vocabulary; want a refusal", declared)
			continue
		}
		if !strings.Contains(err.Error(), "measurement_incomplete") {
			t.Errorf("refusal for %v does not name the missing vocabulary: %v", declared, err)
		}
	}
}

// TestGateVocabularyRefusesForeignOutcomes: mixing the gate names with the
// passed/failed pair leaves it ambiguous which port exit 1 belongs to, and
// guessing is how a failing suite gets routed down the passing edge.
func TestGateVocabularyRefusesForeignOutcomes(t *testing.T) {
	declared := append(gateOutcomeNames(), "failed")
	sort.Strings(declared)
	if _, err := worker.ConventionalCodeOutcomes("merge-gate", declared); err == nil {
		t.Fatal("ConventionalCodeOutcomes accepted gate outcomes mixed with a foreign one; want a refusal")
	}
}

// TestConventionalPairIsUnchanged guards every code node that already ships:
// the PRD §11.1 passed/failed pair must keep resolving exactly as it did, with
// no exit-code table at all.
func TestConventionalPairIsUnchanged(t *testing.T) {
	outcomes, err := worker.ConventionalCodeOutcomes("test", []string{"failed", "passed"})
	if err != nil {
		t.Fatalf("ConventionalCodeOutcomes: %v", err)
	}
	if outcomes.Success != "passed" || outcomes.Failure != "failed" {
		t.Errorf("outcomes = %+v, want passed/failed", outcomes)
	}
	if len(outcomes.ByExitCode) != 0 {
		t.Errorf("ByExitCode = %v, want no table for the two-port convention", outcomes.ByExitCode)
	}
}

// TestGateExitCodesMatchTheLedgerVocabulary keeps the two halves of one
// contract in step. internal/handover computes the outcome from the per-gate
// records and publishes the exit code the gate program reports it with; this
// package maps that exit code back onto the declared outcome. A drift between
// them would route a `measurement_incomplete` report down the
// `changes_required` edge, silently, with both sides looking correct in
// isolation. The two lists cannot be one list: the worker resolves an outcome
// before any ledger record exists, so it must not depend on the ledger's
// vocabulary at runtime.
func TestGateExitCodesMatchTheLedgerVocabulary(t *testing.T) {
	outcomes, err := worker.ConventionalCodeOutcomes("merge-gate", gateOutcomeNames())
	if err != nil {
		t.Fatalf("ConventionalCodeOutcomes: %v", err)
	}
	for code, outcome := range outcomes.ByExitCode {
		want, ok := handover.GateExitCode(outcome)
		if !ok {
			t.Errorf("worker routes exit %d to %q, which internal/handover does not publish an exit code for",
				code, outcome)
			continue
		}
		if want != code {
			t.Errorf("worker routes exit %d to %q; internal/handover reports that outcome with exit %d",
				code, outcome, want)
		}
	}
}
