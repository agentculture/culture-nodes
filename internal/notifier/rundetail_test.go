package notifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchRunDetailParsesTheNestedRunKeyAndPicksTheLatestActor(t *testing.T) {
	older := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	newer := time.Now().UTC().Format(time.RFC3339)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1alpha1/runs/run-1" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"run": {"id": "run-1", "workflow_digest": "sha256:abc"},
			"tokens": [],
			"node_runs": [
				{"updated_at": "` + older + `", "attempts": [
					{"actor_id": "actor-old", "started_at": "` + older + `", "completed_at": "` + older + `"}
				]},
				{"updated_at": "` + newer + `", "attempts": [
					{"actor_id": "actor-new", "started_at": "` + older + `", "completed_at": "` + newer + `"}
				]}
			]
		}`))
	}))
	defer server.Close()

	detail, err := fetchRunDetail(context.Background(), server.Client(), server.URL, "run-1")
	if err != nil {
		t.Fatalf("fetchRunDetail: %v", err)
	}
	if detail.RunID != "run-1" {
		t.Errorf("RunID = %q, want run-1", detail.RunID)
	}
	if detail.WorkflowDigest != "sha256:abc" {
		t.Errorf("WorkflowDigest = %q, want sha256:abc", detail.WorkflowDigest)
	}
	if detail.Actor != "actor-new" {
		t.Errorf("Actor = %q, want actor-new (the most recently completed attempt)", detail.Actor)
	}
}

func TestFetchRunDetailNeverReadsInputOrOutput(t *testing.T) {
	// A response carrying input/output must not surface either through
	// runDetail -- the struct simply has no field to decode them into,
	// which is the boundary c40 enforces structurally rather than by a
	// runtime filter.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"run": {
				"id": "run-1",
				"workflow_digest": "sha256:abc",
				"input": {"secret": "do-not-leak"},
				"output": {"also_secret": "do-not-leak"}
			},
			"node_runs": []
		}`))
	}))
	defer server.Close()

	detail, err := fetchRunDetail(context.Background(), server.Client(), server.URL, "run-1")
	if err != nil {
		t.Fatalf("fetchRunDetail: %v", err)
	}
	if detail.RunID != "run-1" || detail.WorkflowDigest != "sha256:abc" {
		t.Fatalf("unexpected detail: %+v", detail)
	}
	// There is no assertion possible against "detail.Input" because the
	// type has no such field -- that absence is the point of this test.
}

func TestFetchRunDetailNonOKStatusIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	if _, err := fetchRunDetail(context.Background(), server.Client(), server.URL, "missing"); err == nil {
		t.Fatal("fetchRunDetail accepted a 404")
	}
}

func TestDeriveActorEmptyWhenNoAttemptHasDispatched(t *testing.T) {
	if got := deriveActor(nil); got != "" {
		t.Errorf("deriveActor(nil) = %q, want empty", got)
	}
	if got := deriveActor([]nodeRunOutSlice{{Attempts: nil}}); got != "" {
		t.Errorf("deriveActor(no attempts) = %q, want empty", got)
	}
}
