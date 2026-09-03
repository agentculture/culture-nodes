package postgres_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

func TestUpsertWorkerPresenceStoresOnlyActorKeyNames(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	row := postgres.WorkerPresence{
		WorkerID:  "worker-presence-store-test",
		Hostname:  "worker.example",
		Revision:  "abc123",
		ActorKeys: []string{"NODES_ACTOR_ALPHA", "NODES_ACTOR_BETA"},
	}
	if err := s.UpsertWorkerPresence(ctx, row); err != nil {
		t.Fatalf("UpsertWorkerPresence: %v", err)
	}

	var got []string
	if err := s.Pool().QueryRow(ctx,
		`SELECT actor_keys FROM worker_presence WHERE worker_id = $1`, row.WorkerID,
	).Scan(&got); err != nil {
		t.Fatalf("read worker presence: %v", err)
	}
	if strings.Join(got, ",") != strings.Join(row.ActorKeys, ",") {
		t.Fatalf("actor_keys = %q, want key names %q", got, row.ActorKeys)
	}
}

func TestOpenAPIExposesNoWorkerPresencePayloadBeforeT5B(t *testing.T) {
	doc, err := os.ReadFile("../../../api/openapi/openapi.yaml")
	if err != nil {
		t.Fatalf("read OpenAPI document: %v", err)
	}
	for _, forbidden := range []string{"worker_presence", "WorkerPresence"} {
		if strings.Contains(string(doc), forbidden) {
			t.Fatalf("OpenAPI unexpectedly exposes %q before the t5b API handler", forbidden)
		}
	}
}

func TestUpsertWorkerPresenceRejectsTokenShapedActorKeys(t *testing.T) {
	// Validation happens before Store touches its pool, so this security
	// boundary remains testable without PostgreSQL.
	var s *postgres.Store
	for _, actorKey := range []string{"NODES_ACTOR_KEY:token-value", strings.Repeat("x", 65)} {
		err := s.UpsertWorkerPresence(context.Background(), postgres.WorkerPresence{
			WorkerID:  "worker-rejected-actor-key",
			Hostname:  "worker.example",
			ActorKeys: []string{actorKey},
		})
		if err == nil {
			t.Fatalf("UpsertWorkerPresence accepted token-shaped actor key %q", actorKey)
		}
	}
}
