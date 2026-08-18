package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// codeOperationInput (code.go, issue #170) decides what — if anything —
// buildCodeOperation lowers into a code operation's NODES_INPUT_JSON
// environment value. It is tested here without a database because the
// decision is pure: given the bytes resolveNodeInput produced, what goes on
// the wire.

// TestCodeOperationInputOmitsTheUndeclaredBindingDefault: resolveNodeInput
// returns the literal `{}` for every node that declares no input binding —
// the default across today's workflows — and that must forward nothing, or
// every such dispatch would carry a new environment value it did not carry
// yesterday.
func TestCodeOperationInputOmitsTheUndeclaredBindingDefault(t *testing.T) {
	for _, in := range []json.RawMessage{nil, {}, json.RawMessage(`{}`), json.RawMessage(`  {}  `)} {
		got, err := codeOperationInput(in)
		if err != nil {
			t.Fatalf("codeOperationInput(%q): %v", in, err)
		}
		if got != nil {
			t.Errorf("codeOperationInput(%q) = %q, want nil (nothing to forward)", in, got)
		}
	}
}

// TestCodeOperationInputForwardsACanonicalDocument: a genuinely bound value
// is forwarded, canonicalized — key order must not depend on how the
// binding happened to serialize it.
func TestCodeOperationInputForwardsACanonicalDocument(t *testing.T) {
	got, err := codeOperationInput(json.RawMessage(`{"b":2,"a":1}`))
	if err != nil {
		t.Fatalf("codeOperationInput: %v", err)
	}
	if want := `{"a":1,"b":2}`; string(got) != want {
		t.Errorf("codeOperationInput = %q, want canonical %q", got, want)
	}
}

// TestCodeOperationInputForwardsAnArrayOrScalar: §11.2's `from` binding can
// resolve to any JSON value, not only an object — a `bindings` map is the
// only shape restricted to objects.
func TestCodeOperationInputForwardsAnArrayOrScalar(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   json.RawMessage
		want string
	}{
		{"array", json.RawMessage(`[3,1,2]`), `[3,1,2]`},
		{"string", json.RawMessage(`"widget"`), `"widget"`},
		{"number", json.RawMessage(`42`), `42`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := codeOperationInput(tc.in)
			if err != nil {
				t.Fatalf("codeOperationInput(%s): %v", tc.name, err)
			}
			if string(got) != tc.want {
				t.Errorf("codeOperationInput(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestCodeOperationInputRefusesAnOversizedDocument (issue #170): a document
// too large to cross as a single envp element must not be silently
// truncated — it refuses with a diagnostic naming the byte count and the
// env var it would have carried.
func TestCodeOperationInputRefusesAnOversizedDocument(t *testing.T) {
	huge := `{"blob":"` + strings.Repeat("x", maxCodeInputEnvBytes) + `"}`
	_, err := codeOperationInput(json.RawMessage(huge))
	if err == nil {
		t.Fatal("codeOperationInput accepted a document over the limit; want a refusal")
	}
	if !strings.Contains(err.Error(), runners.EnvInputJSON) {
		t.Errorf("error %q does not name %s", err, runners.EnvInputJSON)
	}
	if !strings.Contains(err.Error(), "bytes") {
		t.Errorf("error %q does not quote a byte count", err)
	}
}

// TestCodeOperationInputAtTheLimitIsAccepted: the boundary itself is not
// refused — only strictly over it.
func TestCodeOperationInputAtTheLimitIsAccepted(t *testing.T) {
	// Build a document whose canonical form is exactly maxCodeInputEnvBytes.
	prefix, suffix := `{"blob":"`, `"}`
	pad := maxCodeInputEnvBytes - len(prefix) - len(suffix)
	if pad < 0 {
		t.Fatal("maxCodeInputEnvBytes too small for this test's fixed overhead")
	}
	doc := prefix + strings.Repeat("x", pad) + suffix
	got, err := codeOperationInput(json.RawMessage(doc))
	if err != nil {
		t.Fatalf("codeOperationInput at the limit refused: %v", err)
	}
	if len(got) != maxCodeInputEnvBytes {
		t.Fatalf("test fixture built %d bytes, want exactly %d", len(got), maxCodeInputEnvBytes)
	}

	over := prefix + strings.Repeat("x", pad+1) + suffix
	if _, err := codeOperationInput(json.RawMessage(over)); err == nil {
		t.Fatal("codeOperationInput accepted one byte over the limit")
	}
}

// TestCodeOperationInputRejectsInvalidJSON: a resolved binding is always
// valid JSON in practice (resolveNodeInput only ever marshals), but the
// canonicalization step must fail loudly rather than forward garbage if
// that invariant is ever violated.
func TestCodeOperationInputRejectsInvalidJSON(t *testing.T) {
	if _, err := codeOperationInput(json.RawMessage(`{not json`)); err == nil {
		t.Fatal("codeOperationInput accepted invalid JSON")
	}
}

// TestCodeOperationInputTrimsWhitespaceOnlyDocuments guards the emptiness
// check itself: whitespace-only bytes (never produced by resolveNodeInput
// today, but not ruled out by its return type) must not be canonicalized
// into a spurious `null` forward.
func TestCodeOperationInputTrimsWhitespaceOnlyDocuments(t *testing.T) {
	got, err := codeOperationInput(json.RawMessage("   \n\t "))
	if err != nil {
		t.Fatalf("codeOperationInput(whitespace): %v", err)
	}
	if got != nil {
		t.Errorf("codeOperationInput(whitespace) = %q, want nil", got)
	}
}
