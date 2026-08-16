package worker

import (
	"encoding/json"
	"testing"
)

// Applying a run's recorded affinity at dispatch (issue #107, task t33).
//
// The pinned definition is CACHED per digest and shared by every run of that
// digest (Worker.specs). So the one thing these tests care about, beyond "the
// right actor is used", is that applying one run's affinity cannot be seen by
// another run of the same workflow -- an override that mutated the cached
// nodeSpec would route every concurrent run to whichever actor the last
// trigger happened to pick, and would do it silently.

func TestAffinityOverridesTheNodesDeclaredActor(t *testing.T) {
	node := &nodeSpec{ID: "fix", Kind: kindAgent, Uses: "actor://company/developer@sha256:aaaa"}
	affinity := json.RawMessage(`{"fix":{"actor":"actor://company/security-developer","rule":"security"}}`)

	got := applyAffinity(node, "fix", affinity)
	if got.Uses != "actor://company/security-developer" {
		t.Fatalf("Uses = %q, want the affinity-declared actor", got.Uses)
	}
}

func TestApplyingAffinityDoesNotMutateTheSharedPinnedDefinition(t *testing.T) {
	node := &nodeSpec{ID: "fix", Kind: kindAgent, Uses: "actor://company/developer@sha256:aaaa"}
	original := node.Uses

	first := applyAffinity(node, "fix", json.RawMessage(`{"fix":{"actor":"actor://company/one"}}`))
	if node.Uses != original {
		t.Fatalf("the cached definition was mutated: Uses = %q, want %q", node.Uses, original)
	}
	// A second run of the same digest, with different affinity, must see its
	// own choice -- not the first run's.
	second := applyAffinity(node, "fix", json.RawMessage(`{"fix":{"actor":"actor://company/two"}}`))
	if first.Uses != "actor://company/one" || second.Uses != "actor://company/two" {
		t.Fatalf("affinity leaked across runs: first=%q second=%q", first.Uses, second.Uses)
	}
	// A third run with no affinity at all falls back to the declaration.
	third := applyAffinity(node, "fix", nil)
	if third.Uses != original {
		t.Fatalf("a run with no affinity dispatched to %q, want the declared %q", third.Uses, original)
	}
	if third != node {
		t.Fatal("a run with no affinity should reuse the cached node rather than copy it")
	}
}

func TestAffinityForAnotherNodeLeavesThisOneAlone(t *testing.T) {
	node := &nodeSpec{ID: "fix", Kind: kindAgent, Uses: "actor://company/developer@sha256:aaaa"}
	got := applyAffinity(node, "fix", json.RawMessage(`{"review":{"actor":"actor://company/reviewer"}}`))
	if got.Uses != node.Uses {
		t.Fatalf("Uses = %q, want the declared actor; the affinity named a different node", got.Uses)
	}
}

func TestUnreadableAffinityFallsBackToTheDeclaredActor(t *testing.T) {
	// A malformed column value must not take the dispatch down. The declared
	// `uses` is always a valid answer -- it is what the definition pinned --
	// so falling back to it is strictly safer than refusing to dispatch.
	node := &nodeSpec{ID: "fix", Kind: kindAgent, Uses: "actor://company/developer@sha256:aaaa"}
	for _, raw := range []string{`not json`, `[]`, `{"fix":{"actor":""}}`, `{"fix":"actor://x"}`} {
		got := applyAffinity(node, "fix", json.RawMessage(raw))
		if got.Uses != node.Uses {
			t.Errorf("affinity %s produced Uses = %q, want the declared actor", raw, got.Uses)
		}
	}
}
