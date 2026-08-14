package postgres_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The clarify-then-commit gate's durable state (issue #67, task t14;
// migration 0026).
//
// These tests pin the properties the dispatch site depends on, all four of
// them inherited from the protocol this generalizes
// (deploy/prod/install-secrets.sh, tests/deploy/destructiveconfirm_test.go):
//
//  1. a freshly issued preflight is NOT acknowledged — the first ask is
//     always refused;
//  2. an acknowledgement is SINGLE-USE — exactly one dispatch can consume
//     it, including under concurrency;
//  3. it is WINDOWED — an expired one authorizes nothing, whether or not it
//     was acknowledged;
//  4. and the configuration-time refusal holds even for a caller that goes
//     around the Go check, because the constraint is in the schema.

func issueInput(ns postgres.Namespace, nodeRunID string, expiresIn time.Duration) postgres.IssuePreflightInput {
	return postgres.IssuePreflightInput{
		NamespaceID:  ns.ID,
		RunID:        "run_" + nodeRunID,
		NodeRunID:    nodeRunID,
		NodeID:       "fix",
		ActorKey:     "company/fixer",
		ActorID:      "actor_row_1",
		RecordID:     "ledger_" + nodeRunID,
		RecordDigest: "sha256:" + strings.Repeat("a", 64),
		ExpiresAt:    time.Now().UTC().Add(expiresIn),
	}
}

func TestAnIssuedPreflightIsNotYetAcknowledged(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-preflight-issue")

	issued, err := s.IssuePreflight(ctx, issueInput(ns, "nr_issue_1", 15*time.Minute))
	if err != nil {
		t.Fatalf("IssuePreflight: %v", err)
	}
	if issued.ID == "" {
		t.Fatal("IssuePreflight returned no id")
	}
	if issued.Acknowledged() {
		t.Error("a freshly issued preflight reports itself acknowledged; the first ask is always refused")
	}
	if issued.Consumed() {
		t.Error("a freshly issued preflight reports itself consumed")
	}

	open, ok, err := s.OpenPreflight(ctx, ns.ID, "nr_issue_1")
	if err != nil || !ok {
		t.Fatalf("OpenPreflight: ok=%v err=%v", ok, err)
	}
	if open.ID != issued.ID || open.RecordDigest != issued.RecordDigest {
		t.Errorf("OpenPreflight returned %+v, want the row just issued (%s)", open, issued.ID)
	}

	// Consuming an unacknowledged preflight must be impossible: it is the
	// acknowledgement that authorizes the dispatch, not the briefing.
	if consumed, err := s.ConsumePreflight(ctx, ns.ID, issued.ID, "att_1", time.Now().UTC()); err != nil {
		t.Fatalf("ConsumePreflight: %v", err)
	} else if consumed {
		t.Error("an unacknowledged preflight was consumable; the dispatch would have proceeded unacknowledged")
	}
}

func TestAnAcknowledgementIsSingleUse(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-preflight-single-use")

	issued, err := s.IssuePreflight(ctx, issueInput(ns, "nr_single_1", 15*time.Minute))
	if err != nil {
		t.Fatalf("IssuePreflight: %v", err)
	}
	acked, err := s.AcknowledgePreflight(ctx, postgres.AcknowledgePreflightInput{
		NamespaceID:             ns.ID,
		ID:                      issued.ID,
		AcknowledgedBy:          "actor_row_1",
		AcknowledgementRecordID: "ledger_ack_1",
		Now:                     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("AcknowledgePreflight: %v", err)
	}
	if !acked.Acknowledged() {
		t.Fatal("AcknowledgePreflight returned a row that is not acknowledged")
	}

	consumed, err := s.ConsumePreflight(ctx, ns.ID, issued.ID, "att_1", time.Now().UTC())
	if err != nil {
		t.Fatalf("ConsumePreflight: %v", err)
	}
	if !consumed {
		t.Fatal("an acknowledged, unexpired preflight was not consumable")
	}

	// The second dispatch needs its own confirmation — install-secrets.sh's
	// "this file is consumed on use" property, in a table.
	again, err := s.ConsumePreflight(ctx, ns.ID, issued.ID, "att_2", time.Now().UTC())
	if err != nil {
		t.Fatalf("second ConsumePreflight: %v", err)
	}
	if again {
		t.Error("one acknowledgement authorized two dispatches; it must be single-use")
	}

	// And it is no longer the open preflight for that node run, so the next
	// claim composes a fresh briefing rather than reviving this one.
	if _, ok, err := s.OpenPreflight(ctx, ns.ID, "nr_single_1"); err != nil {
		t.Fatalf("OpenPreflight: %v", err)
	} else if ok {
		t.Error("a consumed preflight is still reported as the open one")
	}
}

// Concurrency is the reason this lives in a table rather than in a third
// ledger record: two workers holding two claims of the same node run must
// not both read one acknowledgement as unconsumed.
func TestOnlyOneConcurrentDispatchConsumesAnAcknowledgement(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-preflight-concurrent")

	issued, err := s.IssuePreflight(ctx, issueInput(ns, "nr_concurrent_1", 15*time.Minute))
	if err != nil {
		t.Fatalf("IssuePreflight: %v", err)
	}
	if _, err := s.AcknowledgePreflight(ctx, postgres.AcknowledgePreflightInput{
		NamespaceID:             ns.ID,
		ID:                      issued.ID,
		AcknowledgedBy:          "actor_row_1",
		AcknowledgementRecordID: "ledger_ack_c",
		Now:                     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AcknowledgePreflight: %v", err)
	}

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := s.ConsumePreflight(ctx, ns.ID, issued.ID, "att_race", time.Now().UTC())
			if err != nil {
				t.Errorf("ConsumePreflight: %v", err)
				return
			}
			if ok {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if winners != 1 {
		t.Errorf("%d of %d concurrent dispatches consumed one acknowledgement, want exactly 1", winners, racers)
	}
}

func TestAnExpiredPreflightAuthorizesNothing(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-preflight-expiry")

	// Issued in the past so it is already outside its window.
	issued, err := s.IssuePreflight(ctx, issueInput(ns, "nr_expired_1", -time.Second))
	if err != nil {
		t.Fatalf("IssuePreflight: %v", err)
	}
	if _, err := s.AcknowledgePreflight(ctx, postgres.AcknowledgePreflightInput{
		NamespaceID:             ns.ID,
		ID:                      issued.ID,
		AcknowledgedBy:          "actor_row_1",
		AcknowledgementRecordID: "ledger_ack_e",
		Now:                     time.Now().UTC(),
	}); err == nil {
		t.Error("an expired preflight accepted an acknowledgement; the window is what makes a stale briefing safe")
	}

	now := time.Now().UTC()
	if consumed, err := s.ConsumePreflight(ctx, ns.ID, issued.ID, "att_e", now); err != nil {
		t.Fatalf("ConsumePreflight: %v", err)
	} else if consumed {
		t.Error("an expired preflight was consumable")
	}

	row, err := s.Preflight(ctx, ns.ID, issued.ID)
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !row.Expired(now) {
		t.Error("the row does not report itself expired")
	}
}

// A pending preflight has to be FINDABLE, or a bridge could only learn about
// it by watching the event stream at exactly the right moment.
func TestPendingPreflightsAreListableByActor(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-preflight-pending")

	waiting, err := s.IssuePreflight(ctx, issueInput(ns, "nr_pending_1", 15*time.Minute))
	if err != nil {
		t.Fatalf("IssuePreflight: %v", err)
	}
	done, err := s.IssuePreflight(ctx, issueInput(ns, "nr_pending_2", 15*time.Minute))
	if err != nil {
		t.Fatalf("IssuePreflight: %v", err)
	}
	if _, err := s.AcknowledgePreflight(ctx, postgres.AcknowledgePreflightInput{
		NamespaceID:             ns.ID,
		ID:                      done.ID,
		AcknowledgedBy:          "actor_row_1",
		AcknowledgementRecordID: "ledger_ack_p",
		Now:                     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AcknowledgePreflight: %v", err)
	}

	pending, err := s.PendingPreflights(ctx, ns.ID, "company/fixer", time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("PendingPreflights: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != waiting.ID {
		t.Fatalf("pending = %d rows (%v), want only the unacknowledged one (%s)", len(pending), pending, waiting.ID)
	}
}

// Acceptance criterion 3, at the door the Go check cannot cover: raw SQL.
// The constraint is what makes "refused at configuration time" true for an
// operator with psql, not only for one going through the API.
func TestRawSQLCannotEnableTheGateWithoutASurface(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-preflight-constraint")

	insert := func(name, capabilities, metadata string) error {
		_, err := s.Pool().Exec(ctx, `
			INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, capabilities, metadata)
			VALUES ($1, $2, $3, 1, 'agent', 'http', $4::jsonb, $5::jsonb)
		`, "actor_raw_"+name, ns.ID, "company/raw-"+name, capabilities, metadata)
		return err
	}

	if err := insert("ungated-surface-less", `{}`, `{"preflight_gate":{"enabled":true}}`); err == nil {
		t.Error("raw SQL enabled the gate for an actor advertising no surface")
	}
	if err := insert("gated", `{"preflight":{"protocol_version":"1.0","host":{"hostname":"h"}}}`,
		`{"preflight_gate":{"enabled":true}}`); err != nil {
		t.Errorf("raw SQL could not enable the gate for an actor that DOES advertise a surface: %v", err)
	}
	// And the ten existing actors — no gate block at all — are untouched.
	if err := insert("plain", `{}`, `{"auth_token_env":"NODES_ACTOR_TOKEN"}`); err != nil {
		t.Errorf("an ungated actor was refused: %v", err)
	}
}

// RegisterActor is the other door, and it refuses with an explanation rather
// than a constraint violation.
func TestRegisterActorRefusesAGateWithoutASurface(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-preflight-register")

	store, err := postgres.NewEngineStore(s, ns.ID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}

	_, err = store.RegisterActor(ctx, postgres.RegisterActorParams{
		ActorKey: "company/ungated",
		Kind:     "agent",
		Protocol: "http",
		Metadata: json.RawMessage(`{"preflight_gate":{"enabled":true}}`),
	})
	if err == nil {
		t.Fatal("RegisterActor accepted a gate against an actor advertising no surface")
	}
	if !strings.Contains(err.Error(), "preflight") {
		t.Errorf("refusal = %q, want it to name the preflight surface", err)
	}

	if _, err := store.RegisterActor(ctx, postgres.RegisterActorParams{
		ActorKey:     "company/gated",
		Kind:         "agent",
		Protocol:     "http",
		Capabilities: json.RawMessage(`{"preflight":{"protocol_version":"1.0","host":{"hostname":"h"}}}`),
		Metadata:     json.RawMessage(`{"preflight_gate":{"enabled":true}}`),
	}); err != nil {
		t.Errorf("RegisterActor refused a properly advertised gate: %v", err)
	}
	// Default-off: an ordinary registration is unaffected.
	if _, err := store.RegisterActor(ctx, postgres.RegisterActorParams{
		ActorKey: "company/plain",
		Kind:     "agent",
		Protocol: "http",
	}); err != nil {
		t.Errorf("RegisterActor refused an ordinary registration: %v", err)
	}
}
