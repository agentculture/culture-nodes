package postgres_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// Flow-store catalog entries (migration 0042, plan task t7, issue #192).
// These tests pin the properties the registry API route
// (internal/api/storeentries.go) depends on:
//
//  1. an entry round-trips graph digest + embedded source + the full
//     evidence manifest (proving run ids, deviation records, capability
//     requirements) verbatim — full fidelity, the q6 decision;
//  2. identity is (namespace, origin, entry_digest): re-adding identical
//     content is idempotent, never duplicated;
//  3. the collision rule is structural: a pulled entry coexists with a
//     locally-authored one — even for the same name and same content — and
//     ingesting a pulled entry never touches the local row (#192
//     acceptance (b));
//  4. a pulled entry must say where it came from; reads are
//     namespace-scoped and an unknown id is ErrNotFound.

func sampleStoreEntryInput(namespaceID string) postgres.CreateStoreEntryInput {
	return postgres.CreateStoreEntryInput{
		NamespaceID:       namespaceID,
		Name:              "jira-intake",
		Origin:            "local",
		GraphDigest:       "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		GraphSourceFormat: "yaml",
		GraphSource:       "name: jira-intake\nspec: {}\n",
		Evidence: postgres.EvidenceManifest{
			ProvingRunIDs: []string{"01RUNAAAAAAAAAAAAAAAAAAAAA", "01RUNBBBBBBBBBBBBBBBBBBBBB"},
			DeviationRecords: []postgres.DeviationRecordRef{
				{Ref: "docs/deviations/2026-08-18-example.md", Note: "watermark re-arm bounded by hand"},
			},
			RequiredCapabilities: []postgres.CapabilityRequirement{
				{Kind: "actor", Ref: "actor://codex@sha256:bbbb", Capabilities: []string{"shell", "workspace-write"}},
				{Kind: "runner", Ref: "runner://headspace", Capabilities: []string{"git"}},
			},
		},
		EntryDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	}
}

func TestStoreEntryRoundTrip(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "storeentry-roundtrip").ID

	in := sampleStoreEntryInput(ns)
	created, err := s.CreateStoreEntry(ctx, in)
	if err != nil {
		t.Fatalf("CreateStoreEntry: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created entry has no id")
	}
	if created.CreatedAt.IsZero() {
		t.Fatal("created entry has a zero created_at")
	}

	got, err := s.GetStoreEntry(ctx, ns, created.ID)
	if err != nil {
		t.Fatalf("GetStoreEntry: %v", err)
	}
	if got.Name != in.Name || got.Origin != in.Origin || got.GraphDigest != in.GraphDigest ||
		got.GraphSourceFormat != in.GraphSourceFormat || got.GraphSource != in.GraphSource ||
		got.EntryDigest != in.EntryDigest || got.SourceRegistry != "" {
		t.Fatalf("round-trip mismatch: got %+v, want fields of %+v", got, in)
	}
	// Full fidelity: the manifest comes back verbatim, value for value.
	if !reflect.DeepEqual(got.Evidence, in.Evidence) {
		t.Fatalf("evidence manifest not verbatim:\n got  %+v\n want %+v", got.Evidence, in.Evidence)
	}
}

func TestStoreEntryIdempotentByDigest(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "storeentry-idem").ID

	in := sampleStoreEntryInput(ns)
	first, err := s.CreateStoreEntry(ctx, in)
	if err != nil {
		t.Fatalf("CreateStoreEntry (first): %v", err)
	}
	second, err := s.CreateStoreEntry(ctx, in)
	if err != nil {
		t.Fatalf("CreateStoreEntry (second): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("re-adding identical content duplicated the entry: %s vs %s", second.ID, first.ID)
	}
}

func TestStoreEntryLocalAndPulledCoexist(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "storeentry-coexist").ID

	local, err := s.CreateStoreEntry(ctx, sampleStoreEntryInput(ns))
	if err != nil {
		t.Fatalf("CreateStoreEntry (local): %v", err)
	}

	// The hardest collision: a pulled entry with the SAME name and the SAME
	// entry digest as the locally-authored one. It must coexist as its own
	// row, never resolve to (or replace) the local one.
	pulledIn := sampleStoreEntryInput(ns)
	pulledIn.Origin = "pulled"
	pulledIn.SourceRegistry = "https://nodes.thor.internal:8443"
	pulled, err := s.CreateStoreEntry(ctx, pulledIn)
	if err != nil {
		t.Fatalf("CreateStoreEntry (pulled): %v", err)
	}
	if pulled.ID == local.ID {
		t.Fatal("pulled entry resolved to the local row: pulling shadows local authorship")
	}
	if pulled.SourceRegistry != pulledIn.SourceRegistry {
		t.Fatalf("pulled source_registry = %q, want %q", pulled.SourceRegistry, pulledIn.SourceRegistry)
	}

	// The local row is untouched by the pull.
	localAgain, err := s.GetStoreEntry(ctx, ns, local.ID)
	if err != nil {
		t.Fatalf("GetStoreEntry (local, after pull): %v", err)
	}
	if !reflect.DeepEqual(localAgain, local) {
		t.Fatalf("local entry changed after a pull:\n before %+v\n after  %+v", local, localAgain)
	}

	// Both are listed under the shared name.
	listed, err := s.ListStoreEntries(ctx, ns, "jira-intake")
	if err != nil {
		t.Fatalf("ListStoreEntries: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d entries under the shared name, want 2 (local + pulled): %+v", len(listed), listed)
	}
	origins := map[string]bool{}
	for _, e := range listed {
		origins[e.Origin] = true
	}
	if !origins["local"] || !origins["pulled"] {
		t.Fatalf("listing does not show both origins: %+v", origins)
	}
}

func TestStoreEntryPulledRequiresSourceRegistry(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "storeentry-pullsrc").ID

	in := sampleStoreEntryInput(ns)
	in.Origin = "pulled"
	in.SourceRegistry = ""
	if _, err := s.CreateStoreEntry(ctx, in); err == nil {
		t.Fatal("a pulled entry with no source registry was accepted")
	}
}

func TestStoreEntryValidation(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "storeentry-validation").ID

	for name, mutate := range map[string]func(*postgres.CreateStoreEntryInput){
		"missing namespace":    func(in *postgres.CreateStoreEntryInput) { in.NamespaceID = "" },
		"missing name":         func(in *postgres.CreateStoreEntryInput) { in.Name = "" },
		"missing graph digest": func(in *postgres.CreateStoreEntryInput) { in.GraphDigest = "" },
		"missing graph source": func(in *postgres.CreateStoreEntryInput) { in.GraphSource = "" },
		"missing entry digest": func(in *postgres.CreateStoreEntryInput) { in.EntryDigest = "" },
		"unknown origin":       func(in *postgres.CreateStoreEntryInput) { in.Origin = "remote" },
	} {
		in := sampleStoreEntryInput(ns)
		mutate(&in)
		if _, err := s.CreateStoreEntry(ctx, in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStoreEntryNotFoundAndNamespaceScope(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "storeentry-scope").ID
	other := pgtest.MustNamespace(t, s, "storeentry-scope-other").ID

	created, err := s.CreateStoreEntry(ctx, sampleStoreEntryInput(ns))
	if err != nil {
		t.Fatalf("CreateStoreEntry: %v", err)
	}

	if _, err := s.GetStoreEntry(ctx, ns, "01JUNKJUNKJUNKJUNKJUNKJUNK"); !errors.Is(err, postgres.ErrNotFound) {
		t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
	}
	// The same id from another namespace must not resolve.
	if _, err := s.GetStoreEntry(ctx, other, created.ID); !errors.Is(err, postgres.ErrNotFound) {
		t.Fatalf("cross-namespace read: err = %v, want ErrNotFound", err)
	}

	listed, err := s.ListStoreEntries(ctx, other, "")
	if err != nil {
		t.Fatalf("ListStoreEntries (other namespace): %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("another namespace lists %d entries, want 0", len(listed))
	}
}

func TestListStoreEntriesNameFilter(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "storeentry-list").ID

	a := sampleStoreEntryInput(ns)
	if _, err := s.CreateStoreEntry(ctx, a); err != nil {
		t.Fatalf("CreateStoreEntry (a): %v", err)
	}
	b := sampleStoreEntryInput(ns)
	b.Name = "pr-upkeep"
	b.EntryDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	if _, err := s.CreateStoreEntry(ctx, b); err != nil {
		t.Fatalf("CreateStoreEntry (b): %v", err)
	}

	all, err := s.ListStoreEntries(ctx, ns, "")
	if err != nil {
		t.Fatalf("ListStoreEntries (all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered list has %d entries, want 2", len(all))
	}

	only, err := s.ListStoreEntries(ctx, ns, "pr-upkeep")
	if err != nil {
		t.Fatalf("ListStoreEntries (filtered): %v", err)
	}
	if len(only) != 1 || only[0].Name != "pr-upkeep" {
		t.Fatalf("filtered list = %+v, want exactly the pr-upkeep entry", only)
	}
}
