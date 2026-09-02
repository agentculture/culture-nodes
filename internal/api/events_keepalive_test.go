package api_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// This file covers task t3 of the login-from-anywhere cycle: both SSE
// streams (GET /v1alpha1/events and GET /v1alpha1/runs/{id}/events) write
// an SSE comment line (one starting with ':') at a fixed keepalive interval
// while idle, so an idle proxied connection is never mistaken for a dead
// one by a proxy in between (Cloudflare closes idle proxied connections at
// ~100 s; the production default of 25 s leaves margin). A comment line is
// invisible to every SSE consumer by specification -- browsers' EventSource
// never dispatches it -- so no event name or payload changes here.
//
// The keepalive interval is injectable (WithSSEKeepaliveInterval) exactly
// so these tests can run it at 20 ms and observe "two keepalives in 60 s"
// as "two keepalives in three intervals", rather than sleeping 60 s.

// keepaliveInterval is the injected interval every test in this file runs
// with; keepaliveWindow is its "60 s equivalent" -- the span in which the
// acceptance criterion expects at least two keepalives to have landed.
const (
	keepaliveInterval = 20 * time.Millisecond
	keepaliveWindow   = 3 * keepaliveInterval
)

// newKeepaliveFixture mirrors newFixture but installs a short SSE keepalive
// interval alongside the short poll interval.
func newKeepaliveFixture(t *testing.T) *fixture {
	t.Helper()
	s := requireStore(t)

	nsID := pgtest.MustNamespace(t, s, "api").ID
	srv, err := apipkg.NewServer(s, nsID,
		apipkg.WithPollInterval(30*time.Millisecond),
		apipkg.WithSSEKeepaliveInterval(keepaliveInterval),
	)
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &fixture{t: t, server: ts, api: srv, store: s, nsID: nsID, client: ts.Client()}
}

// sseRecorder is a flushable http.ResponseWriter whose body can be read
// concurrently with the handler still writing to it -- what a test needs to
// count keepalives while the stream is open, then cancel the request and
// prove the count stops moving. httptest.ResponseRecorder flushes but is
// not safe to read mid-handler.
type sseRecorder struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	body    strings.Builder
	flushes int
}

func newSSERecorder() *sseRecorder {
	return &sseRecorder{header: make(http.Header)}
}

func (r *sseRecorder) Header() http.Header { return r.header }

func (r *sseRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

func (r *sseRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(p)
}

func (r *sseRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushes++
}

// keepalives counts the SSE comment lines (lines whose first byte is ':')
// written so far.
func (r *sseRecorder) keepalives() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	scanner := bufio.NewScanner(strings.NewReader(r.body.String()))
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), ":") {
			n++
		}
	}
	return n
}

// statusCode reports the status the handler wrote.
func (r *sseRecorder) statusCode() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// runKeepaliveStream drives one SSE handler through the mux with a
// cancellable request, waits keepaliveWindow, asserts at least two
// keepalives were written in that window, cancels the request, waits for
// the handler to return, then proves no keepalive arrives once the client
// is gone.
func runKeepaliveStream(t *testing.T, f *fixture, path string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := newSSERecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.api.Handler().ServeHTTP(rec, req)
	}()

	// Wait for the "60 s equivalent" and expect two keepalives in it.
	deadline := time.Now().Add(keepaliveWindow)
	for time.Now().Before(deadline) && rec.keepalives() < 2 {
		time.Sleep(keepaliveInterval / 4)
	}
	if got := rec.keepalives(); got < 2 {
		t.Fatalf("%s: keepalives written within %v = %d, want >= 2", path, keepaliveWindow, got)
	}
	if got := rec.statusCode(); got != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200", path, got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("%s: content type = %q, want text/event-stream", path, ct)
	}

	// Disconnect: the handler must return, and nothing may be written after.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: handler did not return within 2s of the client disconnecting", path)
	}
	after := rec.keepalives()
	time.Sleep(keepaliveWindow)
	if got := rec.keepalives(); got != after {
		t.Fatalf("%s: keepalives after disconnect = %d, want %d (no keepalive after the client is gone)", path, got, after)
	}
}

// TestStreamEventsWritesKeepaliveWhileIdle: GET /v1alpha1/events writes an
// SSE comment every keepalive interval while no event is flowing, and stops
// the moment the client disconnects.
func TestStreamEventsWritesKeepaliveWhileIdle(t *testing.T) {
	f := newKeepaliveFixture(t)
	runKeepaliveStream(t, f, "/v1alpha1/events")
}

// TestStreamRunEventsWritesKeepaliveWhileIdle: the same contract on GET
// /v1alpha1/runs/{id}/events, for a run that is still running and has
// nothing new to say.
func TestStreamRunEventsWritesKeepaliveWhileIdle(t *testing.T) {
	f := newKeepaliveFixture(t)
	runs := publishAndCreateRuns(t, f, 1)
	runKeepaliveStream(t, f, "/v1alpha1/runs/"+runs[0].ID+"/events")
}

// TestSSEKeepaliveIntervalDefaultsTo25s pins the production default: the
// number that has to sit under Cloudflare's ~100 s idle cutoff.
func TestSSEKeepaliveIntervalDefaultsTo25s(t *testing.T) {
	if got, want := apipkg.DefaultSSEKeepaliveInterval, 25*time.Second; got != want {
		t.Fatalf("DefaultSSEKeepaliveInterval = %v, want %v", got, want)
	}
}
