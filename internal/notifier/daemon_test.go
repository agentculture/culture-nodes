package notifier_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/notifier"
)

// This file drives cmd/nodes-notifier's testable core (internal/notifier)
// end to end against three httptest fakes: a fake cross-run SSE server
// (fakeControlPlane, standing in for GET /v1alpha1/events), the same
// fake's run-detail endpoint (GET /v1alpha1/runs/{id}), and a captured
// webhook server (webhookCapture, standing in for Discord). No real
// database, no real webhook, no real control plane anywhere in this file
// -- exactly the "ZERO control-plane changes" boundary task t14 exists to
// keep: the daemon under test only ever issues plain HTTP GETs and POSTs.

const (
	envPrimary = "CULTURE_NODES_WEBHOOK_URL"
)

// waitFor polls cond until it reports true or timeout elapses, failing the
// test on timeout. Every assertion in this file that depends on the
// daemon's background goroutine making progress goes through this rather
// than a fixed sleep, so the suite is fast on a quiet machine and does not
// flake under load.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// capturedPost is the generic (non-Discord-host) webhook body internal/
// notify's BuildMessage produces -- see payload.go's buildGenericMessage.
// The fake webhook server in this file is deliberately not a discord.com
// host, so every delivery arrives in this flat, easy-to-assert shape
// rather than a Discord embed.
type capturedPost struct {
	RunID         string `json:"run_id"`
	Workflow      string `json:"workflow"`
	Event         string `json:"event"`
	Actor         string `json:"actor"`
	DashboardLink string `json:"dashboard_link"`
}

// webhookCapture stands in for Discord: every POST it receives is decoded
// and recorded, in arrival order, under a mutex so concurrent test
// goroutines (the daemon's own background Run) can read a consistent
// snapshot.
type webhookCapture struct {
	mu     sync.Mutex
	posts  []capturedPost
	server *httptest.Server
}

func newWebhookCapture(t *testing.T) *webhookCapture {
	t.Helper()
	wc := &webhookCapture{}
	wc.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p capturedPost
		_ = json.NewDecoder(r.Body).Decode(&p)
		wc.mu.Lock()
		wc.posts = append(wc.posts, p)
		wc.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(wc.server.Close)
	return wc
}

func (wc *webhookCapture) snapshot() []capturedPost {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	out := make([]capturedPost, len(wc.posts))
	copy(out, wc.posts)
	return out
}

func (wc *webhookCapture) count() int {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	return len(wc.posts)
}

// storedEvent is one row of fakeControlPlane's in-memory event log.
type storedEvent struct {
	id, eventType, subject string
}

// fakeControlPlane serves both endpoints the daemon reads: GET
// /v1alpha1/events (a poll-backed cross-run SSE stream honoring "?from="
// exactly like internal/api/events.go's handleStreamEvents/
// pollCrossRunEvents -- strictly-greater-than, id order) and GET
// /v1alpha1/runs/{id} (a minimal RunView carrying only the fields
// fetchRunDetail reads). Events already in the log at connection time, and
// any added afterward via addEvent, are both visible to an open
// connection -- addEvent is safe to call while a daemon is streaming, the
// same way real committed events keep arriving during a real connection.
type fakeControlPlane struct {
	mu               sync.Mutex
	events           []storedEvent
	workflowDigest   map[string]string
	workflowKey      map[string]string // digest -> human-readable workflow key
	workflowHits     int               // GET /v1alpha1/workflows/{digest} count, for the cache assertion
	maxFramesPerConn int               // 0 = unbounded; >0 simulates a flaky connection that drops after N frames
	server           *httptest.Server
}

func newFakeControlPlane(t *testing.T) *fakeControlPlane {
	t.Helper()
	fcp := &fakeControlPlane{workflowDigest: map[string]string{}, workflowKey: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1alpha1/events", fcp.handleEvents)
	mux.HandleFunc("/v1alpha1/runs/", fcp.handleRunDetail)
	mux.HandleFunc("/v1alpha1/workflows/", fcp.handleWorkflowVersion)
	fcp.server = httptest.NewServer(mux)
	t.Cleanup(fcp.server.Close)
	return fcp
}

func (fcp *fakeControlPlane) addEvent(id, eventType, subject string) {
	fcp.mu.Lock()
	defer fcp.mu.Unlock()
	fcp.events = append(fcp.events, storedEvent{id: id, eventType: eventType, subject: subject})
}

func (fcp *fakeControlPlane) setWorkflow(runID, digest string) {
	fcp.mu.Lock()
	defer fcp.mu.Unlock()
	fcp.workflowDigest[runID] = digest
}

// setWorkflowKey makes a digest resolvable to a name through the fake's
// workflows read API. A digest with no entry here 404s, standing in for a
// control plane whose workflows surface is unreachable or does not know
// the digest -- the daemon must still deliver, showing the digest.
func (fcp *fakeControlPlane) setWorkflowKey(digest, key string) {
	fcp.mu.Lock()
	defer fcp.mu.Unlock()
	fcp.workflowKey[digest] = key
}

func (fcp *fakeControlPlane) workflowLookups() int {
	fcp.mu.Lock()
	defer fcp.mu.Unlock()
	return fcp.workflowHits
}

func (fcp *fakeControlPlane) handleWorkflowVersion(w http.ResponseWriter, r *http.Request) {
	digest := strings.TrimPrefix(r.URL.Path, "/v1alpha1/workflows/")
	fcp.mu.Lock()
	fcp.workflowHits++
	key := fcp.workflowKey[digest]
	fcp.mu.Unlock()
	if key == "" {
		http.Error(w, `{"error": "no workflow version with that digest"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id": "wfv_1", "workflow_key": %q, "version": 3, "digest": %q}`, key, digest)
}

func (fcp *fakeControlPlane) setMaxFramesPerConn(n int) {
	fcp.mu.Lock()
	defer fcp.mu.Unlock()
	fcp.maxFramesPerConn = n
}

func (fcp *fakeControlPlane) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimPrefix(r.URL.Path, "/v1alpha1/runs/")
	fcp.mu.Lock()
	digest := fcp.workflowDigest[runID]
	fcp.mu.Unlock()
	if digest == "" {
		digest = "sha256:unknown"
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"run": {"id": %q, "workflow_digest": %q}, "node_runs": []}`, runID, digest)
}

func (fcp *fakeControlPlane) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush support", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	after := r.URL.Query().Get("from")
	ctx := r.Context()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	sent := 0
	for {
		fcp.mu.Lock()
		var pending []storedEvent
		for _, e := range fcp.events {
			if after == "" || e.id > after {
				pending = append(pending, e)
			}
		}
		maxFrames := fcp.maxFramesPerConn
		fcp.mu.Unlock()

		for _, e := range pending {
			data, _ := json.Marshal(map[string]string{"subject": e.subject})
			if _, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", e.id, e.eventType, data); err != nil {
				return
			}
			after = e.id
			sent++
			if maxFrames > 0 && sent >= maxFrames {
				flusher.Flush()
				return // simulate a dropped connection after N frames
			}
		}
		if len(pending) > 0 {
			flusher.Flush()
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// newTestDaemon builds a Daemon wired at fcp/cursorPath/webhook URL, with
// fast reconnect bounds so a test never waits on production-sized backoff.
func newTestDaemon(t *testing.T, fcp *fakeControlPlane, cursorPath string) *notifier.Daemon {
	t.Helper()
	cursor, err := notifier.LoadCursor(cursorPath)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	d, err := notifier.NewDaemon(notifier.Config{
		APIBase:       fcp.server.URL,
		CursorPath:    cursorPath,
		DashboardBase: "http://dashboard.example",
		ReconnectMin:  5 * time.Millisecond,
		ReconnectMax:  20 * time.Millisecond,
		HTTPTimeout:   2 * time.Second,
	}, cursor)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	return d
}

func TestDaemonDeliversLifecycleEventsAndSkipsNoise(t *testing.T) {
	t.Setenv(envPrimary, "")
	fcp := newFakeControlPlane(t)
	wc := newWebhookCapture(t)
	t.Setenv(envPrimary, wc.server.URL)
	fcp.setWorkflow("run-1", "sha256:workflow-1")

	fcp.addEvent("00001", "dev.culture.nodes.run.created", "run-1")
	fcp.addEvent("00002", "dev.culture.nodes.attempt.completed", "run-1") // not a lifecycle event
	fcp.addEvent("00003", "dev.culture.nodes.run.completed", "run-1")

	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	d := newTestDaemon(t, fcp, cursorPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool { return wc.count() >= 2 })
	cancel()

	posts := wc.snapshot()
	if len(posts) != 2 {
		t.Fatalf("got %d webhook posts, want exactly 2 (the noise event must be skipped): %+v", len(posts), posts)
	}
	if posts[0].Event != "dev.culture.nodes.run.created" || posts[1].Event != "dev.culture.nodes.run.completed" {
		t.Fatalf("posts out of order or wrong events: %+v", posts)
	}
	for _, p := range posts {
		if p.RunID != "run-1" {
			t.Errorf("RunID = %q, want run-1", p.RunID)
		}
		if p.Workflow != "sha256:workflow-1" {
			t.Errorf("Workflow = %q, want sha256:workflow-1", p.Workflow)
		}
		if p.DashboardLink != "http://dashboard.example/runs/run-1" {
			t.Errorf("DashboardLink = %q, want http://dashboard.example/runs/run-1", p.DashboardLink)
		}
	}
}

// TestDaemonRendersTheWorkflowNameAndCachesTheLookup is issue #66's first
// finding proven through the daemon itself: when the workflows read API
// can name the digest, every notification carries "name (short-digest)",
// and the immutable digest->key mapping is fetched exactly once no matter
// how many events that run produces. TestDaemonDeliversLifecycleEvents
// AndSkipsNoise above covers the other side -- a digest the workflows
// surface cannot name still delivers, showing the full digest.
func TestDaemonRendersTheWorkflowNameAndCachesTheLookup(t *testing.T) {
	const digest = "sha256:8d4c768f0bde3b02eea9d404046ff646b607a875d9063d13630787267f7d01ab"

	t.Setenv(envPrimary, "")
	fcp := newFakeControlPlane(t)
	wc := newWebhookCapture(t)
	t.Setenv(envPrimary, wc.server.URL)
	fcp.setWorkflow("run-1", digest)
	fcp.setWorkflow("run-2", digest) // same workflow version, a second run
	fcp.setWorkflowKey(digest, "parallel-live-proof")

	fcp.addEvent("00001", "dev.culture.nodes.run.created", "run-1")
	fcp.addEvent("00002", "dev.culture.nodes.run.completed", "run-1")
	fcp.addEvent("00003", "dev.culture.nodes.run.created", "run-2")

	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	d := newTestDaemon(t, fcp, cursorPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool { return wc.count() >= 3 })
	cancel()

	for _, p := range wc.snapshot() {
		if got, want := p.Workflow, "parallel-live-proof (8d4c768)"; got != want {
			t.Errorf("Workflow = %q, want %q", got, want)
		}
	}
	if got := fcp.workflowLookups(); got != 1 {
		t.Errorf("workflows read API hit %d times for 3 notifications of 1 digest, want exactly 1 (cached)", got)
	}
}

// TestRestartDeliversEachLifecycleEventExactlyOnce is task t14's core
// acceptance criterion: "kill-and-restart mid-run posts exactly one
// Discord message per lifecycle event." It runs a daemon against a fake
// control plane holding three lifecycle events, kills it (context
// cancellation, exactly how a real process receives SIGTERM) once it has
// delivered the first two, then starts a brand-new Daemon instance from
// the SAME cursor file against the SAME fake control plane and lets it run
// to completion -- proving the restart resumes from the persisted cursor
// rather than either replaying already-delivered events or losing ones it
// has not reached yet.
func TestRestartDeliversEachLifecycleEventExactlyOnce(t *testing.T) {
	t.Setenv(envPrimary, "")
	fcp := newFakeControlPlane(t)
	wc := newWebhookCapture(t)
	t.Setenv(envPrimary, wc.server.URL)
	fcp.setWorkflow("run-1", "sha256:workflow-1")
	fcp.setWorkflow("run-2", "sha256:workflow-2")

	fcp.addEvent("00001", "dev.culture.nodes.run.created", "run-1")
	fcp.addEvent("00002", "dev.culture.nodes.run.completed", "run-1")
	// The third event is added only after the first instance is killed
	// (below) -- deliberately, so this test does not race the first
	// instance's own delivery speed against how quickly the test can call
	// cancel1(). What it proves either way is the same: no duplicate, no
	// drop, across a kill-and-restart.

	cursorPath := filepath.Join(t.TempDir(), "cursor.json")

	// First instance: run until it has delivered the first two events,
	// then kill it -- mid-stream, before the third event even exists.
	d1 := newTestDaemon(t, fcp, cursorPath)
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() { _ = d1.Run(ctx1); close(done1) }()

	waitFor(t, 3*time.Second, func() bool { return wc.count() >= 2 })
	cancel1()
	select {
	case <-done1:
	case <-time.After(3 * time.Second):
		t.Fatal("first daemon instance did not stop after its context was cancelled")
	}

	afterKill := wc.snapshot()
	if len(afterKill) != 2 {
		t.Fatalf("after kill: got %d posts, want exactly 2: %+v", len(afterKill), afterKill)
	}

	// Now let a third lifecycle event show up -- the run this restarted
	// daemon has to actually catch, not just avoid duplicating the first
	// two.
	fcp.addEvent("00003", "dev.culture.nodes.run.created", "run-2")

	// Restart: a brand-new Daemon, loading the same cursor file fresh from
	// disk (exactly what a restarted process does), against the same fake
	// control plane.
	d2 := newTestDaemon(t, fcp, cursorPath)
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { _ = d2.Run(ctx2); close(done2) }()
	defer func() { cancel2(); <-done2 }()

	waitFor(t, 3*time.Second, func() bool { return wc.count() >= 3 })

	final := wc.snapshot()
	if len(final) != 3 {
		t.Fatalf("got %d total posts across both instances, want exactly 3 (one per lifecycle event): %+v", len(final), final)
	}

	seen := map[string]int{}
	for _, p := range final {
		seen[p.RunID+"|"+p.Event]++
	}
	want := map[string]int{
		"run-1|dev.culture.nodes.run.created":   1,
		"run-1|dev.culture.nodes.run.completed": 1,
		"run-2|dev.culture.nodes.run.created":   1,
	}
	for key, wantCount := range want {
		if seen[key] != wantCount {
			t.Errorf("post count for %s = %d, want %d (no duplicate, no drop): %+v", key, seen[key], wantCount, final)
		}
	}
	for key, gotCount := range seen {
		if _, ok := want[key]; !ok {
			t.Errorf("unexpected extra post for %s (count %d)", key, gotCount)
		}
	}
}

// TestDaemonDedupsAcrossManyReconnectsWithinOneRun forces the fake control
// plane to drop the SSE connection after every single frame
// (maxFramesPerConn = 1), so delivering three events requires the SAME
// running Daemon to reconnect (not restart -- Run's own backoff/reconnect
// loop) twice. Each reconnect resumes with "?from=" set to the Cursor's
// current position; this test proves that resume never re-hands the
// daemon an event it already delivered.
func TestDaemonDedupsAcrossManyReconnectsWithinOneRun(t *testing.T) {
	t.Setenv(envPrimary, "")
	fcp := newFakeControlPlane(t)
	wc := newWebhookCapture(t)
	t.Setenv(envPrimary, wc.server.URL)
	fcp.setMaxFramesPerConn(1)
	fcp.setWorkflow("run-1", "sha256:workflow-1")

	fcp.addEvent("00001", "dev.culture.nodes.run.created", "run-1")
	fcp.addEvent("00002", "dev.culture.nodes.run.completed", "run-1")
	fcp.addEvent("00003", "dev.culture.nodes.run.failed", "run-1")

	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	d := newTestDaemon(t, fcp, cursorPath)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = d.Run(ctx) }()
	defer cancel()

	waitFor(t, 5*time.Second, func() bool { return wc.count() >= 3 })

	// Give any spurious extra reconnect a moment to (wrongly) redeliver
	// something, then assert the count never grew past 3.
	time.Sleep(100 * time.Millisecond)
	posts := wc.snapshot()
	if len(posts) != 3 {
		t.Fatalf("got %d posts across %d forced reconnects, want exactly 3 (no duplicates): %+v", len(posts), 3, posts)
	}
	seen := map[string]bool{}
	for _, p := range posts {
		key := p.RunID + "|" + p.Event
		if seen[key] {
			t.Errorf("event %s delivered more than once across reconnects", key)
		}
		seen[key] = true
	}
}

// TestDaemonRunDetailFailureIsFailOpen proves a GET /v1alpha1/runs/{id}
// failure does not block the loop or drop the notification outright: the
// daemon still posts, with whatever it already knew from the event itself
// (the run id) standing in for the fields it could not fetch.
func TestDaemonRunDetailFailureIsFailOpen(t *testing.T) {
	t.Setenv(envPrimary, "")
	fcp := newFakeControlPlane(t)
	wc := newWebhookCapture(t)
	t.Setenv(envPrimary, wc.server.URL)

	// A tiny mux that reuses fcp's own /v1alpha1/events handler (events
	// still come from fcp) but answers /v1alpha1/runs/* with a hard
	// failure instead of fcp's own (always-200) handleRunDetail, so
	// openStream and fetchRunDetail see different backends without
	// needing two separate Configs.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1alpha1/events", fcp.handleEvents)
	mux.HandleFunc("/v1alpha1/runs/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	combined := httptest.NewServer(mux)
	defer combined.Close()

	fcp.addEvent("00001", "dev.culture.nodes.run.completed", "run-1")

	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	cursor, err := notifier.LoadCursor(cursorPath)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	d, err := notifier.NewDaemon(notifier.Config{
		APIBase:       combined.URL,
		CursorPath:    cursorPath,
		DashboardBase: "http://dashboard.example",
		ReconnectMin:  5 * time.Millisecond,
		ReconnectMax:  20 * time.Millisecond,
		HTTPTimeout:   2 * time.Second,
	}, cursor)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool { return wc.count() >= 1 })
	posts := wc.snapshot()
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want 1 (fail-open on a run-detail fetch failure)", len(posts))
	}
	if posts[0].RunID != "run-1" {
		t.Errorf("RunID = %q, want run-1 (known from the event's own subject even without run detail)", posts[0].RunID)
	}
	if posts[0].Event != "dev.culture.nodes.run.completed" {
		t.Errorf("Event = %q, want dev.culture.nodes.run.completed", posts[0].Event)
	}
}

// TestDaemonToleratesMalformedFrames drives the daemon against a
// hand-written raw SSE body (not fakeControlPlane's own event log) so the
// test can inject frames the real server would never produce: one with an
// id but unparsable JSON data, and one with no id at all. Both must be
// skipped without killing the stream or crashing the daemon, and a valid
// frame after them must still be delivered.
func TestDaemonToleratesMalformedFrames(t *testing.T) {
	t.Setenv(envPrimary, "")
	wc := newWebhookCapture(t)
	t.Setenv(envPrimary, wc.server.URL)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1alpha1/runs/", func(w http.ResponseWriter, r *http.Request) {
		runID := strings.TrimPrefix(r.URL.Path, "/v1alpha1/runs/")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"run": {"id": %q, "workflow_digest": "sha256:wf"}, "node_runs": []}`, runID)
	})
	mux.HandleFunc("/v1alpha1/events", func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// A lifecycle-typed frame whose data is not valid JSON.
		_, _ = fmt.Fprint(w, "id: 00001\nevent: dev.culture.nodes.run.completed\ndata: {not json\n\n")
		// A frame with no id at all.
		_, _ = fmt.Fprint(w, "event: dev.culture.nodes.run.completed\ndata: {\"subject\":\"run-ghost\"}\n\n")
		// A well-formed frame that must still get through.
		_, _ = fmt.Fprint(w, "id: 00002\nevent: dev.culture.nodes.run.completed\ndata: {\"subject\":\"run-1\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	cursor, err := notifier.LoadCursor(cursorPath)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	d, err := notifier.NewDaemon(notifier.Config{
		APIBase:       server.URL,
		CursorPath:    cursorPath,
		DashboardBase: "http://dashboard.example",
		ReconnectMin:  5 * time.Millisecond,
		ReconnectMax:  20 * time.Millisecond,
		HTTPTimeout:   2 * time.Second,
	}, cursor)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool { return wc.count() >= 1 })
	time.Sleep(50 * time.Millisecond) // let a wrongly-accepted malformed frame show up, if any
	posts := wc.snapshot()
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want exactly 1 (both malformed frames must be dropped): %+v", len(posts), posts)
	}
	if posts[0].RunID != "run-1" {
		t.Errorf("RunID = %q, want run-1 (the well-formed frame)", posts[0].RunID)
	}
}
