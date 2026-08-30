package actors_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

func newAsyncFixtureWithWorkflow(t *testing.T, withActor bool, workflow string) *asyncFixture {
	t.Helper()
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()

	ns := pgtest.MustNamespace(t, s, "actors")
	eng, err := storepg.NewEngine(s, ns.ID, engine.WithRetryDelays(0, 0))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	callbacks, err := storepg.NewCallbackStore(s, ns.ID)
	if err != nil {
		t.Fatalf("NewCallbackStore: %v", err)
	}
	signer, err := actors.NewTokenSigner([]byte(testSecret))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}

	f := &asyncFixture{
		t:         t,
		ctx:       ctx,
		store:     s,
		ns:        ns,
		engine:    eng,
		callbacks: callbacks,
		signer:    signer,
		deps: actors.CallbackDeps{
			Store:  callbacks,
			Engine: eng,
			Signer: signer,
		},
		cw:       compileFixture(t, workflow),
		workerID: "worker-" + t.Name(),
	}
	if withActor {
		f.actorID = mustRegisterActor(t, s, ns.ID)
	}

	run, err := eng.CreateRun(ctx, f.cw, json.RawMessage(`{"subject":"async"}`))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	f.run = run
	f.nodeRunID = f.readyNodeRun(run.ID)
	f.claimed = f.claim(f.workerID, f.nodeRunID)
	f.attemptID = "att_" + f.claimed.ID
	f.park()
	return f
}

func TestCallbackRejectsDeltaFromAnotherActorWithoutRedispatch(t *testing.T) {
	f := newAsyncFixtureWithWorkflow(t, true, "async-retries.workflow.yaml")
	otherActorID := mustRegisterActor(t, f.store, f.ns.ID)

	payload, _ := json.Marshal(actors.CompletedPayload{
		Outcome: "completed",
		Output:  json.RawMessage(`{"summary":"done"}`),
		LedgerDelta: &actors.LedgerDelta{Records: []ledger.Record{{
			RecordType: ledger.RecordClaim,
			Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: otherActorID},
			Data:       json.RawMessage(`{"statement":"foreign identity"}`),
		}}},
	})
	result := f.handle(actors.CallbackEvent{EventID: "ev-identity-mismatch", Sequence: 1, Kind: actors.EventCompleted, Payload: payload})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed refusal", result.Disposition, result.Diagnostic)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want exactly the dispatched attempt (no redispatch)", len(attempts))
	}
	if attempts[0].Status != engine.StatusContractRejected {
		t.Errorf("attempt status = %q, want contract_rejected", attempts[0].Status)
	}
	wantDetail := fmt.Sprintf("origin_actor_id %s is not the dispatched actor %s", otherActorID, f.actorID)
	var output struct {
		Error struct {
			Class  string `json:"class"`
			Detail string `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal(attempts[0].Result, &output); err != nil {
		t.Fatalf("decode attempt result %s: %v", attempts[0].Result, err)
	}
	if output.Error.Class != "identity" || output.Error.Detail != wantDetail {
		t.Errorf("result.error = %+v, want class identity and detail %q", output.Error, wantDetail)
	}
	var dispatchCount int
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT attempt FROM work_items WHERE node_run_id = $1 ORDER BY created_at DESC LIMIT 1`, f.nodeRunID).Scan(&dispatchCount); err != nil {
		t.Fatalf("read dispatch count: %v", err)
	}
	if dispatchCount != int(f.claimed.Attempt) {
		t.Errorf("dispatch count = %d, want unchanged %d", dispatchCount, f.claimed.Attempt)
	}
}

func TestCallbackAcceptsDeltaFromDispatchedActor(t *testing.T) {
	f := newAsyncFixtureForActor(t)
	payload, _ := json.Marshal(actors.CompletedPayload{
		Outcome: "completed",
		Output:  json.RawMessage(`{"summary":"done"}`),
		LedgerDelta: &actors.LedgerDelta{Records: []ledger.Record{{
			RecordType: ledger.RecordClaim,
			Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: f.actorID},
			Data:       json.RawMessage(`{"statement":"matching identity"}`),
		}}},
	})
	result := f.handle(actors.CallbackEvent{EventID: "ev-identity-match", Sequence: 1, Kind: actors.EventCompleted, Payload: payload})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}
	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != engine.StatusSucceeded {
		t.Fatalf("attempts = %+v, want one succeeded attempt", attempts)
	}
}

// A bridge reports the identity row it was issued; dispatch names the
// registration row. Two rows, one actor_key: the delta is the dispatched
// actor's own and must be accepted (prod regression on SCRUM-7's intake run
// 01M19PG0QGN6QA77Q6TNMSQQKJ, 2026-08-30).
func TestCallbackAcceptsDeltaFromAnotherRevisionOfTheDispatchedActor(t *testing.T) {
	f := newAsyncFixtureForActor(t)
	var key string
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT actor_key FROM actors WHERE id = $1`, f.actorID).Scan(&key); err != nil {
		t.Fatalf("actor_key of dispatched actor: %v", err)
	}
	sameActorOtherRow := "actor-identity-" + f.attemptID
	if _, err := f.store.Pool().Exec(f.ctx, `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		VALUES ($1, $2, $3, 2, 'agent', 'internal')
	`, sameActorOtherRow, f.ns.ID, key); err != nil {
		t.Fatalf("register second row: %v", err)
	}
	payload, _ := json.Marshal(actors.CompletedPayload{
		Outcome: "completed",
		Output:  json.RawMessage(`{"summary":"done"}`),
		LedgerDelta: &actors.LedgerDelta{Records: []ledger.Record{{
			RecordType: ledger.RecordClaim,
			Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: sameActorOtherRow},
			Data:       json.RawMessage(`{"statement":"same actor, other row"}`),
		}}},
	})
	result := f.handle(actors.CallbackEvent{EventID: "ev-identity-same-key", Sequence: 1, Kind: actors.EventCompleted, Payload: payload})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}
	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != engine.StatusSucceeded {
		t.Fatalf("attempts = %+v, want one succeeded attempt", attempts)
	}
}
