package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// WorkerPresence is one worker process's latest liveness observation.
// ActorKeys contains environment-variable names only, never their values.
type WorkerPresence struct {
	NamespaceID string
	WorkerID    string
	Hostname    string
	Revision    string
	ActorKeys   []string
	LastSeen    time.Time
}

// UpsertWorkerPresence records the latest poll by a worker.
func (s *Store) UpsertWorkerPresence(ctx context.Context, presence WorkerPresence) error {
	if strings.TrimSpace(presence.NamespaceID) == "" {
		return fmt.Errorf("postgres: worker presence requires namespace id")
	}
	if strings.TrimSpace(presence.WorkerID) == "" {
		return fmt.Errorf("postgres: worker presence requires worker id")
	}
	if strings.TrimSpace(presence.Hostname) == "" {
		return fmt.Errorf("postgres: worker presence requires hostname")
	}
	for _, name := range presence.ActorKeys {
		if name == "" || strings.Contains(name, ":") || len(name) > 64 {
			return fmt.Errorf("postgres: actor key %q is not a safe key name", name)
		}
	}
	if presence.ActorKeys == nil {
		presence.ActorKeys = []string{}
	}
	if presence.LastSeen.IsZero() {
		presence.LastSeen = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO worker_presence (namespace_id, worker_id, hostname, revision, actor_keys, last_seen)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (namespace_id, worker_id) DO UPDATE SET
    hostname = EXCLUDED.hostname,
    revision = EXCLUDED.revision,
    actor_keys = EXCLUDED.actor_keys,
    last_seen = EXCLUDED.last_seen`, presence.NamespaceID, presence.WorkerID, presence.Hostname, presence.Revision, presence.ActorKeys, presence.LastSeen)
	if err != nil {
		return fmt.Errorf("postgres: upsert worker presence: %w", err)
	}
	return nil
}
