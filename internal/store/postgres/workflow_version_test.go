package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// TestCreateWorkflowVersionRejectsDuplicateDigest proves
// workflow_versions.content_digest is unique per namespace, and that
// Store.CreateWorkflowVersion surfaces that as the typed ErrDuplicateDigest
// rather than a raw driver error -- docs/initial-design/culture-nodes-prd-spec.md
// §11.3 requires an identical definition to always resolve to the same
// immutable version.
func TestCreateWorkflowVersionRejectsDuplicateDigest(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-wv-digest")

	digest := "sha256:" + store.NewULID()
	in := postgres.CreateWorkflowVersionInput{
		NamespaceID:   ns.ID,
		WorkflowKey:   "delivery-loop",
		Version:       1,
		SourceFormat:  "yaml",
		Source:        "entrypoint: intake\n",
		ContentDigest: digest,
	}

	first, err := s.CreateWorkflowVersion(ctx, in)
	if err != nil {
		t.Fatalf("CreateWorkflowVersion (first): %v", err)
	}
	if first.ContentDigest != digest {
		t.Fatalf("ContentDigest = %q, want %q", first.ContentDigest, digest)
	}
	if first.ID == "" {
		t.Fatal("CreateWorkflowVersion did not assign an ID")
	}

	dup := in
	dup.Version = 2 // a different version number must still collide on digest

	_, err = s.CreateWorkflowVersion(ctx, dup)
	if !errors.Is(err, postgres.ErrDuplicateDigest) {
		t.Fatalf("CreateWorkflowVersion (duplicate digest) error = %v, want ErrDuplicateDigest", err)
	}

	// The first version must be unaffected and still readable.
	got, err := s.GetWorkflowVersion(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetWorkflowVersion: %v", err)
	}
	if got.ID != first.ID || got.ContentDigest != digest {
		t.Fatalf("GetWorkflowVersion = %+v, want ID=%q ContentDigest=%q", got, first.ID, digest)
	}
}

func TestCreateWorkflowVersionAllowsSameDigestInDifferentNamespaces(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	nsA := mustNamespace(t, s, "test-wv-ns-a")
	nsB := mustNamespace(t, s, "test-wv-ns-b")
	digest := "sha256:" + store.NewULID()

	for _, ns := range []postgres.Namespace{nsA, nsB} {
		_, err := s.CreateWorkflowVersion(ctx, postgres.CreateWorkflowVersionInput{
			NamespaceID:   ns.ID,
			WorkflowKey:   "delivery-loop",
			Version:       1,
			SourceFormat:  "yaml",
			Source:        "entrypoint: intake\n",
			ContentDigest: digest,
		})
		if err != nil {
			t.Fatalf("CreateWorkflowVersion in namespace %s: %v", ns.Slug, err)
		}
	}
}

func TestGetWorkflowVersionNotFound(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	_, err := s.GetWorkflowVersion(ctx, "nonexistent-"+store.NewULID())
	if !errors.Is(err, postgres.ErrNotFound) {
		t.Fatalf("GetWorkflowVersion error = %v, want ErrNotFound", err)
	}
}
