package actors_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// The actor's own rejection reason (task t3, issue #125).
//
// Run 01M04Q26ZPNKTXVNBGSDS1YR9F recorded `actor_rejected_input (HTTP 400):
// actor answered Bad Request` and nothing else, while the bridge's response
// body said exactly what was wrong with the dispatch. These tests pin the
// carrying of that body — bounded, sanitized, and kept distinct from the
// engine's own classification.

// refusingActor answers every invocation with one fixed status and body.
func refusingActor(t *testing.T, status int, contentType, body string) (*actors.Client, actors.Endpoint) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return newClient(t), actors.Endpoint{URL: server.URL}
}

// invokeErr drives one invocation and returns the classified failure.
func invokeErr(t *testing.T, client *actors.Client, endpoint actors.Endpoint) error {
	t.Helper()
	_, err := client.Invoke(context.Background(), endpoint, testRequest())
	if err == nil {
		t.Fatal("invocation succeeded; this actor always refuses")
	}
	return err
}

// Acceptance criterion 1 (client half): a 400 with a {error, class} body is
// carried out of the client as the actor's own words, not as a status line.
func TestActorRejectionBodyIsCarriedOffTheWire(t *testing.T) {
	client, endpoint := refusingActor(t, http.StatusBadRequest, "application/json",
		`{"error":"input.instruction is required and must be a non-empty string","class":"actor_rejected_input"}`)

	err := invokeErr(t, client, endpoint)
	actorErr := actors.ActorErrorOf(err)
	if actorErr == nil {
		t.Fatal("no actor error carried off a {error, class} 400 body")
	}
	if actorErr.Message != "input.instruction is required and must be a non-empty string" {
		t.Errorf("actor message = %q, want the bridge's own error text", actorErr.Message)
	}
	if actorErr.Class != "actor_rejected_input" {
		t.Errorf("actor class = %q, want the bridge's own class claim", actorErr.Class)
	}
	if actorErr.Truncated {
		t.Error("a 100-byte body was reported as truncated")
	}
}

// The design note: the bridge's class claim and the control plane's own
// classification are separate facts. A bridge cannot talk the engine into a
// different §13.5 class by inventing one — the claim is recorded beside the
// engine's classification, never in place of it.
func TestActorClassClaimDoesNotBecomeTheEngineClass(t *testing.T) {
	client, endpoint := refusingActor(t, http.StatusBadRequest, "application/json",
		`{"error":"nope","class":"totally_made_up"}`)

	err := invokeErr(t, client, endpoint)
	class, ok := actors.ClassOf(err)
	if !ok {
		t.Fatal("a refused invocation produced no §13.5 class")
	}
	if class != actors.ClassActorRejectedInput {
		t.Errorf("engine class = %q, want %q derived from the 400 status", class, actors.ClassActorRejectedInput)
	}
	if !class.Valid() {
		t.Errorf("engine class %q is not one of §13.5's classes", class)
	}
	actorErr := actors.ActorErrorOf(err)
	if actorErr == nil || actorErr.Class != "totally_made_up" {
		t.Fatalf("the bridge's class claim was not recorded verbatim: %+v", actorErr)
	}
}

// Acceptance criterion 2: the copied body is length-bounded. A bridge must
// not be able to write unbounded text into anything the operator reads.
func TestActorRejectionBodyIsLengthBounded(t *testing.T) {
	long := strings.Repeat("x", 200_000)
	payload, err := json.Marshal(map[string]string{"error": long, "class": strings.Repeat("c", 5000)})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	client, endpoint := refusingActor(t, http.StatusBadRequest, "application/json", string(payload))

	actorErr := actors.ActorErrorOf(invokeErr(t, client, endpoint))
	if actorErr == nil {
		t.Fatal("no actor error carried off an oversized 400 body")
	}
	if len(actorErr.Message) > actors.MaxActorMessageBytes {
		t.Errorf("actor message is %d bytes, want at most %d", len(actorErr.Message), actors.MaxActorMessageBytes)
	}
	if len(actorErr.Class) > actors.MaxActorClassBytes {
		t.Errorf("actor class is %d bytes, want at most %d", len(actorErr.Class), actors.MaxActorClassBytes)
	}
	if !actorErr.Truncated {
		t.Error("a 200 KB error text was carried without being marked truncated")
	}
}

// The same bound on the unstructured path: a body that names no error
// string is snipped, not carried whole.
func TestUnstructuredRejectionBodyIsLengthBounded(t *testing.T) {
	client, endpoint := refusingActor(t, http.StatusBadRequest, "text/html",
		"<html><body>"+strings.Repeat("y", 200_000)+"</body></html>")

	actorErr := actors.ActorErrorOf(invokeErr(t, client, endpoint))
	if actorErr == nil {
		t.Fatal("no actor error carried off an oversized HTML 400 body")
	}
	if len(actorErr.Body) > actors.MaxActorMessageBytes {
		t.Errorf("actor body snippet is %d bytes, want at most %d", len(actorErr.Body), actors.MaxActorMessageBytes)
	}
	if !actorErr.Truncated {
		t.Error("a 200 KB HTML body was carried without being marked truncated")
	}
}

// Acceptance criterion 3 (client half): a non-JSON body still classifies,
// still reports, and never panics. Every case here is one a real deployment
// produces — a bare status with no body, a reverse proxy's HTML error page,
// a body cut off mid-write.
func TestNonJSONRejectionBodiesStillClassify(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		wantBody    string // "" means: any non-empty snippet is acceptable
		wantNil     bool
	}{
		{name: "empty body", contentType: "", body: "", wantNil: true},
		{name: "whitespace only", contentType: "text/plain", body: "   \n\t ", wantNil: true},
		{name: "html error page", contentType: "text/html", body: "<html><head><title>400</title></head></html>",
			wantBody: "<html><head><title>400</title></head></html>"},
		{name: "truncated json", contentType: "application/json", body: `{"error":"input.instruction is req`,
			wantBody: `{"error":"input.instruction is req`},
		{name: "plain text", contentType: "text/plain", body: "Bad Request", wantBody: "Bad Request"},
		{name: "json without an error key", contentType: "application/json", body: `{"detail":"nope"}`,
			wantBody: `{"detail":"nope"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, endpoint := refusingActor(t, http.StatusBadRequest, tc.contentType, tc.body)
			err := invokeErr(t, client, endpoint)

			// The technical classification is unaffected by an unreadable body.
			if class, ok := actors.ClassOf(err); !ok || class != actors.ClassActorRejectedInput {
				t.Fatalf("class = %q (ok=%v), want %q from the 400 status", class, ok, actors.ClassActorRejectedInput)
			}

			actorErr := actors.ActorErrorOf(err)
			if tc.wantNil {
				if actorErr != nil {
					t.Fatalf("an empty body produced an actor error: %+v", actorErr)
				}
				return
			}
			if actorErr == nil {
				t.Fatalf("body %q produced no actor error at all", tc.body)
			}
			if actorErr.Message != "" {
				t.Errorf("actor message = %q, want empty: this body declared no error string", actorErr.Message)
			}
			if actorErr.Body != tc.wantBody {
				t.Errorf("actor body snippet = %q, want %q", actorErr.Body, tc.wantBody)
			}
		})
	}
}

// Diagnostic text from an actor is data, not markup. Control characters —
// an ANSI escape in particular — must not survive into a terminal or a UI
// that renders the run view.
func TestActorRejectionBodyIsSanitized(t *testing.T) {
	payload, err := json.Marshal(map[string]string{
		"error": "line one\nline two\x1b[31mred\x07",
		"class": "actor_rejected_input\n",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	client, endpoint := refusingActor(t, http.StatusBadRequest, "application/json", string(payload))

	actorErr := actors.ActorErrorOf(invokeErr(t, client, endpoint))
	if actorErr == nil {
		t.Fatal("no actor error carried")
	}
	if strings.ContainsAny(actorErr.Message, "\x1b\x07\n") {
		t.Errorf("actor message kept control characters: %q", actorErr.Message)
	}
	if !strings.Contains(actorErr.Message, "line one") || !strings.Contains(actorErr.Message, "red") {
		t.Errorf("sanitizing ate the readable text: %q", actorErr.Message)
	}
	if actorErr.Class != "actor_rejected_input" {
		t.Errorf("actor class = %q, want the trimmed claim", actorErr.Class)
	}
}

// The dial-in path parses the same refusal the outbound HTTP path does. The
// cutover this cycle is running would otherwise silently drop the reason
// again the moment a bridge stops being reachable by address.
func TestDialInRefusalCarriesTheActorError(t *testing.T) {
	_, err := actors.ParseInvocationResponse(http.StatusBadRequest,
		[]byte(`{"error":"input.repo is required","class":"actor_rejected_input"}`))
	if err == nil {
		t.Fatal("a 400 dial-in response parsed as a success")
	}
	actorErr := actors.ActorErrorOf(err)
	if actorErr == nil || actorErr.Message != "input.repo is required" || actorErr.Class != "actor_rejected_input" {
		t.Fatalf("dial-in refusal carried %+v, want the bridge's own error and class", actorErr)
	}
}

// ActorErrorOf is UsageOf's shape: an error that is not a classified
// invocation failure yields nil rather than a panic.
func TestActorErrorOfIgnoresUnrelatedErrors(t *testing.T) {
	if got := actors.ActorErrorOf(nil); got != nil {
		t.Errorf("ActorErrorOf(nil) = %+v, want nil", got)
	}
	if got := actors.ActorErrorOf(context.Canceled); got != nil {
		t.Errorf("ActorErrorOf(context.Canceled) = %+v, want nil", got)
	}
}
