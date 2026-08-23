package worker_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// TestDBRegistryResolvesThroughStoreBinding pins the dispatch-time half of
// the store-pull mapping step (task t8, issue #192): a pulled flow's graph
// pins a ref minted on the source plane; no local registration answers for
// its key, but the newest store binding for the VERBATIM ref supplies the
// local key — so the flow dispatches without the graph document ever being
// rewritten. An unbound foreign ref stays ErrUnknownActor, and a binding
// never overrides a direct registration.
func TestDBRegistryResolvesThroughStoreBinding(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "registry-binding")
	ctx := context.Background()

	// The importing plane's own registration — its key shares nothing with
	// the pinned ref below.
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref)
		VALUES ('actor_binding_local', $1, 'local/dev-lane', 1, 'agent', 'nodes.actor/v1alpha1', 'http://127.0.0.1:7001')
	`, ns.ID); err != nil {
		t.Fatalf("insert local actor: %v", err)
	}

	// The pulled entry the binding hangs off (bindings FK store_entries).
	entry, err := s.CreateStoreEntry(ctx, postgres.CreateStoreEntryInput{
		NamespaceID:       ns.ID,
		Name:              "edge-order",
		Origin:            "pulled",
		SourceRegistry:    "https://nodes.source.internal:8443",
		GraphDigest:       "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		GraphSourceFormat: "yaml",
		GraphSource:       "name: edge-order\nspec: {}\n",
		Evidence: postgres.EvidenceManifest{
			ProvingRunIDs: []string{"01RUNAAAAAAAAAAAAAAAAAAAAA"},
			RequiredCapabilities: []postgres.CapabilityRequirement{
				{Kind: "actor", Ref: "actor://source/dev@sha256:cafe", Capabilities: []string{"shell"}},
			},
		},
		EntryDigest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	})
	if err != nil {
		t.Fatalf("CreateStoreEntry: %v", err)
	}

	const pinnedRef = "actor://source/dev@sha256:cafe"
	if _, err := s.CreateStoreEntryBinding(ctx, postgres.CreateStoreEntryBindingInput{
		NamespaceID:   ns.ID,
		EntryID:       entry.ID,
		RequiredRef:   pinnedRef,
		RequiredKind:  "actor",
		BoundActorID:  "actor_binding_local",
		BoundActorKey: "local/dev-lane",
		BoundBy:       "operator@spark",
	}); err != nil {
		t.Fatalf("CreateStoreEntryBinding: %v", err)
	}

	r, err := worker.NewDBRegistry(s, ns.ID)
	if err != nil {
		t.Fatalf("NewDBRegistry: %v", err)
	}

	endpoint, err := r.Resolve(ctx, pinnedRef)
	if err != nil {
		t.Fatalf("Resolve through binding: %v", err)
	}
	if endpoint.URL != "http://127.0.0.1:7001" {
		t.Fatalf("resolved endpoint = %q, want the bound registration's", endpoint.URL)
	}

	// Attribution follows the binding too: the dispatch is the bound
	// registration's, not unattributed.
	id, err := r.ActorRowID(ctx, pinnedRef)
	if err != nil {
		t.Fatalf("ActorRowID through binding: %v", err)
	}
	if id != "actor_binding_local" {
		t.Fatalf("ActorRowID = %q, want actor_binding_local", id)
	}

	// An unbound foreign ref is still unknown.
	if _, err := r.Resolve(ctx, "actor://source/other@sha256:beef"); !errors.Is(err, worker.ErrUnknownActor) {
		t.Fatalf("Resolve(unbound) error = %v, want ErrUnknownActor", err)
	}
	if _, err := r.ActorRowID(ctx, "actor://source/other@sha256:beef"); !errors.Is(err, worker.ErrUnknownActor) {
		t.Fatalf("ActorRowID(unbound) error = %v, want ErrUnknownActor", err)
	}

	// A direct registration outranks any binding: resolving the local key
	// itself never consults the bindings table.
	direct, err := r.Resolve(ctx, "actor://local/dev-lane")
	if err != nil || direct.URL != "http://127.0.0.1:7001" {
		t.Fatalf("direct Resolve = %+v, %v", direct, err)
	}

	// A SECOND entry binding the same ref to a different local key makes
	// the namespace-wide lookup ambiguous: resolution refuses, naming the
	// conflict, rather than dispatching this flow on the other entry's
	// newer mapping (PR #208 finding 4) — and attribution refuses the same
	// way, so an attempt can never be attributed to an actor Resolve would
	// not have dispatched to.
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref)
		VALUES ('actor_binding_other', $1, 'local/other-lane', 1, 'agent', 'nodes.actor/v1alpha1', 'http://127.0.0.1:7002')
	`, ns.ID); err != nil {
		t.Fatalf("insert second local actor: %v", err)
	}
	otherEntry, err := s.CreateStoreEntry(ctx, postgres.CreateStoreEntryInput{
		NamespaceID:       ns.ID,
		Name:              "edge-order-second",
		Origin:            "pulled",
		SourceRegistry:    "https://nodes.source.internal:8443",
		GraphDigest:       "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		GraphSourceFormat: "yaml",
		GraphSource:       "name: edge-order-second\nspec: {}\n",
		Evidence: postgres.EvidenceManifest{
			ProvingRunIDs: []string{"01RUNBBBBBBBBBBBBBBBBBBBBB"},
			RequiredCapabilities: []postgres.CapabilityRequirement{
				{Kind: "actor", Ref: pinnedRef, Capabilities: []string{"shell"}},
			},
		},
		EntryDigest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
	})
	if err != nil {
		t.Fatalf("CreateStoreEntry (second): %v", err)
	}
	// Raw insert: CreateStoreEntryBinding refuses this join at create time
	// (ErrStoreBindingConflict), so a conflicting row can only exist from a
	// race or from before the guard — which is exactly the state the
	// resolution-side agreement check defends against.
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO store_entry_bindings (id, namespace_id, entry_id, required_ref, required_kind,
			bound_actor_id, bound_actor_key, bound_by)
		VALUES ('binding_conflicting_row', $1, $2, $3, 'actor', 'actor_binding_other', 'local/other-lane', 'operator@spark')
	`, ns.ID, otherEntry.ID, pinnedRef); err != nil {
		t.Fatalf("insert conflicting binding row: %v", err)
	}
	if _, err := r.Resolve(ctx, pinnedRef); !errors.Is(err, worker.ErrUnknownActor) ||
		!strings.Contains(err.Error(), "different local actors") {
		t.Fatalf("Resolve(conflicting) error = %v, want ErrUnknownActor naming the conflict", err)
	}
	if _, err := r.ActorRowID(ctx, pinnedRef); !errors.Is(err, worker.ErrUnknownActor) ||
		!strings.Contains(err.Error(), "different local actors") {
		t.Fatalf("ActorRowID(conflicting) error = %v, want ErrUnknownActor naming the conflict", err)
	}
}
