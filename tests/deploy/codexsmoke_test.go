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
	"strings"
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
// graph exactly as the workflow's own header comment describes: codex-first
// is the entry node, codex-second follows it, both eventually reach a
// terminal node. It intentionally reads the normalized IR (post-defaulting)
// rather than raw YAML, since the IR is what the runtime actually executes.
//
// Node id and actor id are asserted SEPARATELY (task t16). They used to be
// the same string — node `codex-thor` on `actor://company/codex-thor` — which
// read as if a graph had to be authored around this deployment's machine
// names. It does not: the node id is the graph's own vocabulary, the actor id
// is a registry key resolved per deployment, and the smoke's real claim is
// that TWO DISTINCT registered codex actors each complete a node.
func TestCodexSmokePairHasTwoCodexNodesOnEntryChain(t *testing.T) {
	compiled, diags := compileCodexSmokeWorkflow(t)
	if compiled == nil {
		t.Fatalf("workflow did not compile; diagnostics: %+v", diags)
	}

	var ir struct {
		Spec struct {
			Entry string                 `json:"entry"`
			Nodes map[string]smokeIRNode `json:"nodes"`
			Edges []smokeIREdge          `json:"edges"`
		} `json:"spec"`
	}
	unmarshalNormalized(t, compiled, &ir)

	if ir.Spec.Entry != "codex-first" {
		t.Errorf("spec.entry = %q, want %q", ir.Spec.Entry, "codex-first")
	}

	assertCodexAgentNode(t, ir.Spec.Nodes, "codex-first", "company/codex-thor")
	assertCodexAgentNode(t, ir.Spec.Nodes, "codex-second", "company/codex-orin")

	if !hasEdge(ir.Spec.Edges, "codex-first.completed", "codex-second") {
		t.Error("no codex-first.completed -> codex-second edge in the compiled IR")
	}
	if !hasEdge(ir.Spec.Edges, "codex-second.completed", "finish") {
		t.Error("no codex-second.completed -> finish edge in the compiled IR")
	}
}

// smokeIRNode and smokeIREdge are the slices of the normalized IR the
// entry-chain assertions read; named so assertCodexAgentNode and hasEdge
// can share them.
type smokeIRNode struct {
	Kind string `json:"kind"`
	Uses string `json:"uses"`
}

type smokeIREdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// assertCodexAgentNode asserts the named node exists in the compiled IR, is
// agent-kind, and is placed on the given actor registry id, pinned with
// @sha256:. The actor id is a parameter rather than derived from the node id
// because the two are different things: one is the graph's vocabulary, the
// other is a key in a deployment's actors table (task t16).
func assertCodexAgentNode(t *testing.T, nodes map[string]smokeIRNode, id, actorID string) {
	t.Helper()
	n, ok := nodes[id]
	if !ok {
		t.Fatalf("no %s node in the compiled IR", id)
	}
	if n.Kind != "agent" {
		t.Errorf("%s kind = %q, want %q", id, n.Kind, "agent")
	}
	if want := "actor://" + actorID + "@sha256:"; !strings.HasPrefix(n.Uses, want) {
		t.Errorf("%s uses = %q, want a %s actor reference pinned with @sha256:", id, n.Uses, actorID)
	}
}

// hasEdge reports whether the compiled IR carries a from -> to edge.
func hasEdge(edges []smokeIREdge, from, to string) bool {
	for _, e := range edges {
		if e.From == from && e.To == to {
			return true
		}
	}
	return false
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

	for _, id := range []string{"codex-first", "codex-second"} {
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
