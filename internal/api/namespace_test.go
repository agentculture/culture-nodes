package api_test

import (
	"context"
	"sync"
	"testing"

	"github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store"
)

// TestEnsureNamespace_Idempotent proves the guarantee its own doc comment
// makes -- "a second call with the same slug returns the same id" -- for a
// straightforward sequential case: create, then look up.
//
// deploy/helm's api and worker Deployments both depend on exactly this: a
// worker resolves NODES_NAMESPACE_SLUG the identical way (see
// cmd/nodes/worker.go), and whichever process asks first must not leave the
// other pointed at a different namespace id.
func TestEnsureNamespace_Idempotent(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	slug := "ensure-namespace-" + store.NewULID()

	first, err := api.EnsureNamespace(ctx, s, slug, "First")
	if err != nil {
		t.Fatalf("EnsureNamespace (create): %v", err)
	}
	if first == "" {
		t.Fatalf("EnsureNamespace (create) returned an empty id")
	}

	second, err := api.EnsureNamespace(ctx, s, slug, "Second — should be ignored, the row already exists")
	if err != nil {
		t.Fatalf("EnsureNamespace (lookup): %v", err)
	}
	if second != first {
		t.Fatalf("EnsureNamespace: second call returned %q, want the first call's id %q", second, first)
	}
}

// TestEnsureNamespace_ConcurrentRace proves the property that actually
// matters for a Helm-deployed api + worker(s) racing to resolve the same
// slug at pod startup with no ordering guarantee between them: every
// concurrent caller ends up with the same namespace id, never a mix of two.
func TestEnsureNamespace_ConcurrentRace(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	slug := "ensure-namespace-race-" + store.NewULID()

	const callers = 8
	ids := make([]string, callers)
	errs := make([]error, callers)

	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = api.EnsureNamespace(ctx, s, slug, "Race Namespace")
		}(i)
	}
	wg.Wait()

	want := ids[0]
	if want == "" {
		t.Fatalf("caller 0 got an empty id (err: %v)", errs[0])
	}
	for i, id := range ids {
		if errs[i] != nil {
			t.Fatalf("caller %d: EnsureNamespace: %v", i, errs[i])
		}
		if id != want {
			t.Fatalf("caller %d got id %q, want %q (every concurrent caller must agree on one namespace)", i, id, want)
		}
	}
}
