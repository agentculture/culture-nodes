package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// Store-entry bindings (migration 0044, plan task t8, issue #192): the
// mapping step that makes a pulled entry runnable locally. These tests pin
// the properties the API route (internal/api/storebindings.go) and the
// worker registry's resolution fallback depend on:
//
//  1. a binding round-trips verbatim: required ref/kind, the bound
//     registration (row id + key), who bound it, when;
//  2. bindings are append-only records — re-binding the same ref appends a
//     second row, both stay readable, and the CURRENT binding is the
//     newest (CurrentBindings / ResolveStoreBoundActorKey);
//  3. reads are namespace-scoped, and an unbound ref is ErrNotFound.

// insertBindingActor inserts a minimal actor row to bind against —
// bindings FK actors(id), so a real row is required.
func insertBindingActor(t *testing.T, s *postgres.Store, nsID, key string) string {
	t.Helper()
	id := store.NewULID()
	_, err := s.Pool().Exec(context.Background(),
		`INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		 VALUES ($1, $2, $3, 1, 'agent', 'http')`, id, nsID, key)
	if err != nil {
		t.Fatalf("insert actor: %v", err)
	}
	return id
}

func TestStoreEntryBindingRoundTripAndTrail(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "storebinding-roundtrip").ID

	in := sampleStoreEntryInput(ns)
	in.Origin = "pulled"
	in.SourceRegistry = "https://nodes.thor.internal:8443"
	entry, err := s.CreateStoreEntry(ctx, in)
	if err != nil {
		t.Fatalf("CreateStoreEntry: %v", err)
	}

	firstActor := insertBindingActor(t, s, ns, "local/codex-first")
	secondActor := insertBindingActor(t, s, ns, "local/codex-second")

	const ref = "actor://codex@sha256:bbbb"
	first, err := s.CreateStoreEntryBinding(ctx, postgres.CreateStoreEntryBindingInput{
		NamespaceID:   ns,
		EntryID:       entry.ID,
		RequiredRef:   ref,
		RequiredKind:  "actor",
		BoundActorID:  firstActor,
		BoundActorKey: "local/codex-first",
		BoundBy:       "operator@spark",
	})
	if err != nil {
		t.Fatalf("CreateStoreEntryBinding: %v", err)
	}
	if first.RequiredRef != ref || first.RequiredKind != "actor" ||
		first.BoundActorID != firstActor || first.BoundActorKey != "local/codex-first" ||
		first.BoundBy != "operator@spark" || first.CreatedAt.IsZero() {
		t.Fatalf("binding did not round-trip verbatim: %+v", first)
	}

	// Re-binding the same ref APPENDS — the superseded record stays.
	second, err := s.CreateStoreEntryBinding(ctx, postgres.CreateStoreEntryBindingInput{
		NamespaceID:   ns,
		EntryID:       entry.ID,
		RequiredRef:   ref,
		RequiredKind:  "actor",
		BoundActorID:  secondActor,
		BoundActorKey: "local/codex-second",
		BoundBy:       "operator@spark",
	})
	if err != nil {
		t.Fatalf("re-bind: %v", err)
	}

	trail, err := s.ListStoreEntryBindings(ctx, ns, entry.ID)
	if err != nil {
		t.Fatalf("ListStoreEntryBindings: %v", err)
	}
	if len(trail) != 2 {
		t.Fatalf("trail has %d records, want 2 (bindings are append-only records): %+v", len(trail), trail)
	}
	if trail[0].ID != second.ID || trail[1].ID != first.ID {
		t.Fatalf("trail not newest-first: %+v", trail)
	}

	current := postgres.CurrentBindings(trail)
	if got := current[ref]; got.ID != second.ID || got.BoundActorKey != "local/codex-second" {
		t.Fatalf("current binding = %+v, want the newest (id %s)", got, second.ID)
	}

	// The dispatch-resolution read agrees with the trail's newest row.
	key, err := s.ResolveStoreBoundActorKey(ctx, ns, ref)
	if err != nil {
		t.Fatalf("ResolveStoreBoundActorKey: %v", err)
	}
	if key != "local/codex-second" {
		t.Fatalf("resolved key = %q, want local/codex-second", key)
	}
}

// TestStoreEntryBindingCrossEntryAgreement pins the resolution scope fix
// (PR #208 finding 4): bindings are entry-scoped records but the dispatch
// read is namespace-wide, so it answers only when every entry's CURRENT
// binding for the ref agrees — disagreement is ErrStoreBindingConflict
// naming each entry, never a silent newest-entry-wins pick that would let
// one pulled flow's mapping redirect another flow's dispatches.
func TestStoreEntryBindingCrossEntryAgreement(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "storebinding-agreement").ID

	newEntry := func(entryDigest string) string {
		in := sampleStoreEntryInput(ns)
		in.Origin = "pulled"
		in.SourceRegistry = "https://nodes.thor.internal:8443"
		in.EntryDigest = entryDigest
		entry, err := s.CreateStoreEntry(ctx, in)
		if err != nil {
			t.Fatalf("CreateStoreEntry: %v", err)
		}
		return entry.ID
	}
	entryA := newEntry("sha256:aaaa000000000000000000000000000000000000000000000000000000000000")
	entryB := newEntry("sha256:bbbb000000000000000000000000000000000000000000000000000000000000")

	oneActor := insertBindingActor(t, s, ns, "local/lane-one")
	twoActor := insertBindingActor(t, s, ns, "local/lane-two")

	const ref = "actor://codex@sha256:bbbb"
	bind := func(entryID, actorID, actorKey string) {
		t.Helper()
		if _, err := s.CreateStoreEntryBinding(ctx, postgres.CreateStoreEntryBindingInput{
			NamespaceID:   ns,
			EntryID:       entryID,
			RequiredRef:   ref,
			RequiredKind:  "actor",
			BoundActorID:  actorID,
			BoundActorKey: actorKey,
			BoundBy:       "operator@spark",
		}); err != nil {
			t.Fatalf("CreateStoreEntryBinding(%s -> %s): %v", entryID, actorKey, err)
		}
	}

	// Two entries agreeing on the key resolve to it.
	bind(entryA, oneActor, "local/lane-one")
	bind(entryB, oneActor, "local/lane-one")
	if key, err := s.ResolveStoreBoundActorKey(ctx, ns, ref); err != nil || key != "local/lane-one" {
		t.Fatalf("agreeing resolve = %q, %v; want local/lane-one", key, err)
	}

	// Entry B migrates to lane-two: the entries now DISAGREE, and the
	// namespace-wide read refuses with both entries named — even though
	// B's row is the globally newest, which the pre-fix query would have
	// silently answered with.
	bind(entryB, twoActor, "local/lane-two")
	_, err := s.ResolveStoreBoundActorKey(ctx, ns, ref)
	if !errors.Is(err, postgres.ErrStoreBindingConflict) {
		t.Fatalf("disagreeing resolve error = %v, want ErrStoreBindingConflict", err)
	}
	for _, name := range []string{entryA, entryB, "local/lane-one", "local/lane-two"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("conflict error does not name %s: %v", name, err)
		}
	}

	// The agreement set is each entry's CURRENT binding, one row per entry.
	current, err := s.CurrentStoreEntryBindingsByRef(ctx, ns, ref)
	if err != nil {
		t.Fatalf("CurrentStoreEntryBindingsByRef: %v", err)
	}
	if len(current) != 2 {
		t.Fatalf("current bindings = %d rows, want one per entry: %+v", len(current), current)
	}
	byEntry := map[string]string{}
	for _, b := range current {
		byEntry[b.EntryID] = b.BoundActorKey
	}
	if byEntry[entryA] != "local/lane-one" || byEntry[entryB] != "local/lane-two" {
		t.Fatalf("per-entry current bindings = %v", byEntry)
	}

	// Entry A follows the migration: agreement restored, the ref resolves.
	bind(entryA, twoActor, "local/lane-two")
	if key, err := s.ResolveStoreBoundActorKey(ctx, ns, ref); err != nil || key != "local/lane-two" {
		t.Fatalf("converged resolve = %q, %v; want local/lane-two", key, err)
	}
}

func TestStoreEntryBindingValidationAndScope(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "storebinding-validate").ID
	other := pgtest.MustNamespace(t, s, "storebinding-other").ID

	in := sampleStoreEntryInput(ns)
	in.Origin = "pulled"
	in.SourceRegistry = "https://nodes.thor.internal:8443"
	entry, err := s.CreateStoreEntry(ctx, in)
	if err != nil {
		t.Fatalf("CreateStoreEntry: %v", err)
	}
	actorID := insertBindingActor(t, s, ns, "local/codex")

	valid := postgres.CreateStoreEntryBindingInput{
		NamespaceID:   ns,
		EntryID:       entry.ID,
		RequiredRef:   "actor://codex@sha256:bbbb",
		RequiredKind:  "actor",
		BoundActorID:  actorID,
		BoundActorKey: "local/codex",
		BoundBy:       "operator@spark",
	}
	mutations := map[string]func(*postgres.CreateStoreEntryBindingInput){
		"namespace": func(i *postgres.CreateStoreEntryBindingInput) { i.NamespaceID = "" },
		"entry":     func(i *postgres.CreateStoreEntryBindingInput) { i.EntryID = "" },
		"ref":       func(i *postgres.CreateStoreEntryBindingInput) { i.RequiredRef = "" },
		"kind":      func(i *postgres.CreateStoreEntryBindingInput) { i.RequiredKind = "person" },
		"actor id":  func(i *postgres.CreateStoreEntryBindingInput) { i.BoundActorID = "" },
		"actor key": func(i *postgres.CreateStoreEntryBindingInput) { i.BoundActorKey = "" },
		"bound by":  func(i *postgres.CreateStoreEntryBindingInput) { i.BoundBy = "" },
	}
	for name, mutate := range mutations {
		bad := valid
		mutate(&bad)
		if _, err := s.CreateStoreEntryBinding(ctx, bad); err == nil {
			t.Errorf("CreateStoreEntryBinding accepted an input with a bad %s field", name)
		}
	}

	if _, err := s.CreateStoreEntryBinding(ctx, valid); err != nil {
		t.Fatalf("CreateStoreEntryBinding (valid): %v", err)
	}

	// Namespace scoping: the other namespace neither lists nor resolves it.
	if got, err := s.ListStoreEntryBindings(ctx, other, entry.ID); err != nil || len(got) != 0 {
		t.Fatalf("other namespace lists %d bindings (err %v), want 0", len(got), err)
	}
	if _, err := s.ResolveStoreBoundActorKey(ctx, other, valid.RequiredRef); !errors.Is(err, postgres.ErrNotFound) {
		t.Fatalf("cross-namespace resolve error = %v, want ErrNotFound", err)
	}
	// An unbound ref is ErrNotFound, not an empty answer.
	if _, err := s.ResolveStoreBoundActorKey(ctx, ns, "actor://nobody@sha256:0000"); !errors.Is(err, postgres.ErrNotFound) {
		t.Fatalf("unbound ref resolve error = %v, want ErrNotFound", err)
	}
	// CurrentActorByKey: the newest revision answers; an unknown key is
	// ErrNotFound.
	a, err := s.CurrentActorByKey(ctx, ns, "local/codex")
	if err != nil || a.ID != actorID {
		t.Fatalf("CurrentActorByKey = %+v, %v; want row %s", a, err, actorID)
	}
	if _, err := s.CurrentActorByKey(ctx, ns, "local/nobody"); !errors.Is(err, postgres.ErrNotFound) {
		t.Fatalf("unknown actor key error = %v, want ErrNotFound", err)
	}
}

func TestStoreEntryBindingRefusesCrossEntryConflictByName(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "storebinding-conflict").ID

	mkEntry := func(name string) string {
		t.Helper()
		in := sampleStoreEntryInput(ns)
		in.Name = name
		in.Origin = "pulled"
		in.SourceRegistry = "https://nodes.thor.internal:8443"
		// Identity is (namespace, origin, entry_digest): a distinct digest
		// per entry, or ON CONFLICT DO NOTHING hands back the same row.
		in.EntryDigest = "sha256:" + fmt.Sprintf("%064x", len(name)+int(name[len(name)-1]))
		entry, err := s.CreateStoreEntry(ctx, in)
		if err != nil {
			t.Fatalf("CreateStoreEntry %s: %v", name, err)
		}
		return entry.ID
	}
	entryA, entryB := mkEntry("flow-a"), mkEntry("flow-b")
	actorOne := insertBindingActor(t, s, ns, "local/one")
	insertBindingActor(t, s, ns, "local/two")

	const ref = "actor://codex@sha256:cccc"
	bind := func(entryID, actorID, actorKey string) error {
		_, err := s.CreateStoreEntryBinding(ctx, postgres.CreateStoreEntryBindingInput{
			NamespaceID: ns, EntryID: entryID, RequiredRef: ref, RequiredKind: "actor",
			BoundActorID: actorID, BoundActorKey: actorKey, BoundBy: "act_test_operator",
		})
		return err
	}
	if err := bind(entryA, actorOne, "local/one"); err != nil {
		t.Fatalf("first binding: %v", err)
	}
	// Same ref, DIFFERENT actor, from another entry: the namespace-wide
	// newest-wins dispatch lookup would silently flip — refused by name.
	err := bind(entryB, insertBindingActor(t, s, ns, "local/three"), "local/three")
	if !errors.Is(err, postgres.ErrStoreBindingConflict) {
		t.Fatalf("cross-entry different-actor binding = %v, want ErrStoreBindingConflict", err)
	}
	// Same ref, SAME actor, from another entry: unambiguous, allowed.
	if err := bind(entryB, actorOne, "local/one"); err != nil {
		t.Fatalf("cross-entry same-actor binding should be allowed: %v", err)
	}
}
