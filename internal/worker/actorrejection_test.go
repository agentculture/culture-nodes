package worker_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
)

// The refused dispatch an operator can read (task t3, issue #125).
//
// Run 01M04Q26ZPNKTXVNBGSDS1YR9F's attempt recorded `actor_rejected_input
// (HTTP 400): actor answered Bad Request`. The bridge's own body said
// `input.instruction is required and must be a non-empty string`, and that
// sentence never reached the ledger — diagnosing the refusal meant
// reproducing the call by hand against the bridge. These tests drive the
// whole path a real refusal takes: a real worker, a real HTTP actor that
// refuses, a real attempt row, and the real GET /v1alpha1/runs/{id} handler
// an operator reads.

// attemptErrorOut is the diagnostic shape a failed attempt's result takes,
// decoded the way any client of the documented API would decode it.
type attemptErrorOut struct {
	Error struct {
		Class  string `json:"class"`
		Detail string `json:"detail"`
		Actor  *struct {
			Message   string `json:"message"`
			Class     string `json:"class"`
			Body      string `json:"body"`
			Truncated bool   `json:"truncated"`
		} `json:"actor"`
	} `json:"error"`
}

// refusedRunView drives one run against an actor that refuses every
// invocation with status/body, then reads the run back through the real
// HTTP API and returns the `analyze` node's only attempt.
func refusedRunView(t *testing.T, status int, contentType, body string) (attemptErrorOut, string) {
	t.Helper()
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	})

	apiSrv, err := api.NewServer(h.store, h.ns.ID)
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}
	ts := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(ts.Close)

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(30*time.Second, func() bool { return h.run(run.ID).State.Terminal() })
	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed (worker errors: %v)", state, h.workerErrors())
	}

	resp, err := ts.Client().Get(ts.URL + "/v1alpha1/runs/" + run.ID)
	if err != nil {
		t.Fatalf("GET run view: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1alpha1/runs/%s = %d, want 200", run.ID, resp.StatusCode)
	}
	var view api.RunViewOut
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decode run view: %v", err)
	}

	for _, nr := range view.NodeRuns {
		if nr.NodeID != "analyze" {
			continue
		}
		if len(nr.Attempts) != 1 {
			t.Fatalf("node run %s has %d attempts, want 1", nr.ID, len(nr.Attempts))
		}
		attempt := nr.Attempts[0]
		var decoded attemptErrorOut
		if err := json.Unmarshal(attempt.Result, &decoded); err != nil {
			t.Fatalf("attempt result is not the diagnostic shape: %v\nraw: %s", err, attempt.Result)
		}
		return decoded, attempt.Status
	}
	t.Fatalf("no `analyze` node run in the run view: %+v", view.NodeRuns)
	return attemptErrorOut{}, ""
}

// Acceptance criterion 1: the bridge's own error text and class are visible
// in GET /v1alpha1/runs/{id}. This is #125's exact body.
func TestRunViewShowsTheActorsRejectionTextAndClass(t *testing.T) {
	got, status := refusedRunView(t, http.StatusBadRequest, "application/json",
		`{"error":"input.instruction is required and must be a non-empty string","class":"actor_rejected_input"}`)

	if engine.TechStatus(status) != engine.StatusContractRejected {
		t.Errorf("attempt status = %q, want %q", status, engine.StatusContractRejected)
	}
	if got.Error.Class != string(actors.ClassActorRejectedInput) {
		t.Errorf("recorded class = %q, want the engine's own §13.5 classification", got.Error.Class)
	}
	if got.Error.Actor == nil {
		t.Fatal("the run view carries no actor block: the bridge's reason was dropped, which is issue #125")
	}
	if got.Error.Actor.Message != "input.instruction is required and must be a non-empty string" {
		t.Errorf("actor.message = %q, want the bridge's own sentence", got.Error.Actor.Message)
	}
	if got.Error.Actor.Class != "actor_rejected_input" {
		t.Errorf("actor.class = %q, want the bridge's own claim", got.Error.Actor.Class)
	}
}

// The engine's classification and the bridge's claim stay separate records
// of separate facts, all the way into the run view: a bridge that names a
// class this control plane has never heard of gets its claim recorded and
// changes nothing about the attempt's own status.
func TestRunViewKeepsTheEngineClassSeparateFromTheActorsClaim(t *testing.T) {
	got, status := refusedRunView(t, http.StatusBadRequest, "application/json",
		`{"error":"refused","class":"the_bridge_made_this_up"}`)

	if engine.TechStatus(status) != engine.StatusContractRejected {
		t.Errorf("attempt status = %q, want %q — a bridge cannot invent its own status", status, engine.StatusContractRejected)
	}
	if got.Error.Class != string(actors.ClassActorRejectedInput) {
		t.Errorf("recorded class = %q, want %q", got.Error.Class, actors.ClassActorRejectedInput)
	}
	if got.Error.Actor == nil || got.Error.Actor.Class != "the_bridge_made_this_up" {
		t.Fatalf("the bridge's class claim was not recorded beside the engine's: %+v", got.Error.Actor)
	}
}

// Acceptance criterion 2: a bridge cannot write unbounded text into the run
// view. The attempt result is read by operators and by the UI, and it rides
// into event payloads — an actor is not entitled to any amount of either.
func TestRunViewBoundsTheActorsRejectionBody(t *testing.T) {
	payload, err := json.Marshal(map[string]string{
		"error": strings.Repeat("x", 500_000),
		"class": strings.Repeat("c", 4096),
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	got, _ := refusedRunView(t, http.StatusBadRequest, "application/json", string(payload))

	if got.Error.Actor == nil {
		t.Fatal("an oversized rejection body produced no actor block at all")
	}
	if len(got.Error.Actor.Message) > actors.MaxActorMessageBytes {
		t.Errorf("actor.message is %d bytes in the run view, want at most %d",
			len(got.Error.Actor.Message), actors.MaxActorMessageBytes)
	}
	if len(got.Error.Actor.Class) > actors.MaxActorClassBytes {
		t.Errorf("actor.class is %d bytes in the run view, want at most %d",
			len(got.Error.Actor.Class), actors.MaxActorClassBytes)
	}
	if !got.Error.Actor.Truncated {
		t.Error("a 500 KB rejection body was recorded without being marked truncated")
	}
}

// Acceptance criterion 3: a non-JSON refusal still records a technical
// status and never takes the worker down with it.
func TestNonJSONRejectionStillRecordsATechnicalStatus(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		wantActor   bool
	}{
		{name: "empty body", contentType: "", body: "", wantActor: false},
		{name: "html error page", contentType: "text/html",
			body: "<html><head><title>400 Bad Request</title></head><body>nginx</body></html>", wantActor: true},
		{name: "truncated json", contentType: "application/json",
			body: `{"error":"input.instruction is requir`, wantActor: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, status := refusedRunView(t, http.StatusBadRequest, tc.contentType, tc.body)

			if engine.TechStatus(status) != engine.StatusContractRejected {
				t.Errorf("attempt status = %q, want %q", status, engine.StatusContractRejected)
			}
			if got.Error.Class != string(actors.ClassActorRejectedInput) {
				t.Errorf("recorded class = %q, want %q", got.Error.Class, actors.ClassActorRejectedInput)
			}
			if got.Error.Detail == "" {
				t.Error("a refusal with an unreadable body recorded no detail at all")
			}
			if !tc.wantActor {
				if got.Error.Actor != nil {
					t.Fatalf("an empty body produced an actor block: %+v", got.Error.Actor)
				}
				return
			}
			if got.Error.Actor == nil {
				t.Fatalf("body %q recorded no actor block", tc.body)
			}
			if got.Error.Actor.Message != "" {
				t.Errorf("actor.message = %q, want empty: this body declared no error string", got.Error.Actor.Message)
			}
			if got.Error.Actor.Body != tc.body {
				t.Errorf("actor.body = %q, want the unparseable body verbatim (%q)", got.Error.Actor.Body, tc.body)
			}
		})
	}
}
