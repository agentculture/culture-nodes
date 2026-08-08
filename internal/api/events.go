package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/events"
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
		writeAPIError(w, classify(err))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, internalError(errors.New("response writer does not support flushing")))
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

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
