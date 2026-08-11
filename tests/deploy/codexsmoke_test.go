// Package deploytest (this file): an offline, network-free check that
// examples/codex-smoke-pair/smoke.workflow.yaml — the issue #14 / task t8
// two-node codex smoke workflow — still compiles cleanly through the real
// compiler and still carries the properties its own acceptance criteria
// name: both codex nodes declare `sandbox: read-only` and an explicit node
// timeout (h8, h20/c22).
//
// This is the offline half of t8's verification story. The live half —
// actually dispatching to company/codex-thor and company/codex-orin and
// checking the resulting ledger claims — is examples/codex-smoke-pair's own
// run-smoke.sh, which is deliberately NOT exercised here or anywhere in CI:
// codex has no offline mock engine (unlike adapters/colleague's
// COLLEAGUE_ENGINE=mock), so there is no way to prove the live path without
// spending real, billable ChatGPT/API quota. This test proves the workflow
// *shape* is sound; it dispatches nothing and reaches no network.
package deploytest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/agentculture/culture-nodes/internal/compiler"
)

// codexSmokeWorkflowPath locates
// examples/codex-smoke-pair/smoke.workflow.yaml from this test file's own
// path, the same runtime.Caller(0) technique compose_test.go and
// helm_test.go both use to stay independent of the working directory `go
// test` is invoked from.
func codexSmokeWorkflowPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repo root to load smoke.workflow.yaml")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // tests/deploy -> tests -> repo root
	return filepath.Join(repoRoot, "examples", "codex-smoke-pair", "smoke.workflow.yaml")
}

// compileCodexSmokeWorkflow reads and compiles the fixture, failing on an
// internal compiler error (as opposed to a diagnostic, which is a statement
// about the document itself).
func compileCodexSmokeWorkflow(t *testing.T) (*compiler.CompiledWorkflow, []compiler.Diagnostic) {
	t.Helper()
	path := codexSmokeWorkflowPath(t)
	source, err := os.ReadFile(path) // #nosec G304 -- fixed, repo-relative test fixture path.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	compiled, diags, err := compiler.Compile(source, compiler.FormatYAML)
	if err != nil {
		t.Fatalf("Compile(%s) returned an internal error: %v", path, err)
	}
	return compiled, diags
}

// TestCodexSmokePairCompilesWithoutErrors asserts the workflow shape is
// valid: no error diagnostic, a non-nil compiled IR, a content digest.
func TestCodexSmokePairCompilesWithoutErrors(t *testing.T) {
	compiled, diags := compileCodexSmokeWorkflow(t)
	for _, d := range diags {
		if d.Level == compiler.LevelError {
			t.Errorf("unexpected error diagnostic: %s %s: %s", d.Path, d.Code, d.Message)
		}
	}
	if compiled == nil {
		t.Fatal("workflow did not compile (compiled IR is nil)")
	}
	if compiled.Digest == "" {
		t.Error("compiled workflow has no digest")
	}
}

// TestCodexSmokePairHasTwoCodexNodesOnEntryChain asserts the two nodes named
// in the acceptance criteria exist, are agent-kind, and are wired into the
// graph exactly as the workflow's own header comment describes: codex-thor
// is the entry node, codex-orin follows it, both eventually reach a
// terminal node. It intentionally reads the normalized IR (post-defaulting)
// rather than raw YAML, since the IR is what the runtime actually executes.
func TestCodexSmokePairHasTwoCodexNodesOnEntryChain(t *testing.T) {
	compiled, diags := compileCodexSmokeWorkflow(t)
	if compiled == nil {
		t.Fatalf("workflow did not compile; diagnostics: %+v", diags)
	}

	var ir struct {
		Spec struct {
			Entry string `json:"entry"`
			Nodes map[string]struct {
				Kind string `json:"kind"`
				Uses string `json:"uses"`
			} `json:"nodes"`
			Edges []struct {
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"edges"`
		} `json:"spec"`
	}
	unmarshalNormalized(t, compiled, &ir)

	if ir.Spec.Entry != "codex-thor" {
		t.Errorf("spec.entry = %q, want %q", ir.Spec.Entry, "codex-thor")
	}

	thor, ok := ir.Spec.Nodes["codex-thor"]
	if !ok {
		t.Fatal("no codex-thor node in the compiled IR")
	}
	if thor.Kind != "agent" {
		t.Errorf("codex-thor kind = %q, want %q", thor.Kind, "agent")
	}
	if want := "actor://company/codex-thor@sha256:"; len(thor.Uses) < len(want) || thor.Uses[:len(want)] != want {
		t.Errorf("codex-thor uses = %q, want a company/codex-thor actor reference pinned with @sha256:", thor.Uses)
	}

	orin, ok := ir.Spec.Nodes["codex-orin"]
	if !ok {
		t.Fatal("no codex-orin node in the compiled IR")
	}
	if orin.Kind != "agent" {
		t.Errorf("codex-orin kind = %q, want %q", orin.Kind, "agent")
	}
	if want := "actor://company/codex-orin@sha256:"; len(orin.Uses) < len(want) || orin.Uses[:len(want)] != want {
		t.Errorf("codex-orin uses = %q, want a company/codex-orin actor reference pinned with @sha256:", orin.Uses)
	}

	foundThorToOrin := false
	foundOrinToFinish := false
	for _, e := range ir.Spec.Edges {
		if e.From == "codex-thor.completed" && e.To == "codex-orin" {
			foundThorToOrin = true
		}
		if e.From == "codex-orin.completed" && e.To == "finish" {
			foundOrinToFinish = true
		}
	}
	if !foundThorToOrin {
		t.Error("no codex-thor.completed -> codex-orin edge in the compiled IR")
	}
	if !foundOrinToFinish {
		t.Error("no codex-orin.completed -> finish edge in the compiled IR")
	}
}

// TestCodexSmokePairNodesAreReadOnlyWithExplicitTimeout is the direct check
// against t8's own acceptance criteria: "both codex nodes declare read-only
// sandbox (h8) and an explicit node timeout (h20/c22)". Bindings are JSON
// Pointers into /run/input (see internal/compiler/contract.go's binding-root
// rules), so this asserts each node's `sandbox` binding targets a run-input
// field — the actual literal ("read-only") is a run-time value the compiler
// does not see, and run-smoke.sh is what supplies it (SMOKE_SANDBOX,
// defaulting to "read-only").
func TestCodexSmokePairNodesAreReadOnlyWithExplicitTimeout(t *testing.T) {
	compiled, diags := compileCodexSmokeWorkflow(t)
	if compiled == nil {
		t.Fatalf("workflow did not compile; diagnostics: %+v", diags)
	}

	var ir struct {
		Spec struct {
			Nodes map[string]struct {
				Input struct {
					Bindings map[string]string `json:"bindings"`
				} `json:"input"`
				Policy struct {
					Timeout string `json:"timeout"`
				} `json:"policy"`
			} `json:"nodes"`
		} `json:"spec"`
	}
	unmarshalNormalized(t, compiled, &ir)

	for _, id := range []string{"codex-thor", "codex-orin"} {
		n, ok := ir.Spec.Nodes[id]
		if !ok {
			t.Fatalf("no %s node in the compiled IR", id)
		}
		sandboxBinding, ok := n.Input.Bindings["sandbox"]
		if !ok {
			t.Errorf("%s: no input.bindings.sandbox", id)
		} else if sandboxBinding != "/run/input/sandbox" {
			t.Errorf("%s: input.bindings.sandbox = %q, want %q", id, sandboxBinding, "/run/input/sandbox")
		}
		if n.Policy.Timeout == "" {
			t.Errorf("%s: no explicit policy.timeout in the compiled IR", id)
		}
	}
}

// unmarshalNormalized decodes compiled.Normalized (the canonical JSON
// encoding of the IR, per CompiledWorkflow's own docs) into dst. It is a
// small helper shared by the structural assertions above so each stays
// focused on what it checks rather than how the IR is decoded.
func unmarshalNormalized(t *testing.T, compiled *compiler.CompiledWorkflow, dst any) {
	t.Helper()
	if err := json.Unmarshal(compiled.Normalized, dst); err != nil {
		t.Fatalf("unmarshal compiled.Normalized: %v", err)
	}
}
