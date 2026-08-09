package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentculture/culture-nodes/internal/queue"
)

// defaultRelayBatchSize bounds how many outbox rows one Relay.runBatch
// transaction processes. It is deliberately modest: the transaction stays
// open for the duration of every row's queue.Publish and EventSink call in
// the batch (see Relay's doc comment), so a smaller batch keeps that lock
// window shorter at the cost of more round trips.
const defaultRelayBatchSize = 50

// EventSink receives every event a Relay successfully hands off -- e.g. to
// fan out to an SSE stream, an in-memory pub/sub, or an external event bus.
// It is called before the outbox row is marked published in the same
// batch transaction (see Relay.runBatch), so a Sink that itself fails
// aborts that row's publication for retry, exactly like a failed
// queue.Publish.
type EventSink func(ctx context.Context, env Envelope) error

// RelayOptions configures a Relay. The zero value is valid: BatchSize
// defaults to defaultRelayBatchSize and Source defaults to "nodes".
type RelayOptions struct {
	BatchSize int
	Source    string
}

// Relay is the sole publisher of outbox rows (prd-spec §12.5, §12.3): the
// only process that reads unpublished rows from the outbox table and turns
// each one into both a published Envelope (via EventSink) and a
// queue.WorkRef signal (via queue.Queue.Publish).
//
// # At-least-once, by design
//
// Relay.runBatch calls queue.Publish and EventSink for a row *before*
// committing the transaction that marks it published. That ordering is
// deliberate: it guarantees a row is never marked published without both
// hand-offs having already succeeded, but it also means a crash between a
// successful hand-off and the commit leaves the row still 'pending' --
// the next Relay.Run will process it again, calling queue.Publish and
// EventSink a second time for the same row.
//
// This is at-least-once delivery, not exactly-once, and it is intentional:
// the alternative (mark published first, publish second) can silently drop
// a publication on a crash, which is the failure task t10 exists to make
// unrepairable-by-construction impossible. Every downstream consumer of
// events and queue signals is expected to tolerate duplicates -- see
// internal/queue's package doc for why a duplicate work signal is harmless,
// and CloudEvents' own recommendation that consumers de-duplicate by id.
//
// # Stable event IDs make duplicates cheap to recognize
//
// The Envelope built for an outbox row always reuses that row's own id as
// the event ID (never a freshly minted one -- unlike New, which always
// mints fresh). The same is true of the WorkRef.WorkID handed to
// queue.Publish. That means a re-publish after a crash produces the exact
// same event ID and WorkID as the original attempt, so a consumer that
// tracks "have I seen this id" sees a harmless repeat, not a new event with
// no way to tell it apart from the first.
//
// # Exactly-once marking
//
// While delivery is at-least-once, the outbox row's own status transition
// is exactly-once: runBatch processes a bounded batch inside one
// transaction and commits it only after every row in the batch has been
// successfully handed off, so a row's status flips from 'pending' to
// 'published' exactly one time, no matter how many times the hand-off
// itself was retried across crashes.
type Relay struct {
	pool      *pgxpool.Pool
	queue     queue.Queue
	sink      EventSink
	batchSize int
	source    string
}

// NewRelay returns a Relay reading outbox rows from pool, publishing work
// signals through q, and handing off events to sink.
func NewRelay(pool *pgxpool.Pool, q queue.Queue, sink EventSink, opts RelayOptions) *Relay {
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultRelayBatchSize
	}
	source := opts.Source
	if source == "" {
		source = defaultSource
	}
	return &Relay{pool: pool, queue: q, sink: sink, batchSize: batchSize, source: source}
}

// Run drains every currently pending outbox row, oldest first (by id, which
// for the ULIDs this system mints is also insertion order), in bounded
// batches, until none remain or ctx is done.
//
// Run returning nil means "nothing pending right now," a normal and
// expected outcome -- it does not mean the relay has shut down. A caller
// that wants a persistent background relay is expected to call Run
// repeatedly (e.g. on a ticker, from `nodes scheduler` mode); Run itself
// does not loop forever so that a single call is a bounded, easily-tested
// unit of work.
//
// If ctx is canceled or a batch's queue.Publish/EventSink hand-off fails,
// Run returns that error immediately; the batch in flight at that point
// rolls back in full (see runBatch), so every row in it -- including any
// already handed off earlier in that same batch -- stays 'pending' for the
// next Run call to retry.
func (r *Relay) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.runBatch(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

// outboxRow is the subset of outbox's columns runBatch needs.
type outboxRow struct {
	id          string
	namespaceID string
	topic       string
	payload     json.RawMessage
}

// runBatch selects up to r.batchSize pending, available outbox rows (oldest
// first, FOR UPDATE SKIP LOCKED so concurrent Relay instances never process
// the same row twice), hands each to queue.Publish then r.sink in that
// order, marks it published, and commits the whole batch atomically: a
// failure partway through rolls back every row in the batch, not just the
// one that failed, so a row is never left half-published. It returns the
// number of rows committed as published.
func (r *Relay) runBatch(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("events: relay: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	rows, err := tx.Query(ctx, `
		SELECT id, namespace_id, topic, payload
		FROM outbox
		WHERE status = 'pending' AND available_at <= now()
		ORDER BY id
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, r.batchSize)
	if err != nil {
		return 0, fmt.Errorf("events: relay: select batch: %w", err)
	}

	var batch []outboxRow
	for rows.Next() {
		var row outboxRow
		if err := rows.Scan(&row.id, &row.namespaceID, &row.topic, &row.payload); err != nil {
			rows.Close()
			return 0, fmt.Errorf("events: relay: scan: %w", err)
		}
		batch = append(batch, row)
	}
	scanErr := rows.Err()
	rows.Close()
	if scanErr != nil {
		return 0, fmt.Errorf("events: relay: select batch: %w", scanErr)
	}

	if len(batch) == 0 {
		return 0, nil
	}

	for _, row := range batch {
		env, err := r.envelopeFromOutbox(row)
		if err != nil {
			return 0, fmt.Errorf("events: relay: build envelope for outbox row %s: %w", row.id, err)
		}
		ref := workRefFromOutbox(row)

		if err := r.queue.Publish(ctx, ref); err != nil {
			return 0, fmt.Errorf("events: relay: publish work ref for outbox row %s: %w", row.id, err)
		}
		if err := r.sink(ctx, env); err != nil {
			return 0, fmt.Errorf("events: relay: sink event for outbox row %s: %w", row.id, err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE outbox SET status = 'published', published_at = now() WHERE id = $1`,
			row.id,
		); err != nil {
			return 0, fmt.Errorf("events: relay: mark outbox row %s published: %w", row.id, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("events: relay: commit batch: %w", err)
	}
	return len(batch), nil
}

// envelopeFromOutbox builds the Envelope for row, reusing row.id as the
// event ID rather than minting a fresh one -- see Relay's doc comment on
// why that stability across a crash-retry matters.
func (r *Relay) envelopeFromOutbox(row outboxRow) (Envelope, error) {
	env := Envelope{
		ID:     row.id,
		Source: r.source,
		Type:   row.topic,
		Data:   row.payload,
	}
	env.applyDefaults()
	if err := env.Validate(); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// workRefFromOutbox builds the queue.WorkRef for row, reusing row.id as the
// WorkID for the same crash-retry stability reason as envelopeFromOutbox.
// Every outbox row produces a WorkRef, even ones without a node_run_id in
// their payload (e.g. a run.completed event): publishing a reference to
// work that turns out not to exist is harmless, because receiving a signal
// never grants work on its own (see internal/queue's package doc).
func workRefFromOutbox(row outboxRow) queue.WorkRef {
	return queue.WorkRef{
		WorkID:      row.id,
		NodeRunID:   nodeRunIDFromPayload(row.payload),
		NamespaceID: row.namespaceID,
	}
}

// nodeRunIDFromPayload best-effort extracts a "node_run_id" field from an
// outbox row's payload. A payload without one (wrong shape, or an event
// that legitimately has no associated node run) yields "", not an error.
func nodeRunIDFromPayload(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var parsed struct {
		NodeRunID string `json:"node_run_id"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return ""
	}
	return parsed.NodeRunID
}
