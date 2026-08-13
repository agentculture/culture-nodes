package worker_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/runners"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Actor attribution on FAILURE completions (task t2, issue #40, spec claim
// c2 / honesty condition h2). The success paths have stamped
// attempts.actor_id since the attribution resolver landed; these tests pin
// the failure paths to the same rule:
//
//   - a dispatch that failed AFTER the actor reference resolved is
//     attributed to the resolved actors-table row id — a failed, billable
//     dispatch is exactly the one the retry-burn measure must not lose;
//   - a refusal that fired BEFORE resolution stays NULL, even when the
//     registry could have answered ActorRowID — attribution is recorded for
//     what was resolved, never guessed for what was not;
//   - a code-path failure carries the code-runner actor id exactly like the
//     code path's success completion does.

// attributingRegistry is StaticRegistry plus the ActorRowID capability,
// answering with a fixed actors-table row id — what DBRegistry does against
// real rows, without needing endpoint_ref to point at the test server.
type attributingRegistry struct {
	worker.StaticRegistry
	rowIDs map[string]string
}

func (r attributingRegistry) ActorRowID(_ context.Context, ref string) (string, error) {
	if id, ok := r.rowIDs[ref]; ok {
		return id, nil
	}
	// Like DBRegistry, answer by identity when the reference pins a
	// revision digest: the digest pins the revision, not who the actor is.
	if key, _, ok := strings.Cut(ref, "@"); ok {
		if id, found := r.rowIDs[key]; found {
			return id, nil
		}
	}
	return "", fmt.Errorf("attributingRegistry: no row id for %q: %w", ref, worker.ErrUnknownActor)
}

// unresolvableAttributingRegistry refuses every Resolve while still being
// ABLE to answer ActorRowID — the sharpest possible probe that a
// pre-resolution failure records no attribution it could have guessed.
type unresolvableAttributingRegistry struct{ rowID string }

func (r unresolvableAttributingRegistry) Resolve(context.Context, string) (actors.Endpoint, error) {
	return actors.Endpoint{}, worker.ErrUnknownActor
}

func (r unresolvableAttributingRegistry) ActorRowID(context.Context, string) (string, error) {
	return r.rowID, nil
}

// attemptActorID reads the actor_id column of the single attempt recorded
// against nodeKey; nil means SQL NULL.
func attemptActorID(t *testing.T, h *harness, runID, nodeKey string) *string {
	t.Helper()
	var actorID *string
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.actor_id
		FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = $2
	`, runID, nodeKey).Scan(&actorID); err != nil {
		t.Fatalf("read attempt actor_id for %q: %v", nodeKey, err)
	}
	return actorID
}

// A dispatch that resolved its actor and then failed technically (here a
// §13.5-classified 401 from the actor itself) persists the resolved
// actors-table row id on the failed attempt — and the per-actor retry-burn
// measure counts that attempt, which is the point of attributing failures at
// all: a retried, failed dispatch is billable work the comparison between
// actors must see.
func TestFailedAgentDispatchAttributesResolvedActor(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"workload token rejected"}`))
	})
	actorRowID := mustAgentActor(t, h.store, h.ns.ID)

	wk, err := worker.New(h.store, h.engine, worker.Options{
		WorkerID:          "worker-attrib-" + t.Name(),
		NamespaceID:       h.ns.ID,
		LeaseDuration:     30 * time.Second,
		HeartbeatInterval: 200 * time.Millisecond,
		Registry: attributingRegistry{
			StaticRegistry: worker.StaticRegistry{"actor://company/analyzer": {URL: h.actorServer.URL}},
			rowIDs:         map[string]string{"actor://company/analyzer": actorRowID},
		},
		Signer:          h.signer,
		CallbackBaseURL: h.callbackServer.URL,
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	h.worker = wk

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed", state)
	}

	got := attemptActorID(t, h, run.ID, "analyze")
	if got == nil {
		t.Fatal("failed attempt actor_id is NULL, want the resolved actor row id")
	}
	if *got != actorRowID {
		t.Fatalf("failed attempt actor_id = %q, want %q", *got, actorRowID)
	}

	// The failed attempt shows up in the actor's own retry-burn numerator —
	// the exact store query GET /v1alpha1/actors/{id}/stats serves.
	es, err := storepg.NewEngineStore(h.store, h.ns.ID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	stats, err := es.ActorStats(h.ctx, actorRowID)
	if err != nil {
		t.Fatalf("ActorStats: %v", err)
	}
	if stats.Total.RetryBurn.Attempts != 1 {
		t.Errorf("retry burn attempts = %d, want 1 (the failed dispatch must count)", stats.Total.RetryBurn.Attempts)
	}
}

// A refusal that fires BEFORE the actor reference resolves — here Resolve
// itself refusing — persists NULL actor_id even though the registry could
// have answered ActorRowID for the same reference. An unresolved dispatch is
// unattributed; guessing would charge an actor for work it was never sent.
func TestPreResolutionRefusalPersistsNullActorID(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		t.Error("the actor was invoked; an unresolvable reference must not dispatch")
		w.WriteHeader(http.StatusInternalServerError)
	})
	actorRowID := mustAgentActor(t, h.store, h.ns.ID)

	wk, err := worker.New(h.store, h.engine, worker.Options{
		WorkerID:          "worker-noattrib-" + t.Name(),
		NamespaceID:       h.ns.ID,
		LeaseDuration:     30 * time.Second,
		HeartbeatInterval: 200 * time.Millisecond,
		Registry:          unresolvableAttributingRegistry{rowID: actorRowID},
		Signer:            h.signer,
		CallbackBaseURL:   h.callbackServer.URL,
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	h.worker = wk

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed", state)
	}
	var status string
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.status FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'analyze'
	`, run.ID).Scan(&status); err != nil {
		t.Fatalf("read attempt status: %v", err)
	}
	if engine.TechStatus(status) != engine.StatusPolicyDenied {
		t.Errorf("attempt status = %q, want policy_denied for an unresolvable reference", status)
	}
	if got := attemptActorID(t, h, run.ID, "analyze"); got != nil {
		t.Fatalf("pre-resolution refusal persisted actor_id %q, want NULL — attribution must not be guessed", *got)
	}
}

// A code-path failure completion — the runner boundary refusing the dispatch
// outright — carries the code-runner actor id exactly like the code path's
// success completion (dispatchCode's succeeded branch) already does.
func TestCodeDispatchRefusalCarriesCodeRunnerActorID(t *testing.T) {
	h := newCodeHarness(t, func(op runners.Operation, _ int) (runners.Result, error) {
		return runners.Result{}, &runners.DispatchError{
			Kind:        runners.ErrorAuthOrPolicy,
			OperationID: op.OperationID,
			Detail:      "the runner registry holds no such identity",
			Err:         runners.ErrUnregisteredFunction,
		}
	})

	run := h.createRun("code.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed", state)
	}
	got := attemptActorID(t, h.harness, run.ID, "test")
	if got == nil {
		t.Fatal("refused code dispatch persisted NULL actor_id, want the code-runner actor id")
	}
	if *got != h.runnerID {
		t.Fatalf("refused code dispatch actor_id = %q, want the code-runner actor id %q", *got, h.runnerID)
	}
}
