package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// The clarify-then-commit gate's two record types (issue #67, task t14).
//
// The ledger fit the issue states, and the one this task may not bend: the
// preflight is DERIVED — a deterministic function of advertised host state
// and the task declaration — and the acknowledgement is PROPOSED by the
// actor. Neither is observed, and no actor promotes its own claim.
//
// The two rules these tests pin are the ones the ordinary §10.4 producer
// matrix would NOT have caught on its own:
//
//   - An agent (or a human) may propose freely, so without a record-type
//     rule an actor could propose a `dispatch_preflight` — writing its own
//     briefing and then acknowledging it. RulePreflightDerivedOnly refuses
//     that.
//   - A deterministic producer may derive freely, so without a record-type
//     rule the ENGINE could derive a `dispatch_acknowledgement` — deciding
//     on the actor's behalf that the actor understood. That is the engine
//     manufacturing the very evidence the gate exists to create, and
//     RuleAcknowledgementNeverDerived refuses it.

func preflightPayload() json.RawMessage {
	return json.RawMessage(`{
		"verdict": "hold",
		"protocol_version": "1.0",
		"task": {"run_id": "run_1", "node_id": "fix", "actor_key": "company/codex-thor"},
		"host_capabilities": {"hostname": "thor"},
		"refusal": "This dispatch does not proceed until this preflight is acknowledged.",
		"expires_at": "2026-08-14T12:15:00Z"
	}`)
}

func acknowledgementPayload() json.RawMessage {
	return json.RawMessage(`{
		"verdict": "proceed",
		"preflight_ref": "ledger_00000000000000000000000001",
		"preflight_digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	}`)
}

func TestEngineDerivesThePreflightAndAnActorProposesTheAcknowledgement(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLedger(t)

	pre, err := l.Append(ctx, ledger.Record{
		RecordType: ledger.RecordDispatchPreflight,
		RunID:      testRunID,
		Origin:     engineOrigin,
		Authority:  ledger.AuthorityDerived,
		Data:       preflightPayload(),
	})
	if err != nil {
		t.Fatalf("append derived preflight: %v", err)
	}
	if pre.Authority != ledger.AuthorityDerived {
		t.Errorf("preflight authority = %q, want derived", pre.Authority)
	}

	ack, err := l.Append(ctx, ledger.Record{
		RecordType: ledger.RecordDispatchAcknowledgement,
		RunID:      testRunID,
		Origin:     agentOrigin,
		Authority:  ledger.AuthorityProposed,
		SubjectRef: ledger.NullableID(pre.ID),
		Data:       acknowledgementPayload(),
	})
	if err != nil {
		t.Fatalf("append proposed acknowledgement: %v", err)
	}
	if ack.Authority != ledger.AuthorityProposed {
		t.Errorf("acknowledgement authority = %q, want proposed: an actor saying it understood is a claim, not evidence",
			ack.Authority)
	}
	if ack.SubjectRef.String() != pre.ID {
		t.Errorf("acknowledgement subject = %q, want the preflight it answers (%s)", ack.SubjectRef, pre.ID)
	}
}

func TestNoProducerMayPropose_ItsOwnPreflight(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		origin ledger.Origin
	}{
		{"agent", agentOrigin},
		{"human", humanOrigin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, store := newTestLedger(t)
			_, err := l.Append(ctx, ledger.Record{
				RecordType: ledger.RecordDispatchPreflight,
				RunID:      testRunID,
				Origin:     tc.origin,
				Authority:  ledger.AuthorityProposed,
				Data:       preflightPayload(),
			})
			var authErr *ledger.AuthorityError
			if !errors.As(err, &authErr) {
				t.Fatalf("Append error = %v (%T), want an *AuthorityError refusal", err, err)
			}
			if authErr.Rule != ledger.RulePreflightDerivedOnly {
				t.Errorf("rule = %q, want %q", authErr.Rule, ledger.RulePreflightDerivedOnly)
			}
			if store.count() != 0 {
				t.Errorf("stored %d records after a refused append, want 0", store.count())
			}
		})
	}
}

func TestADeterministicProducerMayNotAcknowledgeOnAnActorsBehalf(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		origin ledger.Origin
	}{
		{"engine", engineOrigin},
		{"validator", validatorOrigin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, store := newTestLedger(t)
			_, err := l.Append(ctx, ledger.Record{
				RecordType: ledger.RecordDispatchAcknowledgement,
				RunID:      testRunID,
				Origin:     tc.origin,
				Authority:  ledger.AuthorityDerived,
				Data:       acknowledgementPayload(),
			})
			var authErr *ledger.AuthorityError
			if !errors.As(err, &authErr) {
				t.Fatalf("Append error = %v (%T), want an *AuthorityError refusal", err, err)
			}
			if authErr.Rule != ledger.RuleAcknowledgementNeverDerived {
				t.Errorf("rule = %q, want %q", authErr.Rule, ledger.RuleAcknowledgementNeverDerived)
			}
			if store.count() != 0 {
				t.Errorf("stored %d records after a refused append, want 0", store.count())
			}
		})
	}
}

// A record that states nothing is not a briefing. Both types require their
// core fields, the way `grade` does and unlike the documentary Phase 0
// payloads — an empty preflight would refuse a dispatch in order to tell the
// actor nothing, and an empty acknowledgement would name nothing it
// acknowledged.
func TestBothTypesRequireTheirCoreFields(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLedger(t)

	if _, err := l.Append(ctx, ledger.Record{
		RecordType: ledger.RecordDispatchPreflight,
		RunID:      testRunID,
		Origin:     engineOrigin,
		Authority:  ledger.AuthorityDerived,
		Data:       json.RawMessage(`{}`),
	}); err == nil {
		t.Error("an empty preflight payload was accepted")
	}

	if _, err := l.Append(ctx, ledger.Record{
		RecordType: ledger.RecordDispatchAcknowledgement,
		RunID:      testRunID,
		Origin:     agentOrigin,
		Authority:  ledger.AuthorityProposed,
		Data:       json.RawMessage(`{"verdict":"proceed"}`),
	}); err == nil {
		t.Error("an acknowledgement naming no preflight was accepted")
	}
}
