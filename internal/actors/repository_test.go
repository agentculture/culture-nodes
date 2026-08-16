package actors_test

import (
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// The repository identity seam (task t1, issue #125). What these tests hold
// is one property in four shapes: the identity a bridge reads is the one the
// REGISTRY declared, and nothing else in the document can become it.

func TestWithRepositoryIdentitySetsTheRegisteredIdentity(t *testing.T) {
	input := json.RawMessage(`{"instruction":"fix the top finding"}`)

	var got map[string]any
	if err := json.Unmarshal(actors.WithRepositoryIdentity(input, "agentculture/culture-nodes"), &got); err != nil {
		t.Fatalf("result is not a JSON object: %v", err)
	}
	if got[actors.RepositoryIdentityKey] != "agentculture/culture-nodes" {
		t.Fatalf("%s = %v, want the registered identity", actors.RepositoryIdentityKey, got[actors.RepositoryIdentityKey])
	}
	if got["instruction"] != "fix the top finding" {
		t.Fatalf("the rest of the input did not survive: %v", got)
	}
}

func TestWithRepositoryIdentityLeavesAnUndeclaredDispatchUntouched(t *testing.T) {
	input := json.RawMessage(`{"instruction":"fix the top finding","repo":"/work/repo"}`)

	got := actors.WithRepositoryIdentity(input, "")

	if string(got) != string(input) {
		t.Fatalf("input was rewritten for an actor that declares no identity:\n got %s\nwant %s", got, input)
	}
}

// The acceptance this file exists for: the identity is a registry fact, so a
// run input that names one loses to the registration — and to nothing else.
func TestWithRepositoryIdentityOverridesAnIdentityInTheInput(t *testing.T) {
	input := json.RawMessage(`{"repository":"attacker/owned","repository_identity":"attacker/owned"}`)

	var got map[string]any
	if err := json.Unmarshal(actors.WithRepositoryIdentity(input, "agentculture/culture-nodes"), &got); err != nil {
		t.Fatalf("result is not a JSON object: %v", err)
	}
	if got[actors.RepositoryIdentityKey] != "agentculture/culture-nodes" {
		t.Fatalf("%s = %v, want the registered identity to win over the input's own",
			actors.RepositoryIdentityKey, got[actors.RepositoryIdentityKey])
	}
	// The event payload's own `repository` field is data the node was bound
	// to, not an identity: it travels untouched, and it is not what the
	// bridge resolves a checkout from.
	if got["repository"] != "attacker/owned" {
		t.Fatalf("the payload's own repository field was rewritten: %v", got["repository"])
	}
}

// The same acceptance from the other side: an actor whose registration
// declares no identity dispatches with no identity, however hard the input
// tries to supply one.
func TestWithRepositoryIdentityStripsAnUnregisteredIdentityFromTheInput(t *testing.T) {
	input := json.RawMessage(`{"instruction":"go","repository_identity":"attacker/owned"}`)

	var got map[string]any
	if err := json.Unmarshal(actors.WithRepositoryIdentity(input, ""), &got); err != nil {
		t.Fatalf("result is not a JSON object: %v", err)
	}
	if _, present := got[actors.RepositoryIdentityKey]; present {
		t.Fatalf("%s survived from the input: %v", actors.RepositoryIdentityKey, got)
	}
	if got["instruction"] != "go" {
		t.Fatalf("the rest of the input did not survive: %v", got)
	}
}

func TestWithRepositoryIdentityCarriesOnAnEmptyInput(t *testing.T) {
	for _, empty := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("null")} {
		var got map[string]any
		if err := json.Unmarshal(actors.WithRepositoryIdentity(empty, "agentculture/culture-nodes"), &got); err != nil {
			t.Fatalf("input %q: result is not a JSON object: %v", empty, err)
		}
		if got[actors.RepositoryIdentityKey] != "agentculture/culture-nodes" {
			t.Fatalf("input %q: %s = %v, want the registered identity", empty, actors.RepositoryIdentityKey,
				got[actors.RepositoryIdentityKey])
		}
	}
}

func TestWithRepositoryIdentityLeavesANonObjectInputUntouched(t *testing.T) {
	for _, input := range []json.RawMessage{
		json.RawMessage(`["a","b"]`),
		json.RawMessage(`"just a string"`),
		json.RawMessage(`{not json at all`),
	} {
		if got := actors.WithRepositoryIdentity(input, "agentculture/culture-nodes"); string(got) != string(input) {
			t.Fatalf("input %s was rewritten to %s; a document with nowhere to put the key is left alone", input, got)
		}
	}
}
