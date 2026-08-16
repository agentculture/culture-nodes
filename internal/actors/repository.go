package actors

import "encoding/json"

// RepositoryIdentityKey is the §13.1 input key a dispatch carries the actor's
// registered repository identity under (task t1, issue #125).
//
// It is a NAME, never a path. The engine supplies a stable repository
// identity and lets the actor's own host resolve it to a checkout — the
// decision recorded in
// docs/design/2026-08-15-t16-workspace-provisioner-and-t18-handover-gate.md,
// where handing a path from the control plane was rejected because a path
// chosen on this host need not exist on the actor's (issue #74).
//
// It is deliberately NOT `repo`, which is the checkout path a workflow author
// may bind explicitly and every bridge validates against its allowlist. The
// two answer different questions — "which repository is this actor's lane"
// versus "which directory did the caller name" — and merging them would make
// an identity indistinguishable from a path the moment a bridge read either.
const RepositoryIdentityKey = "repository_identity"

// WithRepositoryIdentity returns the dispatch input with the actor's
// REGISTERED repository identity as the only value under
// RepositoryIdentityKey.
//
// The identity is a deployment fact held in the actor registry — the shape
// `metadata.handover_remote` already ships in — so this function is the one
// place it enters an invocation, and it is authoritative in both directions:
//
//   - an actor whose registration declares an identity has it SET here, over
//     any value the input already carried;
//   - an actor whose registration declares none has the key REMOVED, so a run
//     input, an event payload or an upstream node's output cannot put one
//     there.
//
// The removal is the half that is easy to leave out and expensive to leave
// out. A trigger-created run's input is the event payload verbatim, and a
// GitHub pull-request payload names a repository of its own (the pr-upkeep
// example's contract requires `repository`); a bridge that resolved a
// checkout from whatever arrived under this key would be taking a checkout
// instruction from the event that triggered the run. The identity comes from
// the registry or it does not come at all.
//
// Everything else in the document is left exactly as the node's bindings
// resolved it, including a payload's own `repository` field: that is data the
// node was bound to, and rewriting it would corrupt the actor's input to make
// a point about a different key.
//
// Two shapes cannot hold the key and are returned untouched, for the reasons
// workspace.go's own merge states: an input that is not a JSON object (an
// array or a scalar — rewriting it would corrupt what the binding resolved)
// and one that is not valid JSON at all. An actor that declares an identity
// and receives such an input gets no identity, and refuses the dispatch with
// its own named error, which is a better diagnostic than one invented here.
// An empty or null input becomes an object holding only the identity.
func WithRepositoryIdentity(input json.RawMessage, identity string) json.RawMessage {
	if isEmptyJSON(input) {
		if identity == "" {
			return input
		}
		merged, err := json.Marshal(map[string]string{RepositoryIdentityKey: identity})
		if err != nil {
			return input
		}
		return merged
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		// Not a JSON object: there is nowhere to put the key, and nowhere a
		// forged one could be hiding either.
		return input
	}
	if _, present := fields[RepositoryIdentityKey]; !present && identity == "" {
		// The common case, and the one acceptance 2 is about: nothing to add
		// and nothing to remove, so the bytes the binding resolved are the
		// bytes that go on the wire.
		return input
	}
	if identity == "" {
		delete(fields, RepositoryIdentityKey)
	} else {
		encoded, err := json.Marshal(identity)
		if err != nil {
			return input
		}
		fields[RepositoryIdentityKey] = encoded
	}
	merged, err := json.Marshal(fields)
	if err != nil {
		return input
	}
	return merged
}
