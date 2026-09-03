package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/events"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// eventPollLimit bounds how many rows one poll iteration reads, so a run
// with a very long backlog streams in bounded chunks rather than one huge
// flush.
const eventPollLimit = 200

// terminalRunEventTypes are the run-level event types after which no
// further event will ever be appended for a run — engine.CompleteAttempt's
// every terminating branch (completeRun, failRun, cancel, failBound) emits
// exactly one of these as its last act for the run. handleStreamRunEvents
// closes the stream once it has sent one, since polling further could only
// ever find nothing new.
var terminalRunEventTypes = map[string]bool{
	engine.TypeRunCompleted: true,
	engine.TypeRunFailed:    true,
	engine.TypeRunCancelled: true,
	engine.TypeRunBounded:   true,
}

// eventRow is one events table row.
type eventRow struct {
	ID         string
	Sequence   int64
	Type       string
	Source     string
	Data       json.RawMessage
	OccurredAt time.Time
}

// pollEvents reads committed events newer than afterSequence for one run,
// in order — the same events table internal/engine appends to inside the
// PRD §12.5 transaction, so a row this method reads was never anything but
// committed.
func (s *Server) pollEvents(ctx context.Context, runID string, afterSequence int64) ([]eventRow, error) {
	rows, err := s.Store.Pool().Query(ctx, `
		SELECT id, sequence, event_type, source, data, occurred_at
		FROM events
		WHERE namespace_id = $1 AND aggregate_id = $2 AND sequence > $3
		ORDER BY sequence
		LIMIT $4`,
		s.NamespaceID, runID, afterSequence, eventPollLimit)
	if err != nil {
		return nil, fmt.Errorf("api: poll events for run %s: %w", runID, err)
	}
	defer rows.Close()

	out := make([]eventRow, 0)
	for rows.Next() {
		var row eventRow
		if err := rows.Scan(&row.ID, &row.Sequence, &row.Type, &row.Source, &row.Data, &row.OccurredAt); err != nil {
			return nil, fmt.Errorf("api: poll events for run %s: scan: %w", runID, err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// handleStreamRunEvents is GET /v1alpha1/runs/{id}/events: the SSE stream
// of a run's committed events (PRD §15.1). It manages its own response
// lifecycle rather than going through (*Server).wrap: once the first byte
// of an event-stream response is written, a failure can no longer be
// reported as a JSON error body with its own status line — only a
// pre-stream failure (an unknown run, a ResponseWriter that cannot be
// flushed) still renders the documented Error shape.
func (s *Server) handleStreamRunEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	if _, err := s.engineStore.Run(ctx, id); err != nil {
		s.writeAPIError(w, r, classify(err))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeAPIError(w, r, internalError(errors.New("response writer does not support flushing")))
		return
	}

	after := resumeSequence(r)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx's response buffering, if fronted by one
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	keepalive := time.NewTicker(s.keepaliveInterval)
	defer keepalive.Stop()

	for {
		rows, err := s.pollEvents(ctx, id, after)
		if err != nil {
			// The stream is already open; there is no JSON error channel
			// left to report through. Stop quietly — the client's
			// Last-Event-ID lets it resume from `after` once whatever
			// transient failure this was clears.
			return
		}

		for _, row := range rows {
			if !writeSSEEvent(w, id, row) {
				return
			}
			after = row.Sequence
			if terminalRunEventTypes[row.Type] {
				flusher.Flush()
				return
			}
		}
		flusher.Flush()
		if len(rows) > 0 {
			keepalive.Reset(s.keepaliveInterval) // "idle" is measured from the last real frame
		}

		if !waitForNextPoll(ctx, ticker.C, keepalive.C, w, flusher) {
			return
		}
	}
}

// waitForNextPoll blocks until the next poll tick, writing an SSE comment
// line (": keepalive") and flushing on every keepalive tick that lands in
// between (task t3). It reports false when the stream must stop: the
// request context is done (the client disconnected), or a keepalive write
// failed, which means the same thing. A comment line is the one SSE frame
// every consumer is required to ignore, so it carries no id, no event name
// and no payload — a client's resume cursor is untouched by it.
func waitForNextPoll(ctx context.Context, poll, keepalive <-chan time.Time, w http.ResponseWriter, flusher http.Flusher) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-poll:
			return true
		case <-keepalive:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return false
			}
			flusher.Flush()
		}
	}
}

// resumeSequence reads the client's resume point: the Last-Event-ID header
// (what a reconnecting browser EventSource sends automatically, carrying
// the last `id:` field it received) if present, otherwise the `from` query
// parameter — for a client's very first connection, which cannot set
// Last-Event-ID before it has ever received an id to send. Either absent or
// unparsable means "from the beginning of this run's event log".
func resumeSequence(r *http.Request) int64 {
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n
		}
	}
	if raw := r.URL.Query().Get("from"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// writeSSEEvent renders one committed event as
// "id: <sequence>\nevent: <type>\ndata: <envelope JSON>\n\n" and reports
// whether the write succeeded — false means the client is gone and the
// stream should stop.
func writeSSEEvent(w http.ResponseWriter, runID string, row eventRow) bool {
	env := events.Envelope{
		ID:              row.ID,
		Source:          row.Source,
		SpecVersion:     "1.0",
		Type:            row.Type,
		Subject:         runID,
		Time:            row.OccurredAt,
		DataContentType: "application/json",
		Data:            row.Data,
	}
	body, err := json.Marshal(env)
	if err != nil {
		return true // skip an unmarshalable row rather than killing the whole stream over it
	}
	_, writeErr := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", row.Sequence, row.Type, body)
	return writeErr == nil
}

// --- Cross-run stream (task t17) ---
//
// handleStreamEvents below is the fan-in companion to handleStreamRunEvents
// above: one SSE connection carrying committed events across every run in
// the namespace, instead of one connection per run. It exists for task
// t18's live-mesh view, which needs pulses for every run currently on the
// board without opening N ever-growing polling loops in one browser tab.
//
// crossRunEventPollLimit is deliberately larger than eventPollLimit: this
// scan crosses every run in the namespace rather than filtering to one via
// an equality match on aggregate_id, so a poll needs a wider raw window to
// make steady forward progress once real traffic has more than a couple
// hundred committed rows between polls.
const crossRunEventPollLimit = 500

// runLifecycleEventTypes are the run-level event types handleStreamEvents
// always admits, regardless of active-run scope: terminalRunEventTypes (a
// run ending) plus TypeRunCreated (a run beginning) — together the "runs
// appear/finish" pulses a mesh consumer needs to keep its board in sync,
// even for a run it is not otherwise watching the interior of.
var runLifecycleEventTypes = map[string]bool{
	engine.TypeRunCreated:   true,
	engine.TypeRunCompleted: true,
	engine.TypeRunFailed:    true,
	engine.TypeRunCancelled: true,
	engine.TypeRunBounded:   true,
}

// crossRunEventRow is one events table row read for the cross-run stream —
// eventRow plus the aggregate_id (run id) that row belongs to, which a
// per-run poll already knows from its own path parameter and never needs
// to select.
type crossRunEventRow struct {
	ID         string
	Sequence   int64
	RunID      string
	Type       string
	Source     string
	Data       json.RawMessage
	OccurredAt time.Time
}

// pollCrossRunEvents reads up to crossRunEventPollLimit committed events
// newer than afterID (a row's `id`, the events table's ULID primary key),
// across every run in the namespace, in id order. This is the raw window
// handleStreamEvents applies its active-run/lifecycle scope to — bounding
// the *raw* scan here, rather than the post-filter result count, is what
// keeps one poll's cost constant regardless of how much of the namespace's
// history is currently terminal: a poll that lands entirely on filtered-out
// rows still does at most crossRunEventPollLimit units of index work, never
// an open-ended scan hunting for the next row that happens to match.
//
// The query is scoped to aggregate_type = 'run': every event this codebase
// emits today carries that aggregate_type (internal/store/postgres's
// engine_store.go and async.go both hardcode it), but stating the filter
// explicitly, rather than relying on that being the only value ever
// written, keeps this endpoint's meaning — run events — stable if a future
// aggregate type is ever added to the same table.
func (s *Server) pollCrossRunEvents(ctx context.Context, afterID string) ([]crossRunEventRow, error) {
	rows, err := s.Store.Pool().Query(ctx, `
		SELECT id, sequence, aggregate_id, event_type, source, data, occurred_at
		FROM events
		WHERE namespace_id = $1 AND aggregate_type = 'run' AND id > $2
		ORDER BY id
		LIMIT $3`,
		s.NamespaceID, afterID, crossRunEventPollLimit)
	if err != nil {
		return nil, fmt.Errorf("api: poll cross-run events: %w", err)
	}
	defer rows.Close()

	out := make([]crossRunEventRow, 0)
	for rows.Next() {
		var row crossRunEventRow
		if err := rows.Scan(&row.ID, &row.Sequence, &row.RunID, &row.Type, &row.Source, &row.Data, &row.OccurredAt); err != nil {
			return nil, fmt.Errorf("api: poll cross-run events: scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// activeRunIDs returns the ids of every run in the namespace not yet in one
// of postgres.TerminalRunStatuses() — the default scope handleStreamEvents
// applies (risk r1's containment): a mesh consumer watching "what's live"
// sees every event a still-running run produces, without being handed a
// completed run's full internal history just because it is asking
// cross-run instead of opening that one run's own stream.
//
// It is called at most once per poll iteration (and skipped entirely when
// either the poll found nothing or an explicit runs= filter is in play —
// see handleStreamEvents), so its cost is one indexed lookup per poll
// interval, not per event.
func (s *Server) activeRunIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.Store.Pool().Query(ctx,
		`SELECT id FROM runs WHERE namespace_id = $1 AND status = ANY($2::text[])`,
		s.NamespaceID, postgres.ActiveRunStatuses())
	if err != nil {
		return nil, fmt.Errorf("api: active run ids: %w", err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("api: active run ids: scan: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// runsFilterParam parses the `runs=id,id` query parameter into an explicit
// scope set. An empty/absent parameter (or one that names nothing after
// trimming) returns (nil, false) — "no explicit filter", telling the caller
// to fall back to the active-runs+lifecycle default instead.
func runsFilterParam(r *http.Request) (map[string]bool, bool) {
	raw := r.URL.Query().Get("runs")
	if raw == "" {
		return nil, false
	}
	out := make(map[string]bool)
	for _, id := range strings.Split(raw, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = true
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// resumeEventID reads the client's cross-run resume point: the
// Last-Event-ID header (what a reconnecting browser EventSource sends
// automatically, carrying the last `id:` field it received) if present,
// otherwise the `from` query parameter for a client's very first
// connection. `from=latest` requests a fresh snapshot cursor; any other
// absent value means "from the beginning of this namespace's event log".
// Unlike resumeSequence above, the cursor here is the events table's own
// primary key (a ULID string), not a per-run sequence — see
// handleStreamEvents' ordering doc for why no per-run sequence can serve a
// cross-run resume point.
func resumeEventID(r *http.Request) (after string, latest bool) {
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		return raw, false
	}
	raw := r.URL.Query().Get("from")
	return raw, raw == "latest"
}

// latestEventID captures the namespace-wide events-table high-water mark.
// A tail-only stream starts polling strictly after this committed id.
func (s *Server) latestEventID(ctx context.Context) (string, error) {
	var id string
	if err := s.Store.Pool().QueryRow(ctx,
		`SELECT COALESCE(max(id), '') FROM events WHERE namespace_id = $1`,
		s.NamespaceID).Scan(&id); err != nil {
		return "", fmt.Errorf("api: latest cross-run event id: %w", err)
	}
	return id, nil
}

// crossRunEventInScope applies handleStreamEvents' scoping rule to one raw
// row. An explicit runs= filter (explicit == true) is a closed membership
// test against scope, full stop — active/terminal status plays no part
// once the caller has named exactly which runs it wants. The default
// (explicit == false) admits a run-lifecycle event type unconditionally,
// for any run, and otherwise admits the row only if its run was active as
// of this poll's activeRunIDs snapshot.
func crossRunEventInScope(row crossRunEventRow, scope map[string]bool, explicit bool, active map[string]bool) bool {
	if explicit {
		return scope[row.RunID]
	}
	if runLifecycleEventTypes[row.Type] {
		return true
	}
	return active[row.RunID]
}

// handleStreamEvents is GET /v1alpha1/events: the cross-run companion to
// handleStreamRunEvents above (PRD §15.1's stream, generalized across
// runs). Like that handler it manages its own response lifecycle rather
// than going through (*Server).wrap, for the same reason: once the first
// byte of an event-stream response is written, a failure can no longer be
// reported as a JSON error body with its own status line.
//
// Scope: by default, only events belonging to an ACTIVE (non-terminal) run
// plus run-lifecycle events (run.created, run.completed, run.failed,
// run.cancelled, run.bounded) for any run — so a mesh consumer sees runs
// appear and finish on its board without ever being handed a completed
// run's full internal history it never asked to watch. An explicit
// `runs=id,id` query parameter overrides that default entirely: the stream
// then reports only events for exactly the listed run ids, active or not
// (useful for a consumer that wants to keep watching one specific run,
// terminal or not, alongside the default cross-run feed for everything
// else — two connections, each scoped the way it needs).
//
// Risk r1 containment (docs/plans/2026-08-12-operate-through-the-ui.md,
// "fleet-wide event stream load"): this is a coarse-cadence, bounded-scan
// poll, not a per-run fan-out, by design rather than by accident:
//
//   - Cadence: this handler polls at 2x the per-run stream's interval
//     (s.pollInterval, see WithPollInterval) — one cross-run connection
//     already carries every displayed run's pulses in a single poll, so it
//     does not need the same aggressive default a single-run consumer
//     wants; halving the polling frequency halves this endpoint's
//     steady-state query rate at the cost of at most one extra interval of
//     latency per event.
//   - Bounded scan: pollCrossRunEvents reads at most crossRunEventPollLimit
//     raw rows per poll, ordered by the events table's own id (its primary
//     key), indexed by events_namespace_id_id_idx (migrations/
//     0014_events_namespace_id_index.sql) — regardless of how many of them
//     match the active-run/lifecycle scope. The cursor (after) always
//     advances to the last raw row read in the poll, even when every row
//     in that window was filtered out — so a poll landing entirely on a
//     terminal run's old backlog still makes forward progress in one
//     bounded step, rather than re-scanning the same filtered rows on
//     every subsequent poll forever. Documented catch-up behavior: under a
//     heavy backlog of filtered-out rows, a client may see several
//     visibly-empty polls go by while the cursor works through them before
//     the next admitted event arrives — it never sees an unbounded stall,
//     and the server never needs a bigger LIMIT to get there, only more
//     polls.
//   - The active-run set is fetched fresh once per poll (activeRunIDs), not
//     cached across polls or connections, so a run going terminal
//     mid-connection stops being treated as active starting the very next
//     poll. That does not lose events: terminalRunEventTypes' own guarantee
//     (see its doc comment) is that a run's terminal event is the *last*
//     event ever appended for it, and that type is always in
//     runLifecycleEventTypes, so it is admitted unconditionally regardless
//     of which side of that poll boundary it fell on.
//
// Ordering and resume semantics (honesty condition — no invented
// guarantee): events.id is a ULID minted by internal/store.NewULID, whose
// generator (internal/store/ulid.go) is a single mutex-protected,
// per-process counter, so ids are strictly increasing in generation order
// within one running control-plane process. That makes `ORDER BY id` a
// genuine total order across every run this process ever wrote an event
// for, and `id` a valid resume cursor for it — exactly what this endpoint
// promises. It does NOT promise a causally-ordered, cross-process global
// sequence the way a distributed vector clock or a single shared counter
// would: two ids minted by two independent control-plane processes (a
// deployment ever running more than one API replica writing events, which
// this codebase does not do today — see the package doc's "Single
// namespace" section) are ordered to millisecond timestamp resolution, not
// by any causal relationship between the events they name. This endpoint's
// honest guarantee is "the order this process committed these events in",
// not "the order they were caused in" — the same distinction the per-run
// stream's own `sequence` column draws by being scoped to one run's
// advisory-locked writes rather than claiming a database-wide meaning.
func (s *Server) handleStreamEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeAPIError(w, r, internalError(errors.New("response writer does not support flushing")))
		return
	}

	scope, explicit := runsFilterParam(r)
	after, latest := resumeEventID(r)
	if latest {
		var err error
		after, err = s.latestEventID(ctx)
		if err != nil {
			s.writeAPIError(w, r, internalError(err))
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx's response buffering, if fronted by one
	w.WriteHeader(http.StatusOK)
	if latest && !writeSnapshotSSEEvent(w, after) {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(s.pollInterval * 2)
	defer ticker.Stop()
	keepalive := time.NewTicker(s.keepaliveInterval)
	defer keepalive.Stop()

	for {
		rows, err := s.pollCrossRunEvents(ctx, after)
		if err != nil {
			// The stream is already open; there is no JSON error channel
			// left to report through. Stop quietly — the client's
			// Last-Event-ID lets it resume from `after` once whatever
			// transient failure this was clears.
			return
		}

		var active map[string]bool
		if !explicit && len(rows) > 0 {
			active, err = s.activeRunIDs(ctx)
			if err != nil {
				return
			}
		}

		wrote := false
		for _, row := range rows {
			after = row.ID // advance past every raw row scanned, matched or not -- the bounded catch-up guarantee
			if !crossRunEventInScope(row, scope, explicit, active) {
				continue
			}
			if !writeCrossRunSSEEvent(w, row) {
				return
			}
			wrote = true
		}
		flusher.Flush()
		if wrote {
			keepalive.Reset(s.keepaliveInterval) // "idle" is measured from the last real frame
		}

		if !waitForNextPoll(ctx, ticker.C, keepalive.C, w, flusher) {
			return
		}
	}
}

// writeSnapshotSSEEvent announces the committed high-water mark captured by
// from=latest. The next real event is necessarily polled strictly after id.
func writeSnapshotSSEEvent(w http.ResponseWriter, snapshotID string) bool {
	body, err := json.Marshal(struct {
		SnapshotID string `json:"snapshot_id"`
	}{SnapshotID: snapshotID})
	if err != nil {
		return false
	}
	_, writeErr := fmt.Fprintf(w, "id: %s\nevent: stream.snapshot\ndata: %s\n\n", snapshotID, body)
	return writeErr == nil
}

// writeCrossRunSSEEvent renders one committed event as
// "id: <ulid>\nevent: <type>\ndata: <envelope JSON>\n\n" and reports
// whether the write succeeded — false means the client is gone and the
// stream should stop. Unlike writeSSEEvent's per-run frame, the id here is
// the events table's own primary key (a ULID string), not a per-run
// sequence: no single sequence numbers a cross-run stream, and the row's
// own generation-ordered id is the resume cursor this endpoint documents
// (see handleStreamEvents' ordering note).
func writeCrossRunSSEEvent(w http.ResponseWriter, row crossRunEventRow) bool {
	env := events.Envelope{
		ID:              row.ID,
		Source:          row.Source,
		SpecVersion:     "1.0",
		Type:            row.Type,
		Subject:         row.RunID,
		Time:            row.OccurredAt,
		DataContentType: "application/json",
		Data:            row.Data,
	}
	body, err := json.Marshal(env)
	if err != nil {
		return true // skip an unmarshalable row rather than killing the whole stream over it
	}
	_, writeErr := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", row.ID, row.Type, body)
	return writeErr == nil
}
