// Package postgres implements internal/queue.Queue over a dedicated
// PostgreSQL table (queue_signals, migrations/0005_queue_signals.sql) so a
// local/single-node deployment needs no external queue product
// (docs/initial-design/culture-nodes-prd-spec.md §12.3).
//
// The driver is deliberately dumb: Receive is a plain, unlocked read of
// ready rows. It never claims, leases, fences, or otherwise mutates
// authoritative workflow state (work_items, node_runs, ...) -- that only
// ever happens through the store's fenced claim (task t7). Two callers
// receiving the same WorkRef, or a caller receiving a WorkRef and never
// acking it, are both harmless by design: see internal/queue's package doc
// for why receiving a signal never grants work.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentculture/culture-nodes/internal/queue"
)

// defaultPollInterval bounds how often Receive re-checks for ready signals
// while it is waiting -- long enough to avoid hammering PostgreSQL with a
// busy loop, short enough that a signal published just after a poll is
// still returned well within a caller's wait budget.
const defaultPollInterval = 100 * time.Millisecond

// Driver is a queue.Queue backed by the queue_signals table. It is safe for
// concurrent use: every method issues one self-contained statement (or, for
// Receive, a bounded sequence of them) against the pool.
type Driver struct {
	pool *pgxpool.Pool
}

var _ queue.Queue = (*Driver)(nil)

// New returns a Driver using pool. Callers typically obtain pool from
// (*postgres.Store).Pool() -- the driver has no other dependency on the
// store package, so it never needs typed store methods for its own table.
func New(pool *pgxpool.Pool) *Driver {
	return &Driver{pool: pool}
}

// Publish inserts a work signal row for ref, available immediately. It is
// idempotent by ref.WorkID: publishing the same WorkID twice (e.g. a relay
// retrying after a crash, see internal/events/relay.go) inserts at most one
// row, so a duplicate publish is a harmless no-op rather than a duplicate
// signal.
func (d *Driver) Publish(ctx context.Context, ref queue.WorkRef) error {
	switch {
	case ref.WorkID == "":
		return fmt.Errorf("queue/postgres: Publish: WorkID is required")
	case ref.NamespaceID == "":
		return fmt.Errorf("queue/postgres: Publish: NamespaceID is required")
	}

	_, err := d.pool.Exec(ctx, `
		INSERT INTO queue_signals (id, namespace_id, node_run_id, available_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (id) DO NOTHING
	`, ref.WorkID, ref.NamespaceID, ref.NodeRunID)
	if err != nil {
		return fmt.Errorf("queue/postgres: Publish: %w", err)
	}
	return nil
}

// Receive returns up to max ready deliveries (available_at <= now()),
// oldest-available first. If none are immediately ready, it polls on a
// ticker -- never a busy loop -- until one arrives or wait elapses, then
// returns whatever it has, which may be an empty, nil-error slice; that is
// the normal "nothing to do right now" outcome, not an error.
func (d *Driver) Receive(ctx context.Context, max int, wait time.Duration) ([]queue.Delivery, error) {
	if max <= 0 {
		max = 1
	}
	if wait < 0 {
		wait = 0
	}

	deliveries, err := d.receiveOnce(ctx, max)
	if err != nil || len(deliveries) > 0 || wait == 0 {
		return deliveries, err
	}

	interval := defaultPollInterval
	if wait < interval {
		interval = wait
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	deadline := time.NewTimer(wait)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, nil
		case <-ticker.C:
			deliveries, err := d.receiveOnce(ctx, max)
			if err != nil {
				return nil, err
			}
			if len(deliveries) > 0 {
				return deliveries, nil
			}
		}
	}
}

func (d *Driver) receiveOnce(ctx context.Context, max int) ([]queue.Delivery, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, namespace_id, node_run_id
		FROM queue_signals
		WHERE available_at <= now()
		ORDER BY available_at, id
		LIMIT $1
	`, max)
	if err != nil {
		return nil, fmt.Errorf("queue/postgres: Receive: %w", err)
	}
	defer rows.Close()

	var out []queue.Delivery
	for rows.Next() {
		var ref queue.WorkRef
		if err := rows.Scan(&ref.WorkID, &ref.NamespaceID, &ref.NodeRunID); err != nil {
			return nil, fmt.Errorf("queue/postgres: Receive: scan: %w", err)
		}
		out = append(out, queue.Delivery{WorkRef: ref, Receipt: ref.WorkID})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue/postgres: Receive: %w", err)
	}
	return out, nil
}

// Ack deletes d's signal row. Acking an already-acked (or never-existing)
// delivery affects zero rows and is not an error -- Ack is idempotent, as
// the queue.Queue contract requires.
func (d *Driver) Ack(ctx context.Context, del queue.Delivery) error {
	if del.Receipt == "" {
		return fmt.Errorf("queue/postgres: Ack: Receipt is required")
	}
	if _, err := d.pool.Exec(ctx, `DELETE FROM queue_signals WHERE id = $1`, del.Receipt); err != nil {
		return fmt.Errorf("queue/postgres: Ack: %w", err)
	}
	return nil
}

// Delay pushes d's signal availability out by delay (a negative delay is
// treated as zero). Delaying an already-acked (or never-existing) delivery
// affects zero rows and is not an error, matching Ack's idempotence.
func (d *Driver) Delay(ctx context.Context, del queue.Delivery, delay time.Duration) error {
	if del.Receipt == "" {
		return fmt.Errorf("queue/postgres: Delay: Receipt is required")
	}
	if delay < 0 {
		delay = 0
	}
	availableAt := time.Now().Add(delay)
	if _, err := d.pool.Exec(ctx,
		`UPDATE queue_signals SET available_at = $2 WHERE id = $1`,
		del.Receipt, availableAt,
	); err != nil {
		return fmt.Errorf("queue/postgres: Delay: %w", err)
	}
	return nil
}
