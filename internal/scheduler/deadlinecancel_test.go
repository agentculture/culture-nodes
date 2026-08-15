package scheduler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/scheduler"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// The third cancel origin (task t12, spec claim c46).
//
// Three places in this system ask an actor to stop, and they are separated by
// WHO DECIDED, not by what the request looks like on the wire — internal/api's
// cancelpropagate.go and internal/worker's branchcancel.go both say so in
// their own comments, and the deadline follows them. These tests hold that
// line from the only vantage point that matters for c46's honesty condition:
// an operator with nothing but the run's event stream.
//
// This file imports internal/api and internal/worker purely to name their
// constants. Neither imports internal/scheduler, so the three can be compared
// here without a cycle, and comparing them somewhere is the point — a
// distinctness claim asserted inside one package is a claim about a string
// literal, not about the system.

// The three types are distinct, and each is distinct from the committed-state
// event it shadows. A test that only checked the deadline type against the
// other two would still pass if someone gave the deadline the same name as
// the engine's own branch.cancelled — which is the other half of the
// SENT-versus-DID distinction both sibling comments are built on.
func TestThreeCancelOriginsAreDistinguishable(t *testing.T) {
	byType := map[string]string{
		api.TypeActorCancelRequested:          "operator: a run was cancelled",
		worker.TypeBranchCancelRequested:      "barrier: a sibling branch was reaped",
		scheduler.TypeDeadlineCancelRequested: "deadline: a declared clock ran out",
	}
	if len(byType) != 3 {
		t.Fatalf("the three cancel-requested types collide: %v", byType)
	}
	for typ := range byType {
		if typ == "" {
			t.Fatal("a cancel-requested type is empty")
		}
	}
	// An operator scanning for "who stopped this session" reads the first
	// segment after the prefix. Each origin has to own one.
	const prefix = "dev.culture.nodes."
	origins := map[string]bool{}
	for typ := range byType {
		if len(typ) <= len(prefix) || typ[:len(prefix)] != prefix {
			t.Errorf("cancel type %q does not carry the required %q prefix", typ, prefix)
			continue
		}
		rest := typ[len(prefix):]
		origin := rest
		for i := 0; i < len(rest); i++ {
			if rest[i] == '.' {
				origin = rest[:i]
				break
			}
		}
		if origins[origin] {
			t.Errorf("origin segment %q is used by more than one cancel type: %v", origin, byType)
		}
		origins[origin] = true
	}
}

// c46's honesty condition, end to end: a timeout-driven session stop is
// legible as a timeout-driven session stop from the run's events alone. The
// deadline fires against a bridge that answers the §13.6 cancel, and the run
// picks up the deadline origin's event and neither of the other two.
func TestSchedulerDeadlineCancelRecordsItsOwnOriginOnTheRunsEventStream(t *testing.T) {
	s := requireStore(t)
	f := newDeadlineFixture(t, s)
	inv := f.startAsyncWait(time.Now().Add(-time.Second))

	cancelled := make(chan struct{}, 1)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case cancelled <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer bridge.Close()
	if _, err := s.Pool().Exec(context.Background(), `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref, capabilities, metadata)
		VALUES ($1, $2, 'company/builder', 1, 'agent', 'http', $3, '{}', '{}')`,
		"actor_"+store.NewULID(), f.ns.ID, bridge.URL); err != nil {
		t.Fatalf("register actor bridge: %v", err)
	}

	sch := scheduler.New(s, scheduler.Options{TickInterval: 25 * time.Millisecond})
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = sch.Run(runCtx) }()

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the deadline's cancel never reached the actor bridge")
	}
	// The event is appended after the Cancel call returns, so wait for it
	// rather than for the request that precedes it.
	waitFor(t, 5*time.Second, func() bool {
		return len(eventsOfType(t, s, f.runID, scheduler.TypeDeadlineCancelRequested)) == 1
	})

	events := eventsOfType(t, s, f.runID, scheduler.TypeDeadlineCancelRequested)
	if len(events) != 1 {
		t.Fatalf("deadline cancel events = %d, want exactly 1", len(events))
	}
	got := events[0]
	if got["outcome"] != "sent" {
		t.Errorf("outcome = %v (detail %v), want sent", got["outcome"], got["detail"])
	}
	if got["invocation_id"] != inv.InvocationID {
		t.Errorf("invocation_id = %v, want %s", got["invocation_id"], inv.InvocationID)
	}
	if got["attempt_id"] != inv.AttemptID {
		t.Errorf("attempt_id = %v, want %s", got["attempt_id"], inv.AttemptID)
	}
	if got["node_id"] != "build" {
		t.Errorf("node_id = %v, want build", got["node_id"])
	}

	// And it is not mistakable for either of the other two origins. Answering
	// "did the timeout stop this session, or did an operator?" must not
	// require reading a payload.
	all := runEventTypes(t, s, f.runID)
	if containsString(all, api.TypeActorCancelRequested) {
		t.Errorf("run events = %v, want no %s: no operator cancelled this run", all, api.TypeActorCancelRequested)
	}
	if containsString(all, worker.TypeBranchCancelRequested) {
		t.Errorf("run events = %v, want no %s: no branch was reaped", all, worker.TypeBranchCancelRequested)
	}
}

// An attempted-but-failed cancel is still evidence, and here it is the most
// valuable kind: the attempt row says timed_out either way, so this event is
// the only thing that distinguishes a session that was told to stop from one
// still burning tokens with nobody having reached it. The fixture registers
// no actor, so the actor_ref on the invocation resolves to nothing.
func TestSchedulerDeadlineCancelRecordsAnUnreachableActor(t *testing.T) {
	s := requireStore(t)
	f := newDeadlineFixture(t, s)
	f.startAsyncWait(time.Now().Add(-time.Second))

	sch := scheduler.New(s, scheduler.Options{TickInterval: 25 * time.Millisecond})
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = sch.Run(runCtx) }()

	waitFor(t, 5*time.Second, func() bool {
		return len(eventsOfType(t, s, f.runID, scheduler.TypeDeadlineCancelRequested)) == 1
	})

	got := eventsOfType(t, s, f.runID, scheduler.TypeDeadlineCancelRequested)[0]
	if got["outcome"] != "failed" {
		t.Errorf("outcome = %v, want failed: no actor row exists for this invocation's actor_ref", got["outcome"])
	}
	if got["detail"] == "" || got["detail"] == nil {
		t.Error("a failed cancel must say why; an outcome with no detail is not evidence")
	}
}

// eventsOfType returns the decoded payloads of one event type on a run, in
// sequence order.
func eventsOfType(t *testing.T, s *postgres.Store, runID, eventType string) []map[string]any {
	t.Helper()
	rows, err := s.Pool().Query(context.Background(),
		`SELECT data FROM events WHERE aggregate_id = $1 AND event_type = $2 ORDER BY sequence`, runID, eventType)
	if err != nil {
		t.Fatalf("eventsOfType: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("eventsOfType: scan: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("eventsOfType: decode %s payload: %v", eventType, err)
		}
		out = append(out, payload)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("eventsOfType: %v", err)
	}
	return out
}
