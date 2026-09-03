package main

import (
	"context"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

func TestMeshTargetsReportsRegisteredActorWithoutToken(t *testing.T) {
	db := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, db, "serve-mesh-targets")
	_, err := db.Pool().Exec(context.Background(), `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref, metadata)
		VALUES ($1, $2, 'company/no-token', 1, 'agent', 'http', 'http://bridge.test',
		        '{"auth_token_env":"NODES_ACTOR_NO_TOKEN_TOKEN"}')`, store.NewULID(), ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := meshTargets(context.Background(), db, ns.ID, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || !strings.Contains(targets[0].Error, "no bearer configured") {
		t.Fatalf("targets = %#v, want one unobserved target with no bearer configured", targets)
	}
}
