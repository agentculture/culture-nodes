package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

func TestDeclarationVersionsAndLinks(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "declarations")
	a, err := s.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: ns.ID, Name: "intake", Body: []byte(`{"trigger":"one"}`), Author: "human:alice"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: ns.ID, Name: "intake", Body: []byte(`{"trigger":"two"}`), Author: "human:alice"})
	if err != nil {
		t.Fatal(err)
	}
	if a.DeclarationID != b.DeclarationID || a.Version != 1 || b.Version != 2 || a.Digest == b.Digest {
		t.Fatalf("versions: %+v %+v", a, b)
	}
	secondNamespace := mustNamespace(t, s, "declarations-other")
	sameName, err := s.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: secondNamespace.ID, Name: "intake", Body: []byte(`{"trigger":"one"}`), Author: "human:bob"})
	if err != nil || sameName.DeclarationID == a.DeclarationID {
		t.Fatalf("namespace-scoped name: %+v %v", sameName, err)
	}
	old, err := s.GetDeclarationVersion(ctx, a.ID)
	if err != nil || old.Digest != a.Digest {
		t.Fatalf("old version: %+v %v", old, err)
	}
	other, err := s.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: ns.ID, Name: "review", Body: []byte(`{"trigger":"three"}`), Author: "human:alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"must", "can"} {
		if err := s.LinkDeclarations(ctx, ns.ID, other.DeclarationID, a.DeclarationID, kind); err != nil {
			t.Fatal(err)
		}
	}
	links, err := s.ListDeclarationLinks(ctx, ns.ID, other.DeclarationID)
	if err != nil || len(links) != 2 {
		t.Fatalf("links: %+v %v", links, err)
	}
}

// TestListNamespaceDeclarationLinksReturnsAllEdges is the store-level half
// of task t20 (#328): the focus route builds the whole graph in memory, so
// it needs every link in the namespace regardless of which endpoint it was
// queried by -- unlike ListDeclarationLinks, which is scoped to one "from".
func TestListNamespaceDeclarationLinksReturnsAllEdges(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "declaration-graph-links")

	names := []string{"a", "b", "c"}
	ids := map[string]string{}
	for _, name := range names {
		v, err := s.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: ns.ID, Name: name, Body: []byte(`{"n":"` + name + `"}`), Author: "human:alice"})
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = v.DeclarationID
	}
	if err := s.LinkDeclarations(ctx, ns.ID, ids["a"], ids["b"], "must"); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkDeclarations(ctx, ns.ID, ids["c"], ids["b"], "can"); err != nil {
		t.Fatal(err)
	}

	// A second namespace's links must never leak into the first's listing.
	other := mustNamespace(t, s, "declaration-graph-links-other")
	ov, err := s.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: other.ID, Name: "x", Body: []byte(`{}`), Author: "human:alice"})
	if err != nil {
		t.Fatal(err)
	}
	ov2, err := s.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: other.ID, Name: "y", Body: []byte(`{}`), Author: "human:alice"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LinkDeclarations(ctx, other.ID, ov.DeclarationID, ov2.DeclarationID, "must"); err != nil {
		t.Fatal(err)
	}

	links, err := s.ListNamespaceDeclarationLinks(ctx, ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 2 {
		t.Fatalf("links: %+v", links)
	}
	seen := map[[3]string]bool{}
	for _, l := range links {
		seen[[3]string{l.FromDeclarationID, l.ToDeclarationID, l.Kind}] = true
	}
	if !seen[[3]string{ids["a"], ids["b"], "must"}] || !seen[[3]string{ids["c"], ids["b"], "can"}] {
		t.Fatalf("missing expected edges: %+v", links)
	}
}

func TestAliasCycleNamesThreeAliases(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "aliases")
	v, err := s.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: ns.ID, Name: "anchor", Body: []byte(`{"a":1}`), Author: "human:alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if _, err := s.CreateDeclarationAlias(ctx, ns.ID, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddDeclarationToAlias(ctx, ns.ID, "a", v.DeclarationID); err != nil {
		t.Fatal(err)
	}
	members, err := s.ListDeclarationAliasMembers(ctx, ns.ID, "a")
	if err != nil || len(members) != 1 || members[0] != v.DeclarationID {
		t.Fatalf("alias members: %v %v", members, err)
	}
	for _, move := range [][2]string{{"b", "a"}, {"c", "b"}} {
		if err := s.MoveDeclarationAlias(ctx, ns.ID, move[0], move[1], v.ID, "human:alice"); err != nil {
			t.Fatal(err)
		}
	}
	// The moves must store the REQUESTED parent (a regression guard: the walk
	// once advanced the variable the new parent pointed at, storing the root).
	var parentOfC string
	if err := s.Pool().QueryRow(ctx, `SELECT p.name FROM declaration_aliases c JOIN declaration_aliases p ON p.id=c.parent_alias_id
		WHERE c.namespace_id=$1 AND c.name='c'`, ns.ID).Scan(&parentOfC); err != nil || parentOfC != "b" {
		t.Fatalf("parent of c = %q (%v), want b", parentOfC, err)
	}
	err = s.MoveDeclarationAlias(ctx, ns.ID, "a", "c", v.ID, "human:alice")
	if err == nil || !strings.Contains(err.Error(), "a -> c -> b -> a") {
		t.Fatalf("cycle = %v", err)
	}
	history, err := s.ListDeclarationHistory(ctx, ns.ID)
	if err != nil || len(history) != 2 {
		t.Fatalf("rejected move wrote history: %+v %v", history, err)
	}
}

func TestActivationAndAliasHistoryAppendOnly(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "history")
	v, err := s.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: ns.ID, Name: "work", Body: []byte(`{"a":1}`), Author: "human:alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		if _, err := s.CreateDeclarationAlias(ctx, ns.ID, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordDeclarationActivation(ctx, ns.ID, v.ID, "activate", "human:alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveDeclarationAlias(ctx, ns.ID, "one", "two", v.ID, "declaration:"+v.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDeclarationActivation(ctx, ns.ID, v.ID, "deactivate", "human:alice", ""); err != nil {
		t.Fatal(err)
	}
	history, err := s.ListDeclarationHistory(ctx, ns.ID)
	if err != nil || len(history) != 3 {
		t.Fatalf("history: %+v %v", history, err)
	}
	for i, kind := range []string{"activate", "alias_move", "deactivate"} {
		if history[i].Kind != kind || history[i].TargetVersionID != v.ID || history[i].Actor == "" {
			t.Fatalf("history[%d]: %+v", i, history[i])
		}
	}
	if history[1].AliasName == nil || *history[1].AliasName != "one" || history[1].NewParentName == nil || *history[1].NewParentName != "two" {
		t.Fatalf("move: %+v", history[1])
	}
	if _, err := s.Pool().Exec(ctx, `UPDATE declaration_history SET actor = 'tampered' WHERE id = $1`, history[0].ID); err == nil {
		t.Fatal("history row was mutable")
	}
	if err := s.MoveDeclarationAliasWithSupersedes(ctx, ns.ID, "one", "", v.ID, "human:alice", history[1].ID); err != nil {
		t.Fatal(err)
	}
	history, err = s.ListDeclarationHistory(ctx, ns.ID)
	if err != nil || len(history) != 4 || history[3].SupersedesID == nil || *history[3].SupersedesID != history[1].ID || history[3].OldParentName == nil || *history[3].OldParentName != "two" || history[3].NewParentName != nil {
		t.Fatalf("correction history: %+v %v", history, err)
	}
}
