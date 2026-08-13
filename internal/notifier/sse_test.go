package notifier

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenStreamSetsFromQueryAndLastEventIDHeader(t *testing.T) {
	var gotQuery, gotHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("from")
		gotHeader = r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	resp, err := openStream(context.Background(), server.Client(), server.URL, nil, "01CURSOR")
	if err != nil {
		t.Fatalf("openStream: %v", err)
	}
	defer resp.Body.Close()

	if gotQuery != "01CURSOR" {
		t.Errorf("from query = %q, want 01CURSOR", gotQuery)
	}
	if gotHeader != "01CURSOR" {
		t.Errorf("Last-Event-ID header = %q, want 01CURSOR", gotHeader)
	}
}

func TestOpenStreamScopesToRunsQueryParameter(t *testing.T) {
	var gotRuns string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRuns = r.URL.Query().Get("runs")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	resp, err := openStream(context.Background(), server.Client(), server.URL, []string{"run-1", "run-2"}, "")
	if err != nil {
		t.Fatalf("openStream: %v", err)
	}
	defer resp.Body.Close()

	if gotRuns != "run-1,run-2" {
		t.Errorf("runs query = %q, want run-1,run-2", gotRuns)
	}
}

func TestOpenStreamNonOKStatusIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer server.Close()

	if _, err := openStream(context.Background(), server.Client(), server.URL, nil, ""); err == nil {
		t.Fatal("openStream accepted a 500 response")
	}
}

func TestReadFramesParsesIDEventData(t *testing.T) {
	body := "id: 01AAAA\nevent: dev.culture.nodes.run.completed\ndata: {\"subject\":\"run-1\"}\n\n" +
		"id: 01BBBB\nevent: dev.culture.nodes.attempt.completed\ndata: {\"subject\":\"run-1\"}\n\n"

	var got []Frame
	err := readFrames(context.Background(), strings.NewReader(body), func(f Frame) error {
		got = append(got, f)
		return nil
	})
	if err != nil {
		t.Fatalf("readFrames: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d frames, want 2", len(got))
	}
	if got[0].ID != "01AAAA" || got[0].Type != "dev.culture.nodes.run.completed" {
		t.Errorf("frame 0 = %+v", got[0])
	}
	if string(got[0].Data) != `{"subject":"run-1"}` {
		t.Errorf("frame 0 data = %s", got[0].Data)
	}
	if got[1].ID != "01BBBB" {
		t.Errorf("frame 1 id = %q, want 01BBBB", got[1].ID)
	}
}

func TestReadFramesToleratesAKeepAliveBlankLine(t *testing.T) {
	body := "\n\nid: 01AAAA\nevent: dev.culture.nodes.run.completed\ndata: {}\n\n\n"

	var got []Frame
	err := readFrames(context.Background(), strings.NewReader(body), func(f Frame) error {
		got = append(got, f)
		return nil
	})
	if err != nil {
		t.Fatalf("readFrames: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1 (leading/trailing blank lines are not frames)", len(got))
	}
}

func TestReadFramesStopsOnHandlerError(t *testing.T) {
	body := "id: 01AAAA\nevent: e\ndata: {}\n\nid: 01BBBB\nevent: e\ndata: {}\n\n"
	boom := fmt.Errorf("boom")

	calls := 0
	err := readFrames(context.Background(), strings.NewReader(body), func(f Frame) error {
		calls++
		return boom
	})
	if err != boom {
		t.Fatalf("readFrames err = %v, want the handler's error", err)
	}
	if calls != 1 {
		t.Fatalf("handler called %d times, want exactly 1 (it must stop at the first error)", calls)
	}
}

func TestReadFramesReturnsNilOnCleanEOF(t *testing.T) {
	err := readFrames(context.Background(), strings.NewReader(""), func(f Frame) error {
		t.Fatal("handler called on an empty stream")
		return nil
	})
	if err != nil {
		t.Fatalf("readFrames on empty stream = %v, want nil", err)
	}
}
