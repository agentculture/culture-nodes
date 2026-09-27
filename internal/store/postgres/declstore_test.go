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
