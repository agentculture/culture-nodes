package compiler

import (
	"encoding/json"
	"testing"
)

// Issue #73, option A. A binding value used to be a JSON-Pointer string and
// nothing else, which meant an observable — the one thing a human node is
// waiting for — could only reach the node through run input. An author read
// the graph and could not see what the node watched. These tests pin the
// literal form end to end at the compiler: it decodes, it survives
// normalization byte for byte, it is checked against the node's own input
// contract, and it never becomes ambiguous with a pointer.

// TestLiteralBindingCompilesAlongsidePointers is the acceptance case: pointer
// and literal bindings coexist on one node, and the document compiles clean.
func TestLiteralBindingCompilesAlongsidePointers(t *testing.T) {
	compiled, diags := compileFixture(t, "literal-binding.workflow.yaml", FormatYAML)
	if compiled == nil {
		t.Fatalf("literal-binding fixture did not compile: %s", renderDiagnostics(diags))
	}
	if len(diags) != 0 {
		t.Errorf("literal-binding fixture produced diagnostics, want none: %s", renderDiagnostics(diags))
	}

	bindings := compiled.IR.Spec.Nodes["start"].Input.Bindings
	if got := bindings["instruction"]; got.Pointer != "/run/input/instruction" || got.isLiteral() {
		t.Errorf("instruction binding = %+v, want the pointer /run/input/instruction", got)
	}
	for name, want := range map[string]string{
		"observe": `{"kind":"github_pr_merged","pr":42}`,
		"retries": `3`,
		"enabled": `true`,
		"note":    `null`,
	} {
		got := bindings[name]
		if !got.isLiteral() {
			t.Errorf("%s binding = %+v, want a literal", name, got)
			continue
		}
		if got.Pointer != "" {
			t.Errorf("%s binding carries pointer %q as well as a literal", name, got.Pointer)
		}
		assertSameJSON(t, name, got.Literal, want)
	}
}

// TestLiteralBindingSurvivesNormalization proves the declaration reaches the
// runtime as the author wrote it: the normalized IR — the exact bytes the
// content digest addresses — carries the literal object inline, not a pointer
// standing in for it.
func TestLiteralBindingSurvivesNormalization(t *testing.T) {
	compiled, diags := compileFixture(t, "literal-binding.workflow.yaml", FormatYAML)
	if compiled == nil {
		t.Fatalf("literal-binding fixture did not compile: %s", renderDiagnostics(diags))
	}

	var normalized struct {
		Spec struct {
			Nodes map[string]struct {
				Input struct {
					Bindings map[string]json.RawMessage `json:"bindings"`
				} `json:"input"`
			} `json:"nodes"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(compiled.Normalized, &normalized); err != nil {
		t.Fatalf("decode normalized IR: %v", err)
	}

	bindings := normalized.Spec.Nodes["start"].Input.Bindings
	assertSameJSON(t, "instruction", bindings["instruction"], `"/run/input/instruction"`)
	assertSameJSON(t, "observe", bindings["observe"],
		`{"literal":{"kind":"github_pr_merged","pr":42}}`)
	assertSameJSON(t, "note", bindings["note"], `{"literal":null}`)
}

// TestLiteralIsCheckedAgainstTheNodeInputContract is the reason a literal is
// worth more than an opaque blob: the value is fully known at publish time, so
// a literal the node's own contract refuses is a publish-time error rather
// than a first-dispatch surprise.
func TestLiteralIsCheckedAgainstTheNodeInputContract(t *testing.T) {
	compiled, diags := compileFixture(t, "err-literal-binding.workflow.yaml", FormatYAML)
	if compiled != nil {
		t.Fatal("a literal that violates the node's input contract compiled")
	}

	want := map[string]bool{
		// The declared integer got a string.
		"/spec/nodes/start/input/bindings/observe/literal/pr": false,
		// additionalProperties: false, and `stray` is not a declared member.
		"/spec/nodes/start/input/bindings/stray/literal": false,
	}
	for _, d := range diags {
		if d.Code != CodeContractLiteralInvalid {
			continue
		}
		if _, ok := want[d.Path]; !ok {
			t.Errorf("unexpected %s at %s: %s", d.Code, d.Path, d.Message)
			continue
		}
		want[d.Path] = true
	}
	for path, seen := range want {
		if !seen {
			t.Errorf("no %s diagnostic at %s; got: %s", CodeContractLiteralInvalid, path, renderDiagnostics(diags))
		}
	}
}

// TestPointerBindingsAreUnaffectedByTheLiteralCheck guards the migration
// promise: the literal check reads only literals, so a node whose bindings are
// all pointers is judged exactly as it was before issue #73 — including the
// `required` members only a pointer can supply, which the literal check must
// never mistake for a missing literal.
func TestPointerBindingsAreUnaffectedByTheLiteralCheck(t *testing.T) {
	compiled, diags := compileFixture(t, "deliver-change.workflow.yaml", FormatYAML)
	if compiled == nil {
		t.Fatalf("the PRD §11.1 example stopped compiling: %s", renderDiagnostics(diags))
	}
	for _, d := range diags {
		if d.Code == CodeContractLiteralInvalid {
			t.Errorf("pointer-only workflow produced a literal diagnostic at %s: %s", d.Path, d.Message)
		}
	}
}

// TestBindingValueRejectsAmbiguousShapes pins the decoder's half of "keep
// pointers unambiguous": a bare string is always a pointer, a literal is
// always explicitly wrapped, and anything else is refused rather than guessed
// at.
func TestBindingValueRejectsAmbiguousShapes(t *testing.T) {
	cases := map[string]string{
		"a bare object":            `{"kind":"github_pr_merged"}`,
		"literal beside a sibling": `{"literal":1,"pointer":"/run/input"}`,
		"an empty object":          `{}`,
		"null":                     `null`,
		"a bare array":             `[1,2]`,
		"a bare number":            `7`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var v bindingValue
			if err := json.Unmarshal([]byte(raw), &v); err == nil {
				t.Fatalf("decoded %s as %+v, want a refusal", raw, v)
			}
		})
	}
}

// TestBindingValueRoundTrips proves a decoded value re-encodes to the shape it
// came from. The IR's bytes are what the content digest addresses, so a
// binding that re-encoded differently would silently re-digest every published
// workflow that carries one.
func TestBindingValueRoundTrips(t *testing.T) {
	for _, raw := range []string{
		`"/run/input/subject"`,
		`{"literal":{"kind":"github_pr_merged","pr":42}}`,
		`{"literal":null}`,
		`{"literal":[1,"two",false]}`,
		`{"literal":""}`,
	} {
		var v bindingValue
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("encode %s: %v", raw, err)
		}
		assertSameJSON(t, raw, encoded, raw)
	}
}

// assertSameJSON compares two JSON documents by value rather than by bytes.
func assertSameJSON(t *testing.T, what string, got json.RawMessage, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("%s: decode got %s: %v", what, got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("%s: decode want %s: %v", what, want, err)
	}
	gotCanonical, _ := json.Marshal(gotValue)
	wantCanonical, _ := json.Marshal(wantValue)
	if string(gotCanonical) != string(wantCanonical) {
		t.Errorf("%s = %s, want %s", what, gotCanonical, wantCanonical)
	}
}
