package actors_test

import (
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// Continuation carriage on the protocol (task t4, spec claim c3 / honesty
// h2, docs/adr/0010-continuation-ref-on-request.md).
//
// §13.2 always declared `continuation_ref` on the synchronous result. What
// was missing was the other three quarters of the round trip: a request
// field to hand a prior ref back with, and the field on the §13.4 `completed`
// payload an actor that finished LATE reports through — which is the path a
// long session takes, so a ref that only the synchronous body could carry was
// unreachable exactly where it matters most.

// A dispatch with no prior ref omits the key entirely rather than sending
// null or "". A bridge's "was I given a session to resume" test is therefore
// key presence, and no bridge can mistake an empty string for a handle.
func TestInvocationRequestOmitsAbsentContinuationRef(t *testing.T) {
	encoded, err := json.Marshal(actors.InvocationRequest{
		ProtocolVersion: actors.ProtocolVersion,
		RunID:           "run_01J",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := decoded["continuation_ref"]; present {
		t.Errorf("request body = %s, want no continuation_ref key at all when there is no prior ref", encoded)
	}
}

// A dispatch that HAS a prior ref sends it under §13.2's own field name —
// one PRD vocabulary word for one fact, the direction supplied by the
// message rather than by a second name (ADR 0010 §1).
func TestInvocationRequestCarriesContinuationRef(t *testing.T) {
	ref := "sess_01JQZPRIOR"
	encoded, err := json.Marshal(actors.InvocationRequest{
		ProtocolVersion: actors.ProtocolVersion,
		RunID:           "run_01J",
		ContinuationRef: &ref,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded struct {
		ContinuationRef *string `json:"continuation_ref"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.ContinuationRef == nil || *decoded.ContinuationRef != ref {
		t.Errorf("request continuation_ref = %v, want %q (body %s)", decoded.ContinuationRef, ref, encoded)
	}
}

// The async twin of a synchronous result's ref: a `completed` callback's
// `continuation_ref` lands on the attempt row this callback commits, exactly
// as its Usage block does (task t1's TestCallbackCompletedPersistsUsage).
// Without this, stickiness would be reachable only for turns short enough to
// answer inline.
func TestCallbackCompletedPersistsContinuationRef(t *testing.T) {
	f := newAsyncFixture(t)

	ref := "sess_01JQZASYNC"
	payload, _ := json.Marshal(actors.CompletedPayload{
		Outcome:         "completed",
		Output:          json.RawMessage(`{"summary":"done late"}`),
		ContinuationRef: &ref,
	})

	result := f.handle(actors.CallbackEvent{
		EventID: "ev-continuation", Sequence: 1, Kind: actors.EventCompleted, Payload: payload,
	})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	if attempts[0].ContinuationRef == nil || *attempts[0].ContinuationRef != ref {
		t.Errorf("ContinuationRef = %v, want %q persisted on the committed attempt",
			attempts[0].ContinuationRef, ref)
	}
}

// A `completed` callback that offers no ref leaves the attempt's ref nil.
// A resumable conversation nobody told us about is indistinguishable from
// none, and the honest reading is the conservative one: nothing to resume.
func TestCallbackCompletedWithoutContinuationRefStaysNil(t *testing.T) {
	f := newAsyncFixture(t)

	result := f.handle(completedEvent("ev-no-continuation", 1, "done"))
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts[0].ContinuationRef != nil {
		t.Errorf("ContinuationRef = %q, want nil for a completed event that offered none",
			*attempts[0].ContinuationRef)
	}
}
