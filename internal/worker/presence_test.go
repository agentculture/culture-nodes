package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

func TestWorkerWritesPresenceWithinOnePollInterval(t *testing.T) {
	s := testStore
	if s == nil {
		t.Skip("no PostgreSQL available: set NODES_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	ns, err := s.CreateNamespace(ctx, "worker-presence-"+store.NewULID(), "Worker Presence")
	if err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	eng, err := postgres.NewEngine(s, ns.ID)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	const workerID = "worker-presence-poll-test"
	wk, err := worker.New(s, eng, worker.Options{
		WorkerID:     workerID,
		NamespaceID:  ns.ID,
		Hostname:     "poll-host.example",
		Revision:     "revision-under-test",
		ActorKeys:    []string{"NODES_ACTOR_ALPHA"},
		PollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}

	started := time.Now()
	if _, err := wk.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	var hostname, revision string
	var actorKeys []string
	var lastSeen time.Time
	if err := s.Pool().QueryRow(ctx, `SELECT hostname, revision, actor_keys, last_seen FROM worker_presence WHERE worker_id = $1`, workerID).
		Scan(&hostname, &revision, &actorKeys, &lastSeen); err != nil {
		t.Fatalf("read worker presence after one poll: %v", err)
	}
	if hostname != "poll-host.example" || revision != "revision-under-test" {
		t.Fatalf("presence identity = (%q, %q), want configured hostname and revision", hostname, revision)
	}
	if len(actorKeys) != 1 || actorKeys[0] != "NODES_ACTOR_ALPHA" {
		t.Fatalf("actor_keys = %q, want names only", actorKeys)
	}
	if lastSeen.Before(started.Add(-time.Millisecond)) || lastSeen.After(started.Add(50*time.Millisecond)) {
		t.Fatalf("presence last_seen %s was not written within one poll interval", lastSeen)
	}
}
