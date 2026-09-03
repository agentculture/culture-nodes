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
	ns := mustNamespace(t, s, "worker-presence")
	row := postgres.WorkerPresence{
		NamespaceID: ns.ID,
		WorkerID:    "worker-presence-store-test",
		Hostname:    "worker.example",
		Revision:    "abc123",
		ActorKeys:   []string{"NODES_ACTOR_ALPHA", "NODES_ACTOR_BETA"},
	}
	if err := s.UpsertWorkerPresence(ctx, row); err != nil {
		t.Fatalf("UpsertWorkerPresence: %v", err)
	}

	var got []string
	if err := s.Pool().QueryRow(ctx,
		`SELECT actor_keys FROM worker_presence WHERE namespace_id = $1 AND worker_id = $2`,
		ns.ID, row.WorkerID,
	).Scan(&got); err != nil {
		t.Fatalf("read worker presence: %v", err)
	}
	if strings.Join(got, ",") != strings.Join(row.ActorKeys, ",") {
		t.Fatalf("actor_keys = %q, want key names %q", got, row.ActorKeys)
	}
}

// Presence is namespace-scoped state, not a deployment-wide observation
// (PR #292 review): the same worker id in two namespaces is two rows, and
// neither upsert may overwrite the other's hostname, revision or actor keys.
func TestUpsertWorkerPresenceKeepsNamespacesSeparate(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	first := mustNamespace(t, s, "worker-presence-a")
	second := mustNamespace(t, s, "worker-presence-b")
	const workerID = "worker-shared-id"

	for _, row := range []postgres.WorkerPresence{
		{NamespaceID: first.ID, WorkerID: workerID, Hostname: "thor", Revision: "aaa"},
		{NamespaceID: second.ID, WorkerID: workerID, Hostname: "orin", Revision: "bbb"},
	} {
		if err := s.UpsertWorkerPresence(ctx, row); err != nil {
			t.Fatalf("UpsertWorkerPresence(%s): %v", row.NamespaceID, err)
		}
	}

	for _, want := range []struct{ namespaceID, hostname, revision string }{
		{first.ID, "thor", "aaa"},
		{second.ID, "orin", "bbb"},
	} {
		var hostname, revision string
		if err := s.Pool().QueryRow(ctx,
			`SELECT hostname, revision FROM worker_presence WHERE namespace_id = $1 AND worker_id = $2`,
			want.namespaceID, workerID,
		).Scan(&hostname, &revision); err != nil {
			t.Fatalf("read worker presence for %s: %v", want.namespaceID, err)
		}
		if hostname != want.hostname || revision != want.revision {
			t.Fatalf("presence for %s = (%q, %q), want (%q, %q) -- one namespace overwrote the other",
				want.namespaceID, hostname, revision, want.hostname, want.revision)
		}
	}
}

// The namespace is required, and like every other validation it is refused
// before Store touches its pool.
func TestUpsertWorkerPresenceRequiresNamespace(t *testing.T) {
	var s *postgres.Store
	err := s.UpsertWorkerPresence(context.Background(), postgres.WorkerPresence{
		WorkerID: "worker-without-namespace",
		Hostname: "worker.example",
	})
	if err == nil {
		t.Fatal("UpsertWorkerPresence accepted a presence row with no namespace id")
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
			NamespaceID: "namespace-rejected-actor-key",
			WorkerID:    "worker-rejected-actor-key",
			Hostname:    "worker.example",
			ActorKeys:   []string{actorKey},
		})
		if err == nil {
			t.Fatalf("UpsertWorkerPresence accepted token-shaped actor key %q", actorKey)
		}
	}
}
