package notifier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/notify"
)

// A realistic digest: the workflow label must show its first seven hex
// characters (after the "sha256:" algorithm prefix), never all 64 -- see
// issue #66, where a live Discord message rendered the full digest where a
// human needed a name.
const testDigest = "sha256:8d4c768f0bde3b02eea9d404046ff646b607a875d9063d13630787267f7d01ab"

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

// workflowFake serves both endpoints the workflow-name path needs: the
// run detail (GET /v1alpha1/runs/{id}) and the workflows read API (GET
// /v1alpha1/workflows/{digest}). workflowHits counts only the latter, so a
// test can prove the digest->key lookup was cached rather than repeated.
type workflowFake struct {
	server       *httptest.Server
	workflowHits atomic.Int32
	// workflowBody, when non-empty, replaces the default workflow-version
	// JSON body; workflowStatus, when non-zero, replaces the 200.
	workflowBody   string
	workflowStatus int
	// actorID, when non-empty, is the actor of the single attempt each run
	// detail reports; empty means a run with no agent actor at all.
	actorID string
}

func newWorkflowFake(t *testing.T) *workflowFake {
	t.Helper()
	f := &workflowFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1alpha1/runs/", func(w http.ResponseWriter, r *http.Request) {
		runID := strings.TrimPrefix(r.URL.Path, "/v1alpha1/runs/")
		attempts := "[]"
		if f.actorID != "" {
			attempts = `[{"actor_id": "` + f.actorID + `", "started_at": "2026-08-13T10:00:00Z"}]`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"run": {"id": "` + runID + `", "workflow_digest": "` + testDigest + `"},
			"node_runs": [{"updated_at": "2026-08-13T10:00:00Z", "attempts": ` + attempts + `}]
		}`))
	})
	mux.HandleFunc("/v1alpha1/workflows/", func(w http.ResponseWriter, r *http.Request) {
		f.workflowHits.Add(1)
		if got, want := r.URL.Path, "/v1alpha1/workflows/"+testDigest; got != want {
			t.Errorf("workflow lookup path = %q, want %q", got, want)
		}
		if f.workflowStatus != 0 {
			w.WriteHeader(f.workflowStatus)
			return
		}
		body := f.workflowBody
		if body == "" {
			body = `{"id": "wfv_1", "workflow_key": "parallel-live-proof", "version": 3, "digest": "` + testDigest + `"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// TestWorkflowLabelPrefersTheNameAndKeepsTheDigestShortAndSecondary is the
// pure-rendering half of issue #66's first finding: a name when one is
// known, the digest kept but demoted to a short parenthetical, and an
// honest fall back to the full digest when no name resolved.
func TestWorkflowLabelPrefersTheNameAndKeepsTheDigestShortAndSecondary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		detail runDetail
		want   string
	}{
		{
			name:   "name and digest",
			detail: runDetail{WorkflowDigest: testDigest, WorkflowKey: "parallel-live-proof"},
			want:   "parallel-live-proof (8d4c768)",
		},
		{
			name:   "no key resolved falls back to the full digest",
			detail: runDetail{WorkflowDigest: testDigest},
			want:   testDigest,
		},
		{
			name:   "no digest at all is just the name",
			detail: runDetail{WorkflowKey: "parallel-live-proof"},
			want:   "parallel-live-proof",
		},
		{
			name:   "neither is empty, never a stray parenthesis",
			detail: runDetail{},
			want:   "",
		},
		{
			name:   "a digest with no algorithm prefix still shortens",
			detail: runDetail{WorkflowDigest: "8d4c768f0bde3b02", WorkflowKey: "k"},
			want:   "k (8d4c768)",
		},
		{
			name:   "a digest already shorter than the cut is used whole",
			detail: runDetail{WorkflowDigest: "sha256:abc", WorkflowKey: "k"},
			want:   "k (abc)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.detail.workflowLabel(); got != tc.want {
				t.Errorf("workflowLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWorkflowKeyLookupIsCachedAcrossRuns is the caching half: the
// digest->key mapping is immutable (a digest content-addresses one
// normalized workflow version forever), so two runs pinning the same
// digest must cost exactly one GET /v1alpha1/workflows/{digest}.
func TestWorkflowKeyLookupIsCachedAcrossRuns(t *testing.T) {
	f := newWorkflowFake(t)
	cache := newWorkflowKeyCache()

	for _, runID := range []string{"run-1", "run-2", "run-3"} {
		detail, err := fetchRunDetail(context.Background(), f.server.Client(), f.server.URL, runID)
		if err != nil {
			t.Fatalf("fetchRunDetail(%s): %v", runID, err)
		}
		key, err := cache.lookup(context.Background(), f.server.Client(), f.server.URL, detail.WorkflowDigest)
		if err != nil {
			t.Fatalf("lookup(%s): %v", runID, err)
		}
		detail.WorkflowKey = key

		if got, want := detail.workflowLabel(), "parallel-live-proof (8d4c768)"; got != want {
			t.Errorf("workflowLabel() for %s = %q, want %q", runID, got, want)
		}
	}

	if got := f.workflowHits.Load(); got != 1 {
		t.Errorf("workflows read API hit %d times, want exactly 1 (the digest->key mapping is immutable and must be cached)", got)
	}
}

// TestWorkflowKeyLookupNeverReadsWorkflowContent keeps the c40 boundary on
// the new lookup: GET /v1alpha1/workflows/{digest} answers with the whole
// workflow version -- source, normalized IR, node definitions -- and none
// of that may reach a notification. As with fetchRunDetail, the guarantee
// is structural: the decode target has no field to hold it.
func TestWorkflowKeyLookupNeverReadsWorkflowContent(t *testing.T) {
	f := newWorkflowFake(t)
	f.workflowBody = `{
		"id": "wfv_1",
		"workflow_key": "parallel-live-proof",
		"version": 3,
		"digest": "` + testDigest + `",
		"source_format": "yaml",
		"source": "nodes:\n  build:\n    prompt: do-not-leak\n",
		"normalized_ir": {"nodes": {"build": {"prompt": "do-not-leak"}}}
	}`
	cache := newWorkflowKeyCache()

	key, err := cache.lookup(context.Background(), f.server.Client(), f.server.URL, testDigest)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if key != "parallel-live-proof" {
		t.Fatalf("key = %q, want parallel-live-proof", key)
	}

	detail := runDetail{RunID: "run-1", WorkflowDigest: testDigest, WorkflowKey: key}
	raw, err := notify.BuildMessage(testDiscordWebhookURL, detail.payload("dev.culture.nodes.run.created", "http://dashboard.example"))
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}
	if strings.Contains(string(raw), "do-not-leak") {
		t.Fatalf("workflow source/IR leaked into a notification: %s", raw)
	}
}

// TestWorkflowKeyLookupFailureFallsBackToTheDigest: a name is legibility,
// not correctness -- an unreachable or 404-ing workflows read API must
// leave the notification showing the digest, never block it.
func TestWorkflowKeyLookupFailureFallsBackToTheDigest(t *testing.T) {
	f := newWorkflowFake(t)
	f.workflowStatus = http.StatusNotFound
	cache := newWorkflowKeyCache()

	if _, err := cache.lookup(context.Background(), f.server.Client(), f.server.URL, testDigest); err == nil {
		t.Fatal("lookup accepted a 404")
	}
	// Nothing was cached, so a later attempt (e.g. after the control plane
	// finishes starting up) tries again rather than serving a bad answer
	// forever.
	if _, err := cache.lookup(context.Background(), f.server.Client(), f.server.URL, testDigest); err == nil {
		t.Fatal("second lookup accepted a 404")
	}
	if got := f.workflowHits.Load(); got != 2 {
		t.Errorf("workflows read API hit %d times, want 2 (a failed lookup must not be cached)", got)
	}

	detail := runDetail{RunID: "run-1", WorkflowDigest: testDigest}
	if got := detail.workflowLabel(); got != testDigest {
		t.Errorf("workflowLabel() = %q, want the full digest %q", got, testDigest)
	}
}

const (
	testDiscordWebhookURL = "https://discord.com/api/webhooks/123456/token-abc"
	testGenericWebhookURL = "https://hooks.example.com/incoming/xyz"
)

// TestRunWithNoActorProducesAMessageWithNoActorFieldAtAll is issue #66's
// second finding, end to end from a run detail: a code-node or wait-node
// run has no agent actor, and the honest rendering omits the field rather
// than showing a blank one -- no dangling "(actor: )" in the content line,
// no empty embed field, no "actor" key in the generic body. Absent and
// empty are different facts.
func TestRunWithNoActorProducesAMessageWithNoActorFieldAtAll(t *testing.T) {
	f := newWorkflowFake(t) // actorID left empty: no attempt has dispatched
	detail, err := fetchRunDetail(context.Background(), f.server.Client(), f.server.URL, "run-1")
	if err != nil {
		t.Fatalf("fetchRunDetail: %v", err)
	}
	if detail.Actor != "" {
		t.Fatalf("Actor = %q, want empty (no attempt carried an actor)", detail.Actor)
	}
	key, err := newWorkflowKeyCache().lookup(context.Background(), f.server.Client(), f.server.URL, detail.WorkflowDigest)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	detail.WorkflowKey = key
	payload := detail.payload("dev.culture.nodes.run.created", "http://dashboard.example")

	discordRaw, err := notify.BuildMessage(testDiscordWebhookURL, payload)
	if err != nil {
		t.Fatalf("BuildMessage(discord): %v", err)
	}
	if strings.Contains(strings.ToLower(string(discordRaw)), "actor") {
		t.Errorf("an actor-less run must produce NO actor field at all, got: %s", discordRaw)
	}

	var msg struct {
		Content string `json:"content"`
		Embeds  []struct {
			Description string `json:"description"`
			Fields      []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"fields"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(discordRaw, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msg.Embeds) != 1 {
		t.Fatalf("want exactly one embed, got %d", len(msg.Embeds))
	}
	if got, want := len(msg.Embeds[0].Fields), 4; got != want {
		t.Errorf("embed has %d fields, want %d (Run, Workflow, Event, Dashboard -- no blank Actor): %+v", got, want, msg.Embeds[0].Fields)
	}
	if got, want := msg.Embeds[0].Description, "Run run-1 reached dev.culture.nodes.run.created"; got != want {
		t.Errorf("description = %q, want %q", got, want)
	}
	if got, want := msg.Content, "parallel-live-proof (8d4c768): dev.culture.nodes.run.created"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}

	genericRaw, err := notify.BuildMessage(testGenericWebhookURL, payload)
	if err != nil {
		t.Fatalf("BuildMessage(generic): %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(genericRaw, &generic); err != nil {
		t.Fatalf("unmarshal generic: %v", err)
	}
	if _, present := generic["actor"]; present {
		t.Errorf(`generic body carries an "actor" key for an actor-less run: %s`, genericRaw)
	}
	if got, want := generic["workflow"], "parallel-live-proof (8d4c768)"; got != want {
		t.Errorf("workflow = %v, want %v", got, want)
	}
}

// TestRunWithAnActorStillCarriesIt is the other side of the same coin:
// omission applies only to the legitimately-empty case.
func TestRunWithAnActorStillCarriesIt(t *testing.T) {
	f := newWorkflowFake(t)
	f.actorID = "codex-thor"
	detail, err := fetchRunDetail(context.Background(), f.server.Client(), f.server.URL, "run-1")
	if err != nil {
		t.Fatalf("fetchRunDetail: %v", err)
	}
	payload := detail.payload("dev.culture.nodes.run.completed", "http://dashboard.example/")
	if payload.Actor != "codex-thor" {
		t.Fatalf("Actor = %q, want codex-thor", payload.Actor)
	}
	if got, want := payload.DashboardLink, "http://dashboard.example/runs/run-1"; got != want {
		t.Errorf("DashboardLink = %q, want %q", got, want)
	}
	raw, err := notify.BuildMessage(testDiscordWebhookURL, payload)
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}
	if !strings.Contains(string(raw), "codex-thor") || !strings.Contains(string(raw), "(actor: codex-thor)") {
		t.Errorf("a run WITH an actor must still render it: %s", raw)
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
