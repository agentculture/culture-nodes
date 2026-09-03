package worker

import (
	"context"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

func (w *Worker) recordPresence(ctx context.Context) error {
	if err := w.db.UpsertWorkerPresence(ctx, postgres.WorkerPresence{
		WorkerID:  w.opts.WorkerID,
		Hostname:  w.opts.Hostname,
		Revision:  w.opts.Revision,
		ActorKeys: w.opts.ActorKeys,
		LastSeen:  w.opts.Now(),
	}); err != nil {
		return fmt.Errorf("worker: record presence: %w", err)
	}
	return nil
}
