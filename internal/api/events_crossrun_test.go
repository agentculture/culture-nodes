package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/events"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// This file covers task t17's GET /v1alpha1/events, the cross-run
// companion to GET /v1alpha1/runs/{id}/events (internal/api/events.go's
// handleStreamEvents). Its four acceptance-criterion tests below drive real
// runs through newFixture's httptest server, and insert events directly via
// (*postgres.Store).InsertEvent -- the same escape hatch runs_timewindow_
// test.go uses for hand-picked timestamps -- so each test can assert on
// events it planted without also having to predict every event
// s.Engine.CreateRun itself emits along the way (run.created, node-run.
// ready for the start node, ...). Every planted event carries a distinct
// "marker" field in its data payload precisely so assertions can pick their
// own events out of that incidental engine noise by content, not by
// position or count.

// crossRunSSEEvent is one frame read off GET /v1alpha1/events: the parsed
// `id:`/`event:`/`data:` triple, with data further decoded as an
// events.Envelope so a test can read Subject (the run id) and the
// marker inside Data without hand-parsing JSON twice.
type crossRunSSEEvent struct {
	FrameID string
	Type    string
	Subject string
	Marker  string
}

// markerPayload is the data shape every event this file plants carries.
type markerPayload struct {
	Marker string `json:"marker"`
}

// insertMarkerEvent appends a committed, non-lifecycle run event
// (events.TypeAttemptCompleted -- a real event type, but not one of
// runLifecycleEventTypes) carrying {"marker": marker} as its data, so a
// test can find it in a stream by content. It is the tool
// TestStreamEventsInterleavesTwoActiveRuns and its siblings use to prove
// active-run scoping: this type is only ever admitted to the default
// cross-run scope because its run is active, never because of its type.
func insertMarkerEvent(t *testing.T, f *fixture, runID, marker string) storepg.Event {
	t.Helper()
	data, err := json.Marshal(markerPayload{Marker: marker})
	if err != nil {
		t.Fatalf("marshal marker payload: %v", err)
	}
	ev, err := f.store.InsertEvent(context.Background(), storepg.InsertEventInput{
		NamespaceID:   f.nsID,
		AggregateType: "run",
		AggregateID:   runID,
		EventType:     events.TypeAttemptCompleted,
		Data:          data,
	})
	if err != nil {
		t.Fatalf("insert marker event %s for run %s: %v", marker, runID, err)
	}
	return ev
}

// insertLifecycleEvent appends a committed run-lifecycle event (eventType
// must be one of runLifecycleEventTypes' members for a test to observe it
// pass the default scope regardless of run status) with an empty data
// payload -- exactly the shape internal/engine's real completion path
// writes for run.completed/failed/cancelled/bounded.
func insertLifecycleEvent(t *testing.T, f *fixture, runID, eventType string) storepg.Event {
	t.Helper()
	ev, err := f.store.InsertEvent(context.Background(), storepg.InsertEventInput{
		NamespaceID:   f.nsID,
		AggregateType: "run",
		AggregateID:   runID,
		EventType:     eventType,
	})
	if err != nil {
		t.Fatalf("insert lifecycle event %s for run %s: %v", eventType, runID, err)
	}
	return ev
}

// setRunStatus flips a run's status directly, the same raw-SQL escape
// hatch runs_timewindow_test.go uses for created_at/updated_at -- there is
// no API surface for forcing a run terminal without actually running a
// workflow to completion, and these tests need that transition under
// precise control.
func setRunStatus(t *testing.T, f *fixture, runID, status string) {
	t.Helper()
	if _, err := f.store.Pool().Exec(context.Background(),
		`UPDATE runs SET status = $2 WHERE id = $1`, runID, status); err != nil {
		t.Fatalf("set run %s status = %s: %v", runID, status, err)
	}
}

// publishAndCreateRuns publishes the given fixture workflow once and
// creates n runs against it, returning their RunOut records in creation
// order. Every run starts 'running' (migrations/0002's runs.status
// default), so it is active-scope-eligible by construction unless a test
// calls setRunStatus afterward.
func publishAndCreateRuns(t *testing.T, f *fixture, n int) []apipkg.RunOut {
	t.Helper()
	source := readFixtureWorkflow(t, "edge-order-ordered.workflow.yaml")

	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	runs := make([]apipkg.RunOut, n)
	for i := range runs {
		resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
			createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &runs[i])
		requireStatus(t, resp, body, http.StatusCreated)
	}
	return runs
}

// streamCrossRunEvents opens GET /v1alpha1/events (query appended verbatim
// if non-empty, lastEventID sent as the Last-Event-ID header if non-empty)
// and reads frames until either stop returns true for the accumulated
// slice, or timeout elapses -- the cross-run stream, unlike the per-run
// one, never closes itself, so a caller-supplied stopping condition (not a
// terminal event) is what ends the read. It always returns whatever frames
// were read, even on timeout, so a test asserting "at most these markers
// appeared" can still inspect a partial read.
func streamCrossRunEvents(t *testing.T, f *fixture, query, lastEventID string, timeout time.Duration, stop func([]crossRunSSEEvent) bool) []crossRunSSEEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	url := f.url("/v1alpha1/events")
	if query != "" {
		url += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", url, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("GET %s: content type = %q, want text/event-stream", url, ct)
	}

	var (
		out     []crossRunSSEEvent
		current crossRunSSEEvent
		rawData string
		scanner = bufio.NewScanner(resp.Body)
	)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			current.FrameID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			current.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			rawData = strings.TrimPrefix(line, "data: ")
		case line == "":
			if current.FrameID == "" {
				continue // a blank line between frames, not a frame boundary
			}
			var env events.Envelope
			var marker markerPayload
			if err := json.Unmarshal([]byte(rawData), &env); err == nil {
				current.Subject = env.Subject
				_ = json.Unmarshal(env.Data, &marker)
				current.Marker = marker.Marker
			}
			out = append(out, current)
			current, rawData = crossRunSSEEvent{}, ""
			if stop(out) {
				return out
			}
		}
	}
	return out // timeout or connection close -- return whatever was read
}

// markers extracts the non-empty Marker field from every event in evs, in
// order -- the common assertion shape every test below needs: "which of my
// planted events showed up, and in what order".
func markers(evs []crossRunSSEEvent) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		if e.Marker != "" {
			out = append(out, e.Marker)
		}
	}
	return out
}

// containsMarker reports whether evs holds an event whose Marker equals m.
func containsMarker(evs []crossRunSSEEvent, m string) bool {
	for _, e := range evs {
		if e.Marker == m {
			return true
		}
	}
	return false
}

// allMarkersSeen returns a stop predicate for streamCrossRunEvents: true
// once every marker in wantMarkers has been observed -- so a test does not
// have to guess how many incidental engine-noise events (run.created,
// node-run.ready, ...) will arrive interleaved with the markers it
// actually cares about.
func allMarkersSeen(wantMarkers ...string) func([]crossRunSSEEvent) bool {
	return func(evs []crossRunSSEEvent) bool {
		seen := make(map[string]bool, len(wantMarkers))
		for _, e := range evs {
			seen[e.Marker] = true
		}
		for _, m := range wantMarkers {
			if !seen[m] {
				return false
			}
		}
		return true
	}
}

// TestStreamEventsInterleavesTwoActiveRuns is acceptance criterion 1's
// centerpiece: one GET /v1alpha1/events connection must carry committed
// events from two concurrently active runs, in the order this process
// committed them, without a mesh consumer ever opening a second
// connection.
func TestStreamEventsInterleavesTwoActiveRuns(t *testing.T) {
	f := newFixture(t)
	runs := publishAndCreateRuns(t, f, 2)
	runA, runB := runs[0], runs[1]

	insertMarkerEvent(t, f, runA.ID, "a1")
	insertMarkerEvent(t, f, runB.ID, "b1")
	insertMarkerEvent(t, f, runA.ID, "a2")

	evs := streamCrossRunEvents(t, f, "", "", 5*time.Second, allMarkersSeen("a1", "b1", "a2"))

	got := markers(evs)
	want := []string{"a1", "b1", "a2"}
	if len(got) < len(want) {
		t.Fatalf("markers observed = %v, want at least %v (in order)", got, want)
	}
	// Filter got down to just the three markers this test planted, in
	// arrival order, and check that relative order survived the fan-in --
	// interleaving must not scramble each run's own event order.
	var filtered []string
	wantSet := map[string]bool{"a1": true, "b1": true, "a2": true}
	for _, m := range got {
		if wantSet[m] {
			filtered = append(filtered, m)
		}
	}
	if len(filtered) != len(want) {
		t.Fatalf("filtered markers = %v, want exactly %v", filtered, want)
	}
	for i := range want {
		if filtered[i] != want[i] {
			t.Errorf("marker[%d] = %q, want %q (full order: %v)", i, filtered[i], want[i], filtered)
		}
	}

	// Both runs' subjects must appear on the SAME connection -- the actual
	// "no per-run fan-out" proof, not just "both markers eventually
	// arrived somehow".
	subjects := map[string]bool{}
	for _, e := range evs {
		subjects[e.Subject] = true
	}
	if !subjects[runA.ID] || !subjects[runB.ID] {
		t.Fatalf("one connection's subjects = %v, want both %s and %s", subjects, runA.ID, runB.ID)
	}
}

// TestStreamEventsExcludesTerminalRunEventsByDefault is acceptance
// criterion 1's second half: a terminal run's own event history must not
// flood a resuming or freshly-connecting cross-run consumer. runB is
// pushed to 'completed' with five ordinary events already sitting in its
// backlog before the stream ever connects; the default scope must skip
// all five and admit only its lifecycle (run.completed) event, while
// runA -- left active -- streams normally.
func TestStreamEventsExcludesTerminalRunEventsByDefault(t *testing.T) {
	f := newFixture(t)
	runs := publishAndCreateRuns(t, f, 2)
	runA, runB := runs[0], runs[1]

	insertMarkerEvent(t, f, runA.ID, "a1")

	backlog := make([]string, 5)
	for i := range backlog {
		backlog[i] = fmt.Sprintf("b-old-%d", i)
		insertMarkerEvent(t, f, runB.ID, backlog[i])
	}
	setRunStatus(t, f, runB.ID, "completed")
	insertLifecycleEvent(t, f, runB.ID, events.TypeRunCompleted)

	// a1 (runA's marker) was planted first, before runB's whole backlog and
	// its terminal transition -- so it is also the *earliest* event by id
	// order, and would arrive and satisfy a marker-only stop condition
	// before the connection ever reads as far as runB's later
	// run.completed frame. Wait for both, so the test genuinely observes
	// the lifecycle event rather than racing past it.
	stop := func(evs []crossRunSSEEvent) bool {
		if !allMarkersSeen("a1")(evs) {
			return false
		}
		for _, e := range evs {
			if e.Type == events.TypeRunCompleted && e.Subject == runB.ID {
				return true
			}
		}
		return false
	}
	evs := streamCrossRunEvents(t, f, "", "", 3*time.Second, stop)

	if !containsMarker(evs, "a1") {
		t.Fatalf("markers = %v, want a1 (runA is active, must stream by default)", markers(evs))
	}
	for _, old := range backlog {
		if containsMarker(evs, old) {
			t.Errorf("terminal run's backlog marker %q leaked into the default-scope stream: %v", old, markers(evs))
		}
	}

	var sawRunCompleted bool
	for _, e := range evs {
		if e.Type == events.TypeRunCompleted && e.Subject == runB.ID {
			sawRunCompleted = true
		}
	}
	if !sawRunCompleted {
		t.Errorf("runB's run.completed lifecycle event never appeared; a mesh consumer would never see runB finish")
	}
}

// TestStreamEventsResumeAfterDisconnect is acceptance criterion 5's resume
// case: a client that disconnects after observing frame id X and
// reconnects with Last-Event-ID: X must see only events committed after X,
// never a replay of what it already has.
func TestStreamEventsResumeAfterDisconnect(t *testing.T) {
	f := newFixture(t)
	runs := publishAndCreateRuns(t, f, 1)
	run := runs[0]

	insertMarkerEvent(t, f, run.ID, "a1")

	first := streamCrossRunEvents(t, f, "", "", 3*time.Second, allMarkersSeen("a1"))
	if !containsMarker(first, "a1") {
		t.Fatalf("first connection markers = %v, want a1", markers(first))
	}
	if len(first) == 0 {
		t.Fatalf("first connection read no frames at all")
	}
	lastID := first[len(first)-1].FrameID
	if lastID == "" {
		t.Fatalf("last frame of the first connection carried no id")
	}

	insertMarkerEvent(t, f, run.ID, "a2")

	second := streamCrossRunEvents(t, f, "", lastID, 3*time.Second, allMarkersSeen("a2"))
	if !containsMarker(second, "a2") {
		t.Fatalf("resumed connection markers = %v, want a2", markers(second))
	}
	if containsMarker(second, "a1") {
		t.Errorf("resumed connection replayed a1, which the client already had before disconnecting: %v", markers(second))
	}
}

// TestStreamEventsRunsFilterOverridesDefaultScope is acceptance criterion
// 5's runs= case: an explicit runs= filter must report only the named run
// ids, active or not -- including excluding a THIRD run's lifecycle event,
// which the default scope would have admitted unconditionally. That is the
// actual proof the filter *overrides* the default rather than narrowing it
// or adding to it.
func TestStreamEventsRunsFilterOverridesDefaultScope(t *testing.T) {
	f := newFixture(t)
	runs := publishAndCreateRuns(t, f, 3)
	runA, runB, runC := runs[0], runs[1], runs[2]

	insertMarkerEvent(t, f, runA.ID, "a1")
	insertMarkerEvent(t, f, runB.ID, "b1")
	setRunStatus(t, f, runC.ID, "completed")
	insertLifecycleEvent(t, f, runC.ID, events.TypeRunCompleted)

	evs := streamCrossRunEvents(t, f, "runs="+runB.ID, "", 3*time.Second, allMarkersSeen("b1"))

	if !containsMarker(evs, "b1") {
		t.Fatalf("filtered markers = %v, want b1 (explicitly requested)", markers(evs))
	}
	if containsMarker(evs, "a1") {
		t.Errorf("runs=%s leaked runA's event a1: %v", runB.ID, markers(evs))
	}
	for _, e := range evs {
		if e.Subject == runC.ID {
			t.Errorf("runs=%s leaked runC's %s event even though runC was not named -- the explicit filter must override the default lifecycle overlay, not add to it",
				runB.ID, e.Type)
		}
	}
}
