package e2etest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentculture/culture-nodes/internal/compiler"
)

// The reference workflow is a deliverable in its own right: the
// implementation issue's Milestone 0 asks that "the delivery-loop example
// compiles deterministically", and PRD §21.3 asks that "all example workflows
// pass schema and policy validation".
//
// This test needs no database, so it runs everywhere — including on a machine
// where every other test in this package skips for want of PostgreSQL.
func TestReferenceWorkflowCompilesCleanlyAndDeterministically(t *testing.T) {
	source, err := os.ReadFile(filepath.Clean(referenceWorkflowPath))
	if err != nil {
		t.Fatalf("read %s: %v", referenceWorkflowPath, err)
	}

	compiled, diags, err := compiler.Compile(source, compiler.FormatYAML)
	if err != nil {
		t.Fatalf("Compile returned an internal error: %v", err)
	}
	if compiled == nil {
		t.Fatal("the reference workflow did not compile")
	}

	// Zero ERRORS is the checklist item. Zero WARNINGS is the stronger claim
	// this example is meant to make: every component is digest-pinned and
	// every loop is bounded, so `nodes validate` prints nothing to fix.
	for _, d := range diags {
		t.Errorf("unexpected %s diagnostic at %s: %s — %s", d.Level, d.Path, d.Code, d.Message)
	}

	if compiled.Name != "delivery-loop" || compiled.Version != "1.0.0" {
		t.Errorf("name/version = %q/%q, want delivery-loop/1.0.0", compiled.Name, compiled.Version)
	}

	// Deterministic: the same source compiles to byte-identical IR and one
	// digest, which is what makes a run's pin meaningful.
	second, _, err := compiler.Compile(source, compiler.FormatYAML)
	if err != nil || second == nil {
		t.Fatalf("second Compile: %v", err)
	}
	if second.Digest != compiled.Digest {
		t.Errorf("digest differs between compilations: %s vs %s", compiled.Digest, second.Digest)
	}
	if !bytes.Equal(second.Normalized, compiled.Normalized) {
		t.Error("normalized IR differs between compilations of identical source")
	}

	// The human-review branch. While the human-task surface was deferred
	// (deviation d1, github issue #3) this assertion ran the other way round:
	// the reference workflow had to declare NO approval node, because a run
	// reaching one would have failed with a `not_implemented` diagnostic
	// rather than waiting for a human. That surface shipped — the engine
	// parks the token (t6), the decision endpoint resumes it (t7), the worker
	// provably never sees approval work (t8) — and
	// tests/e2e/humanreview_test.go drives the whole branch end to end, so
	// the reference workflow now carries §11.1's approval node as authored.
	// This asserts the fixture's comment is telling the truth.
	if !bytes.Contains(compiled.Normalized, []byte(`"approval"`)) {
		t.Error("the reference workflow declares no approval node; PRD §11.1 routes verify.blocked to " +
			"`human-review`, and tests/e2e/humanreview_test.go drives that branch")
	}
}
