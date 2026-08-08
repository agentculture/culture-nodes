package compiler

import (
	"strings"
	"testing"
)

// wantDiag is the (level, path, code) triple every deliberate-error fixture
// asserts exactly. Message and hint are prose and are checked separately where
// their content is load-bearing (the runner-cap diagnostics).
type wantDiag struct {
	level Level
	path  string
	code  string
}

// TestDeliberateErrorFixtures is the diagnostic-precision acceptance test: one
// fixture per validation level, each asserting its exact diagnostic list in
// the exact deterministic order (path, then code).
func TestDeliberateErrorFixtures(t *testing.T) {
	cases := []struct {
		name string
		file string
		want []wantDiag
	}{
		{
			// The owners level. Under the t2 authoring schema both
			// metadata.ownerRef and node.ownerRef are required, so removing
			// them trips the structure gate too; the owners level's own
			// verdict — this node has no resolvable owner — is reported
			// independently, because "the field is absent" and "no owner
			// could be resolved for this node" are different statements.
			name: "missing ownerRef",
			file: "err-missing-owner.workflow.yaml",
			want: []wantDiag{
				{LevelError, "/metadata/ownerRef", CodeStructureSchema},
				{LevelError, "/spec/nodes/start/ownerRef", CodeOwnersUnresolved},
				{LevelError, "/spec/nodes/start/ownerRef", CodeStructureSchema},
			},
		},
		{
			name: "unknown edge target",
			file: "err-unknown-edge-target.workflow.yaml",
			want: []wantDiag{
				{LevelError, "/spec/edges/1/to", CodeGraphEdgeTargetUnknown},
			},
		},
		{
			name: "unknown edge source node",
			file: "err-unknown-edge-source.workflow.yaml",
			want: []wantDiag{
				{LevelError, "/spec/edges/1/from", CodeGraphEdgeSourceUnknown},
			},
		},
		{
			name: "outcome not declared by the node contract",
			file: "err-undeclared-outcome.workflow.yaml",
			want: []wantDiag{
				{LevelError, "/spec/edges/1/from", CodeGraphOutcomeUndeclared},
			},
		},
		{
			name: "unreachable node",
			file: "err-unreachable-node.workflow.yaml",
			want: []wantDiag{
				{LevelError, "/spec/nodes/orphan", CodeGraphNodeUnreachable},
			},
		},
		{
			name: "no end node reachable from entry",
			file: "err-no-end-reachable.workflow.yaml",
			want: []wantDiag{
				{LevelError, "/spec/nodes", CodeGraphNoEndReachable},
			},
		},
		{
			name: "entry names an unknown node",
			file: "err-unknown-entry.workflow.yaml",
			want: []wantDiag{
				{LevelError, "/spec/entry", CodeGraphEntryUnknown},
			},
		},
		{
			name: "invalid JSON Pointer bindings",
			file: "err-bad-binding.workflow.yaml",
			want: []wantDiag{
				{LevelError, "/spec/nodes/start/input/bindings/ghost", CodeContractBindingNodeUnknown},
				{LevelError, "/spec/nodes/start/input/bindings/wrong", CodeContractBindingUnresolved},
			},
		},
		{
			name: "invalid CEL",
			file: "err-bad-cel.workflow.yaml",
			want: []wantDiag{
				{LevelError, "/spec/edges/0/when", CodeContractCELInvalid},
			},
		},
		{
			name: "code node over the runner caps",
			file: "err-over-cap-code.workflow.yaml",
			want: []wantDiag{
				{LevelError, "/spec/nodes/build/contract/maxInlinePayloadBytes", CodePolicyPayloadOverCap},
				{LevelError, "/spec/nodes/build/policy/timeout", CodePolicyTimeoutOverCap},
			},
		},
		{
			name: "unknown ledger projection",
			file: "err-unknown-projection.workflow.yaml",
			want: []wantDiag{
				{LevelError, "/spec/nodes/start/input/bindings/scope", CodeLedgerProjectionUnknown},
			},
		},
		{
			name: "observed evidence declared by a non-runner node",
			file: "err-observe-not-permitted.workflow.yaml",
			want: []wantDiag{
				{LevelError, "/spec/nodes/start/ledger/observe", CodeLedgerObserveNotPermitted},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiled, diags := compileFixture(t, tc.file, FormatYAML)
			assertDiagnostics(t, diags, tc.want)
			if compiled != nil {
				t.Error("Compile returned a CompiledWorkflow despite error diagnostics")
			}
		})
	}
}

// assertDiagnostics compares the full diagnostic list, order included.
func assertDiagnostics(t *testing.T, got []Diagnostic, want []wantDiag) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d diagnostics, want %d:\n%s", len(got), len(want), renderDiagnostics(got))
	}
	for i := range want {
		if got[i].Level != want[i].level || got[i].Path != want[i].path || got[i].Code != want[i].code {
			t.Fatalf("diagnostic[%d] = {%s %s %s}, want {%s %s %s}\nfull list:\n%s",
				i, got[i].Level, got[i].Path, got[i].Code,
				want[i].level, want[i].path, want[i].code, renderDiagnostics(got))
		}
	}
}

func renderDiagnostics(diags []Diagnostic) string {
	var b strings.Builder
	for _, d := range diags {
		b.WriteString("  ")
		b.WriteString(string(d.Level))
		b.WriteString(" ")
		b.WriteString(d.Path)
		b.WriteString(" ")
		b.WriteString(d.Code)
		b.WriteString(": ")
		b.WriteString(d.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// TestRunnerCapDiagnosticsNameTheCapAndItsSource is claim c40 stated as a
// test: refusing an over-cap code node is only useful if the diagnostic says
// what the cap is and where it comes from.
func TestRunnerCapDiagnosticsNameTheCapAndItsSource(t *testing.T) {
	_, diags := compileFixture(t, "err-over-cap-code.workflow.yaml", FormatYAML)
	if len(diags) != 2 {
		t.Fatalf("diagnostics = %s, want two", renderDiagnostics(diags))
	}
	for _, d := range diags {
		if !strings.Contains(d.Message, RunnerLimitSource) {
			t.Errorf("%s message %q does not name the limit source %q", d.Code, d.Message, RunnerLimitSource)
		}
	}

	payload, timeout := diags[0], diags[1]
	if !strings.Contains(payload.Message, "6291456") {
		t.Errorf("payload message %q does not name the 6 MiB cap in bytes", payload.Message)
	}
	if !strings.Contains(timeout.Message, "900") {
		t.Errorf("timeout message %q does not name the 900s cap", timeout.Message)
	}
}

// TestDiagnosticOrderIsDeterministic checks the documented sort: by path, then
// by code. It uses a fixture that trips several levels at once.
func TestDiagnosticOrderIsDeterministic(t *testing.T) {
	first, _ := compileFixture(t, "err-many.workflow.yaml", FormatYAML)
	if first != nil {
		t.Fatal("err-many fixture must not produce a CompiledWorkflow")
	}
	_, diags := compileFixture(t, "err-many.workflow.yaml", FormatYAML)
	if len(diags) < 3 {
		t.Fatalf("expected several diagnostics, got %s", renderDiagnostics(diags))
	}
	for i := 1; i < len(diags); i++ {
		prev, cur := diags[i-1], diags[i]
		if prev.Path > cur.Path {
			t.Fatalf("diagnostics not sorted by path:\n%s", renderDiagnostics(diags))
		}
		if prev.Path == cur.Path && prev.Code > cur.Code {
			t.Fatalf("diagnostics not sorted by code within a path:\n%s", renderDiagnostics(diags))
		}
	}

	// Repeat compilations must produce the identical sequence.
	_, again := compileFixture(t, "err-many.workflow.yaml", FormatYAML)
	if len(again) != len(diags) {
		t.Fatalf("diagnostic count is not stable across compilations")
	}
	for i := range diags {
		if diags[i] != again[i] {
			t.Fatalf("diagnostic[%d] differs across compilations: %+v vs %+v", i, diags[i], again[i])
		}
	}
}

// TestHasErrors distinguishes a warning-only compilation from a failed one.
func TestHasErrors(t *testing.T) {
	if HasErrors(nil) {
		t.Error("HasErrors(nil) = true")
	}
	if HasErrors([]Diagnostic{{Level: LevelWarning}}) {
		t.Error("HasErrors(warning-only) = true")
	}
	if !HasErrors([]Diagnostic{{Level: LevelWarning}, {Level: LevelError}}) {
		t.Error("HasErrors(with an error) = false")
	}
}

func TestPolicyWarningsDoNotBlockCompilation(t *testing.T) {
	compiled, diags := compileFixture(t, "warn-policy.workflow.yaml", FormatYAML)
	if compiled == nil {
		t.Fatalf("policy warnings must not block compilation; diagnostics:\n%s", renderDiagnostics(diags))
	}
	want := []wantDiag{
		{LevelWarning, "/spec/nodes/build/operation/image", CodePolicyImageUnpinned},
		{LevelWarning, "/spec/nodes/build/operation/requiresShell", CodePolicyShellRequested},
		{LevelWarning, "/spec/nodes/build/uses", CodePolicyComponentUnpinned},
	}
	assertDiagnostics(t, diags, want)
}
