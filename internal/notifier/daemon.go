// daemon.go builds a Daemon from Config and runs the reconnect/backoff
// loop that ties the SSE consumer (sse.go), the durable cursor
// (cursor.go), the run-detail fetch (rundetail.go), and the lifecycle
// filter (lifecycle.go) together into notify.Notify calls. See doc.go for
// the package-level design notes.
package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/notify"
)

// Defaults applied by Config.normalize when a field is left at its zero
// value.
const (
	defaultReconnectMin = 500 * time.Millisecond
	defaultReconnectMax = 30 * time.Second
	defaultHTTPTimeout  = 10 * time.Second
)

// Config is the daemon's resolved configuration -- what cmd/nodes-notifier
// builds from flags/environment (see main.go) before constructing a
// Daemon. It carries no webhook URL: internal/notify.ResolveWebhook reads
// that directly from the environment at delivery time (CULTURE_NODES_
// WEBHOOK_URL / DISCORD_WEBHOOK_URL), the one place it is ever read, per
// that package's own doc comment.
type Config struct {
	// APIBase is the control plane's base URL (e.g. "http://localhost:8080"),
	// used for both GET /v1alpha1/events and GET /v1alpha1/runs/{id}.
	APIBase string
	// CursorPath is the durable cursor file's path. Required: an
	// unconfigured cursor is the same class of mistake --state-dir being
	// unset is for nodes-runner (cmd/nodes-runner/main.go's
	// resolveDurability) -- a daemon that cannot remember its position
	// silently turns every restart into either a dropped or a duplicated
	// batch of notifications.
	CursorPath string
	// Runs, when non-empty, scopes the stream to exactly these run ids
	// (the server's "?runs=" override -- see internal/api/events.go's
	// handleStreamEvents doc comment). Empty means the server's own
	// default: every active run, plus lifecycle events for any run.
	Runs []string
	// DashboardBase is the base URL used to build Payload.DashboardLink
	// ("{DashboardBase}/runs/{id}"). Defaults to APIBase: the API server
	// also serves the web dashboard from the same origin (see
	// internal/api/server.go's static-file fallback), so most deployments
	// need no separate value.
	DashboardBase string
	// ReconnectMin and ReconnectMax bound the exponential backoff Run
	// applies between a dropped SSE connection and the next reconnect
	// attempt. Default 500ms / 30s.
	ReconnectMin time.Duration
	ReconnectMax time.Duration
	// HTTPTimeout bounds each GET /v1alpha1/runs/{id} detail fetch.
	// Default 10s. It does not apply to the SSE connection itself, which
	// is meant to stay open indefinitely.
	HTTPTimeout time.Duration
}

// normalize fills defaults and validates the required fields, returning
// the resolved Config a Daemon is built from.
func (c Config) normalize() (Config, error) {
	if strings.TrimSpace(c.APIBase) == "" {
		return Config{}, fmt.Errorf("notifier: APIBase is required")
	}
	if strings.TrimSpace(c.CursorPath) == "" {
		return Config{}, fmt.Errorf("notifier: CursorPath is required")
	}
	if c.DashboardBase == "" {
		c.DashboardBase = c.APIBase
	}
	if c.ReconnectMin <= 0 {
		c.ReconnectMin = defaultReconnectMin
	}
	if c.ReconnectMax <= 0 {
		c.ReconnectMax = defaultReconnectMax
	}
	if c.ReconnectMax < c.ReconnectMin {
		c.ReconnectMax = c.ReconnectMin
	}
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = defaultHTTPTimeout
	}
	return c, nil
}

// Daemon is one running notifier: an SSE consumer over Config.APIBase's
// cross-run event feed, a Cursor durably tracking position and delivery,
// and a lifecycle filter feeding internal/notify.Notify.
type Daemon struct {
	cfg        Config
	cursor     *Cursor
	stream     *http.Client
	detail     *http.Client
	journal    notify.JournalFunc
	diagnostic func(string)
}

// Option configures a Daemon at construction (NewDaemon).
type Option func(*Daemon)

// WithJournal sets the callback every notify.Notify call hands its
// delivery outcome to (see internal/notify's JournalEntry). Never
// required; a nil journal means Notify simply does not journal, matching
// internal/notify's own zero-value behavior.
func WithJournal(fn notify.JournalFunc) Option {
	return func(d *Daemon) { d.journal = fn }
}

// WithDiagnostic sets the callback for human-readable progress lines
// (connect/reconnect, malformed frames, run-detail fetch failures). Never
// required; a nil diagnostic means silence. cmd/nodes-notifier wires this
// to clifmt.EmitDiagnostic.
func WithDiagnostic(fn func(string)) Option {
	return func(d *Daemon) { d.diagnostic = fn }
}

// NewDaemon builds a Daemon from cfg and an already-loaded cursor (see
// LoadCursor) -- the cursor is a separate construction step from the
// Daemon itself so a caller can inspect/log the resumed position before
// Run starts consuming.
func NewDaemon(cfg Config, cursor *Cursor, opts ...Option) (*Daemon, error) {
	normalized, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	if cursor == nil {
		return nil, fmt.Errorf("notifier: cursor is required")
	}
	d := &Daemon{
		cfg:    normalized,
		cursor: cursor,
		// stream has no client-side timeout: it holds one long-lived SSE
		// connection whose lifetime is governed by Run's ctx, not by a
		// fixed deadline.
		stream: &http.Client{},
		detail: &http.Client{Timeout: normalized.HTTPTimeout},
	}
	for _, opt := range opts {
		opt(d)
	}
	return d, nil
}

// Run consumes the cross-run event stream until ctx is cancelled,
// reconnecting with exponential backoff (Config.ReconnectMin doubling up
// to ReconnectMax) across any disconnect -- a clean server-side close, a
// transport error, or a bad response. Every reconnect resumes from the
// Cursor's current position (Config never loses forward progress across a
// reconnect the same process survives, and LoadCursor/persist means it
// survives a process restart too).
//
// Run returns nil on a clean shutdown (ctx cancelled) and otherwise never
// returns on its own -- a daemon binary is expected to call Run once from
// main and let a signal-derived ctx end it, exactly like
// cmd/nodes-runner's listen().
func (d *Daemon) Run(ctx context.Context) error {
	backoff := d.cfg.ReconnectMin
	for {
		if ctx.Err() != nil {
			return nil
		}

		frameReceived, err := d.streamOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}

		if frameReceived {
			// Real progress was made on this connection, however it
			// ended -- the next attempt starts fresh rather than
			// inheriting a backoff grown by an earlier, unrelated run of
			// bad luck.
			backoff = d.cfg.ReconnectMin
		}
		if err != nil {
			d.diagnosef("nodes-notifier: stream error, reconnecting in %s: %v", backoff, err)
		} else {
			d.diagnosef("nodes-notifier: stream closed, reconnecting in %s", backoff)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > d.cfg.ReconnectMax {
			backoff = d.cfg.ReconnectMax
		}
	}
}

// streamOnce opens exactly one SSE connection from the cursor's current
// position and reads frames from it until it ends. frameReceived reports
// whether at least one frame was read before the stream ended, for Run's
// backoff-reset decision.
func (d *Daemon) streamOnce(ctx context.Context) (frameReceived bool, err error) {
	resp, err := openStream(ctx, d.stream, d.cfg.APIBase, d.cfg.Runs, d.cursor.Last())
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()

	err = readFrames(ctx, resp.Body, func(f Frame) error {
		frameReceived = true
		return d.handleFrame(ctx, f)
	})
	return frameReceived, err
}

// sseEnvelope is the local, minimal decode of the JSON body a frame's
// "data:" line carries (events.Envelope on the wire -- see
// internal/events/envelope.go). Only Subject is read: writeCrossRunSSEEvent
// (internal/api/events.go) always sets it to the run id, which is exactly
// the identifier fetchRunDetail needs and events carrying "IDs, not
// content" (internal/events/doc.go:16-25) promises is safe to read here.
// Defined locally rather than importing internal/events.Envelope for the
// same reason lifecycle.go hardcodes its type strings -- see that file's
// doc comment.
type sseEnvelope struct {
	Subject string `json:"subject"`
}

// handleFrame is the per-event decision point: skip a malformed or
// already-seen frame, silently advance past a non-lifecycle one, or -- for
// a new lifecycle event -- durably mark it delivered (BEFORE attempting
// delivery, see Cursor's doc comment) and hand a built Payload to
// notify.Notify.
//
// handleFrame never returns an error for anything this daemon can recover
// from on its own (a malformed frame, a run-detail fetch failure, a failed
// webhook POST) -- only a cursor persistence failure propagates, since an
// unwritable cursor file means this daemon can no longer make its
// exactly-once-across-restarts promise and streamOnce/Run should treat
// that as a reason to stop and reconnect (as the same class of error as a
// dropped connection) rather than silently drift out of sync with disk.
func (d *Daemon) handleFrame(ctx context.Context, f Frame) error {
	if f.ID == "" {
		d.diagnosef("nodes-notifier: dropping a frame with no event id (type=%q)", f.Type)
		return nil
	}
	if d.cursor.Seen(f.ID) {
		return nil
	}
	if !isLifecycleEvent(f.Type) {
		return d.cursor.Advance(f.ID)
	}

	var env sseEnvelope
	if err := json.Unmarshal(f.Data, &env); err != nil || env.Subject == "" {
		d.diagnosef("nodes-notifier: dropping malformed lifecycle event %s (type=%q): %v", f.ID, f.Type, err)
		return d.cursor.Advance(f.ID)
	}
	runID := env.Subject

	// Durably mark this event delivered BEFORE attempting the webhook
	// POST below: see Cursor's doc comment for why this ordering is what
	// keeps a crash from ever producing a duplicate Discord message.
	if err := d.cursor.MarkDelivered(f.ID); err != nil {
		return fmt.Errorf("notifier: persist cursor before delivering %s: %w", f.ID, err)
	}

	detail, err := fetchRunDetail(ctx, d.detail, d.cfg.APIBase, runID)
	if err != nil {
		// Fail-open, matching the webhook transport's own posture: a
		// run-detail fetch failure must not drop the notification
		// outright or block the loop. Post what is already known (the
		// run id from the event's own Subject, and the event type) rather
		// than nothing.
		d.diagnosef("nodes-notifier: run-detail fetch failed for event %s (run %s): %v", f.ID, runID, err)
		detail = runDetail{RunID: runID}
	}

	payload := notify.Payload{
		RunID:         detail.RunID,
		Workflow:      detail.WorkflowDigest,
		Event:         f.Type,
		Actor:         detail.Actor,
		DashboardLink: strings.TrimRight(d.cfg.DashboardBase, "/") + "/runs/" + runID,
	}
	notify.Notify(ctx, payload, d.journal)
	return nil
}

func (d *Daemon) diagnosef(format string, args ...any) {
	if d.diagnostic == nil {
		return
	}
	d.diagnostic(fmt.Sprintf(format, args...))
}
