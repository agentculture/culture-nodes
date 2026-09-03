package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
)

type tailStreamFrame struct {
	ID   string
	Type string
	Data json.RawMessage
}

func openTailStream(t *testing.T, f *fixture) (<-chan tailStreamFrame, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url("/v1alpha1/events?from=latest"), nil)
	if err != nil {
		cancel()
		t.Fatalf("new tail stream request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := f.client.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open tail stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		t.Fatalf("open tail stream status = %d, want 200", resp.StatusCode)
	}

	frames := make(chan tailStreamFrame, 64)
	go func() {
		defer close(frames)
		defer resp.Body.Close()
		var frame tailStreamFrame
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				frame.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				frame.Type = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				frame.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
			case line == "" && frame.Type != "":
				frames <- frame
				frame = tailStreamFrame{}
			}
		}
	}()
	return frames, cancel
}

func nextTailFrame(t *testing.T, frames <-chan tailStreamFrame) tailStreamFrame {
	t.Helper()
	select {
	case frame, ok := <-frames:
		if !ok {
			t.Fatal("tail stream closed before the next frame")
		}
		return frame
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for tail stream frame")
		return tailStreamFrame{}
	}
}

func TestStreamEventsFromLatestStartsWithSnapshotAndLeavesDefaultUnchanged(t *testing.T) {
	f := newFixture(t)
	run := publishAndCreateRuns(t, f, 1)[0]
	latest := insertMarkerEvent(t, f, run.ID, "before-tail")

	frames, cancel := openTailStream(t, f)
	defer cancel()
	first := nextTailFrame(t, frames)
	if first.Type != "stream.snapshot" {
		t.Fatalf("first frame type = %q, want stream.snapshot", first.Type)
	}
	if first.ID != latest.ID {
		t.Errorf("snapshot frame id = %q, want namespace max event id %q", first.ID, latest.ID)
	}
	var data struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(first.Data, &data); err != nil {
		t.Fatalf("decode snapshot data %s: %v", first.Data, err)
	}
	if data.SnapshotID != latest.ID {
		t.Errorf("snapshot_id = %q, want %q", data.SnapshotID, latest.ID)
	}

	after := insertMarkerEvent(t, f, run.ID, "after-tail")
	second := nextTailFrame(t, frames)
	if second.ID != after.ID {
		t.Errorf("first event after snapshot has id %q, want %q", second.ID, after.ID)
	}

	defaultFrames := streamCrossRunEvents(t, f, "", "", 3*time.Second, allMarkersSeen("before-tail"))
	if !containsMarker(defaultFrames, "before-tail") {
		t.Fatalf("cursor-less default omitted historical event; markers = %v", markers(defaultFrames))
	}
}

func TestStreamEventsFromLatestSkipsFiftyTerminalRunsLifecycleHistory(t *testing.T) {
	f := newFixture(t)
	runs := publishAndCreateRuns(t, f, 50)
	for _, run := range runs {
		setRunStatus(t, f, run.ID, "completed")
		insertLifecycleEvent(t, f, run.ID, engine.TypeRunCompleted)
	}

	frames, cancel := openTailStream(t, f)
	defer cancel()
	if first := nextTailFrame(t, frames); first.Type != "stream.snapshot" {
		t.Fatalf("first frame type = %q, want stream.snapshot", first.Type)
	}

	postSnapshot := insertLifecycleEvent(t, f, runs[0].ID, engine.TypeRunCompleted)
	second := nextTailFrame(t, frames)
	if second.ID != postSnapshot.ID {
		t.Fatalf("frame after snapshot = (%q, %q), want only newly committed lifecycle event %q", second.ID, second.Type, postSnapshot.ID)
	}
}

func TestStreamEventsFromLatestDeliversRunCreatedBetweenSnapshotAndRunsReadOnce(t *testing.T) {
	f := newFixture(t)
	publishAndCreateRuns(t, f, 1)

	frames, cancel := openTailStream(t, f)
	defer cancel()
	if first := nextTailFrame(t, frames); first.Type != "stream.snapshot" {
		t.Fatalf("first frame type = %q, want stream.snapshot", first.Type)
	}

	run := publishAndCreateRuns(t, f, 1)[0]
	var listed apipkg.RunListOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs"), nil, &listed)
	requireStatus(t, resp, body, http.StatusOK)
	found := false
	for _, item := range listed.Items {
		if item.ID == run.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("GET /runs after run creation did not include %s", run.ID)
	}

	count := 0
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case frame := <-frames:
			if frame.Type != engine.TypeRunCreated {
				continue
			}
			var envelope struct {
				Subject string `json:"subject"`
			}
			if err := json.Unmarshal(frame.Data, &envelope); err != nil {
				t.Fatalf("decode run.created envelope: %v", err)
			}
			if envelope.Subject == run.ID {
				count++
			}
		case <-deadline:
			if count != 1 {
				t.Fatalf("run.created for %s delivered %d times after snapshot, want exactly once", run.ID, count)
			}
			return
		}
	}
}
