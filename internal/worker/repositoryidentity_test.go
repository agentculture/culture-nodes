package worker_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// The repository identity on dispatch (task t1, issue #125).
//
// A trigger-created run's input IS the event payload, and a payload carries
// no checkout: which repository an actor works in is a fact about the
// DEPLOYMENT, so it lives on the actor's registration and reaches the bridge
// from there. These tests drive the real DBRegistry over a real actors row —
// a hand-rolled registry would prove the plumbing against a seam no
// deployment uses (the same reason withRegisteredActor exists for the
// clarify gate).

// dispatchedInput is the input document the actor was handed on its single
// invocation.
func dispatchedInput(t *testing.T, h *harness) map[string]any {
	t.Helper()
	invocations := h.invocations()
	if len(invocations) != 1 {
		t.Fatalf("actor was invoked %d times, want 1 (worker errors: %v)", len(invocations), h.workerErrors())
	}
	var input map[string]any
	if err := json.Unmarshal(invocations[0].Input, &input); err != nil {
		t.Fatalf("dispatch input is not a JSON object: %v\nraw: %s", err, invocations[0].Input)
	}
	return input
}

// Acceptance 1: a registered actor carrying the key has it delivered.
func TestDispatchCarriesTheRegisteredRepositoryIdentity(t *testing.T) {
	pgtest.RequireStore(t, testStore)
	h := newHarness(t, completesSynchronously,
		withRegisteredActor(t, `{}`, `{"repository_identity":"agentculture/culture-nodes"}`))

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	input := dispatchedInput(t, h)
	if input[actors.RepositoryIdentityKey] != "agentculture/culture-nodes" {
		t.Fatalf("dispatch input %s = %v, want the identity the registration declares",
			actors.RepositoryIdentityKey, input[actors.RepositoryIdentityKey])
	}
	// The node's own binding is untouched: the identity is added beside it,
	// never in place of it.
	if input["subject"] != "widget" {
		t.Fatalf("the node's resolved binding did not survive: %v", input)
	}
}

// Acceptance 2: an actor whose registration says nothing about a repository
// dispatches exactly as it does today — no key, no new required field.
func TestDispatchWithoutARegisteredIdentityIsUnchanged(t *testing.T) {
	pgtest.RequireStore(t, testStore)
	h := newHarness(t, completesSynchronously, withRegisteredActor(t, `{}`, `{}`))

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	input := dispatchedInput(t, h)
	if _, present := input[actors.RepositoryIdentityKey]; present {
		t.Fatalf("an actor that declares no repository identity was sent one: %v", input)
	}
	if len(input) != 1 || input["subject"] != "widget" {
		t.Fatalf("dispatch input = %v, want exactly the node's resolved binding", input)
	}
}

// Acceptance 3, the half that matters most: the identity comes from the
// registry and from nowhere else. This run's input names a repository twice
// — once the way a GitHub event payload does (`repository`) and once under
// the identity key itself — and neither is what the control plane sends.
func TestRunInputCannotInfluenceTheDispatchedRepositoryIdentity(t *testing.T) {
	pgtest.RequireStore(t, testStore)
	h := newHarness(t, completesSynchronously,
		withRegisteredActor(t, `{}`, `{"repository_identity":"agentculture/culture-nodes"}`))

	run := h.createRun("sync.workflow.yaml",
		`{"subject":"widget","repository":"attacker/owned","repository_identity":"attacker/owned"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	input := dispatchedInput(t, h)
	if input[actors.RepositoryIdentityKey] != "agentculture/culture-nodes" {
		t.Fatalf("dispatch input %s = %v, want the registry's identity to win over the run input's",
			actors.RepositoryIdentityKey, input[actors.RepositoryIdentityKey])
	}
	// The payload's own `repository` field is data the node was bound to. It
	// travels as it always did; it simply is not an identity.
	if input["repository"] != "attacker/owned" {
		t.Fatalf("the payload's own repository field was rewritten: %v", input["repository"])
	}
}

// The same acceptance where there is no registration to win the argument: an
// unregistered identity in a run input is not passed on, because a bridge
// reading the identity key must never be reading a payload.
func TestRunInputIdentityIsStrippedWhenTheActorDeclaresNone(t *testing.T) {
	pgtest.RequireStore(t, testStore)
	h := newHarness(t, completesSynchronously, withRegisteredActor(t, `{}`, `{}`))

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget","repository_identity":"attacker/owned"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	input := dispatchedInput(t, h)
	if _, present := input[actors.RepositoryIdentityKey]; present {
		t.Fatalf("a run input's repository identity reached the actor: %v", input)
	}
	if input["subject"] != "widget" {
		t.Fatalf("the rest of the node's resolved binding did not survive: %v", input)
	}
}

// The registry half on its own: the identity is read from the CURRENT
// registration, the same highest-revision read the endpoint and the auth
// token come from.
func TestDBRegistryResolvesTheRepositoryIdentityFromMetadata(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "registry-repository")
	ctx := context.Background()

	for _, row := range []struct {
		id, key  string
		revision int
		metadata string
	}{
		{"actor_repo_rev1", "company/multilane", 1, `{"repository_identity":"agentculture/old-lane"}`},
		{"actor_repo_rev2", "company/multilane", 2, `{"repository_identity":"agentculture/culture-nodes"}`},
		{"actor_repo_none", "company/silent", 1, `{"auth_token_env":""}`},
	} {
		if _, err := s.Pool().Exec(ctx, `
			INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref, metadata)
			VALUES ($1, $2, $3, $4, 'agent', 'nodes.actor/v1alpha1', 'http://127.0.0.1:1', $5)
		`, row.id, ns.ID, row.key, row.revision, row.metadata); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}

	r, err := worker.NewDBRegistry(s, ns.ID)
	if err != nil {
		t.Fatalf("NewDBRegistry: %v", err)
	}

	endpoint, err := r.Resolve(ctx, "actor://company/multilane")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if endpoint.RepositoryIdentity != "agentculture/culture-nodes" {
		t.Fatalf("RepositoryIdentity = %q, want the highest revision's identity", endpoint.RepositoryIdentity)
	}

	silent, err := r.Resolve(ctx, "actor://company/silent")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if silent.RepositoryIdentity != "" {
		t.Fatalf("RepositoryIdentity = %q for a registration that declares none, want empty", silent.RepositoryIdentity)
	}
}
