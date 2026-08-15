package handover_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/handover"
)

// The language boundary, pinned against a REAL bridge's output.
//
// testdata/bridge-handover-block.json is not hand-written: it is the verbatim
// §13.2 body produced by running adapters/codex's own
// `preserve.handover_ref` against a real scratch git repository, captured at
// the t9 build. The Go wire type and the Python dataclass are two independent
// declarations of one contract, and nothing else in either test suite would
// notice them drifting — a renamed key would simply decode as absent, and
// "absent" is the same shape as "this dispatch handed nothing over", which is
// exactly the silence this package exists to end.
//
// Regenerate it, deliberately, only when the bridge's own
// HandoverResult.to_dict() changes shape.

func TestARealBridgeBlockDecodesAndYieldsAFetchableClaim(t *testing.T) {
	raw, err := os.ReadFile("testdata/bridge-handover-block.json")
	if err != nil {
		t.Fatalf("read the captured bridge body: %v", err)
	}

	var result actors.InvocationResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("the Go wire type cannot decode a real bridge body: %v", err)
	}

	ref, ok := result.Handover.ClaimedRef()
	if !ok {
		t.Fatalf("a real bridge's created handover block yielded no claimed ref: %+v", result.Handover)
	}
	// The ref a bridge really mints must pass the fence this package applies
	// to it. These are two independently-written rules — preserve.py's
	// mint_handover_ref and ValidateRef — and a mismatch would mean every
	// real handover was silently refused.
	if err := handover.ValidateRef(ref); err != nil {
		t.Fatalf("a real bridge's ref %q is refused by the fence: %v", ref, err)
	}

	if result.Handover.Handle == nil {
		t.Fatal("the git_ref handle did not survive the decode")
	}
	if result.Handover.Handle.Publication != "pending" {
		t.Errorf("handle publication = %q, want pending: a bridge never pushes, and a consuming node "+
			"reading `pending` can say so by name instead of guessing at a fetch failure",
			result.Handover.Handle.Publication)
	}
	if result.Handover.Handle.Kind != "git_ref" {
		t.Errorf("handle kind = %q, want git_ref", result.Handover.Handle.Kind)
	}
}

// The other half of the block's vocabulary: a bridge that could not create a
// ref reports a capability from the graph's closed enum, and it must yield no
// claim at all rather than a ref name nothing will find.
func TestABlockThatCreatedNothingYieldsNoClaim(t *testing.T) {
	raw := []byte(`{"outcome":"completed","output":{},"handover":{
		"attempted":true,"created":false,"ref":null,"commit":null,"remote":null,"handle":null,
		"missing_capability":"git_ref_publish",
		"reason":"this host cannot name a remote a handover ref could reach"}}`)

	var result actors.InvocationResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ref, ok := result.Handover.ClaimedRef(); ok {
		t.Fatalf("an unsuccessful handover claimed ref %q", ref)
	}
}

// A body with no handover block at all — every dispatch that asked for none,
// which is nearly all of them.
func TestABodyWithNoHandoverBlockYieldsNoClaim(t *testing.T) {
	var result actors.InvocationResult
	if err := json.Unmarshal([]byte(`{"outcome":"completed","output":{}}`), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := result.Handover.ClaimedRef(); ok {
		t.Fatal("a body with no handover block claimed a ref")
	}
}
