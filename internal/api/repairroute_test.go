package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/repair"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// suiteVerdictOut mirrors components.schemas.SuiteVerdictResult — the wire
// shape a gate reads, encoded here rather than reached for from internal/api
// (see api_test.go's package doc).
type suiteVerdictOut struct {
	Verdict      ledger.Record  `json:"verdict"`
	Routing      *ledger.Record `json:"routing"`
	RoutingError string         `json:"routing_error"`
}

// thorCapabilities is the capability surface a codex bridge on thor really
// advertises, trimmed to the keys a routing decision reads. Its values are
// what docs/baselines/dispatch-posture.md measured there: `go` runs under
// workspace-write, and nothing on that host has network egress.
const thorCapabilities = `{"preflight":{"protocol_version":"1.0","host":{
  "hostname":"thor",
  "commit_policy":{"commits":true},
  "default_sandbox_mode":"workspace-write",
  "dispatch_grants":{"workspace-write":["workspace-write","tmp-write"]},
  "toolchains":[{"name":"go","state":"present","usable_in":["workspace-write"],
                 "unusable_in":{"read-only":"nothing is writable in this mode"}}]}}}`

// newRoutingFixture is a fixture whose repair-router identity is registered.
//
// Registering it is not test scaffolding around a gap — it is the deployment
// obligation the option's own doc states, and
// TestAnUnrecordableRoutingIsReportedRatherThanDropped is the test for what
// happens when a deployment skips it.
func newRoutingFixture(t *testing.T) *fixture {
	t.Helper()
	s := requireStore(t)
	nsID := pgtest.MustNamespace(t, s, "api").ID

	// The router identity is registered BEFORE the server that will write
	// under it, which is the order a deployment does it in
	// (register-actor.sh, then deploy). It gets a generated id rather than
	// the literal DefaultRepairRouterActorID because this package's tests
	// share one PostgreSQL and actors.id is a global primary key.
	routerID := store.NewULID()
	if _, err := s.Pool().Exec(context.Background(),
		`INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		 VALUES ($1, $2, $3, 1, 'validator', 'http')`,
		routerID, nsID, "gate/repair-router-"+routerID); err != nil {
		t.Fatalf("register the repair router identity: %v", err)
	}

	srv, err := apipkg.NewServer(s, nsID,
		apipkg.WithPollInterval(30*time.Millisecond),
		apipkg.WithDecisionAuthSecret(decisionAuthSecret),
		apipkg.WithRepairRouterActorID(routerID),
	)
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &fixture{t: t, server: ts, api: srv, store: s, nsID: nsID, client: ts.Client()}
}

// insertActorWithCapabilities registers an agent actor advertising a
// capability surface, which is what makes it resolvable as a repair lane.
func insertActorWithCapabilities(t *testing.T, f *fixture, key, capabilities string) string {
	t.Helper()
	id := f.insertActor(key)
	_, err := f.store.Pool().Exec(context.Background(),
		`UPDATE actors SET capabilities = $2 WHERE id = $1`, id, capabilities)
	if err != nil {
		t.Fatalf("set capabilities: %v", err)
	}
	return id
}

// seedAgentClaim appends the proposed claim a finished session leaves behind.
// It is how the control plane knows WHICH actor's workspace a gate failure is
// in, and therefore which lane a repair would go to.
func seedAgentClaim(t *testing.T, f *fixture, runID, actorID string) {
	t.Helper()
	_, err := f.api.Ledger.Append(context.Background(), ledger.Record{
		RecordType: ledger.RecordClaim,
		RunID:      runID,
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: actorID},
		Authority:  ledger.AuthorityProposed,
		Data:       json.RawMessage(`{"statement":"implemented the package"}`),
	})
	if err != nil {
		t.Fatalf("append agent claim: %v", err)
	}
}

func rejectingGateReq(validator string) createSuiteVerdictReq {
	req := passingGateReq(validator)
	req.ExitCode = exitCode(1)
	return req
}

func postVerdict(t *testing.T, f *fixture, runID string, req createSuiteVerdictReq) suiteVerdictOut {
	t.Helper()
	var out suiteVerdictOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+runID+"/suite-verdicts"), decisionAuthSecret, req, &out)
	requireStatus(t, resp, body, http.StatusCreated)
	return out
}

func routingPayload(t *testing.T, rec *ledger.Record) map[string]any {
	t.Helper()
	if rec == nil {
		t.Fatal("no routing record was returned")
	}
	var data map[string]any
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		t.Fatalf("decode routing payload: %v", err)
	}
	return data
}

// The headline (issue #102): a failing gate stops landing in the operator's
// session and becomes a routed, bounded, recorded decision.
func TestARejectingGateRoutesToABoundedRepair(t *testing.T) {
	f := newRoutingFixture(t)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")
	lane := insertActorWithCapabilities(t, f, "codex-thor", thorCapabilities)
	seedAgentClaim(t, f, run.ID, lane)

	out := postVerdict(t, f, run.ID, rejectingGateReq(validator))

	if out.RoutingError != "" {
		t.Fatalf("routing_error = %q, want none", out.RoutingError)
	}
	data := routingPayload(t, out.Routing)
	if data["selected"] != string(repair.DestinationRepair) {
		t.Fatalf("selected = %v, want %q (%v)", data["selected"], repair.DestinationRepair, data["rationale"])
	}
	if data["attempt_number"] != float64(1) {
		t.Fatalf("attempt_number = %v, want 1", data["attempt_number"])
	}
	if data["repair_lane_actor_id"] != lane {
		t.Fatalf("repair_lane_actor_id = %v, want the actor that did the work (%s)", data["repair_lane_actor_id"], lane)
	}
	bound, _ := data["bound"].(map[string]any)
	if bound["max_attempts"] != float64(repair.MaxAttempts) || bound["at_ceiling"] != repair.AtCeiling {
		t.Fatalf("bound = %v, want the stated ceiling and behaviour", bound)
	}
	if out.Routing.Authority != ledger.AuthorityDerived || out.Routing.Origin.Kind != ledger.OriginValidator {
		t.Fatalf("routing record = %s/%s, want derived from a validator",
			out.Routing.Authority, out.Routing.Origin.Kind)
	}
	if out.Routing.SubjectRef.String() != out.Verdict.ID {
		t.Fatalf("routing subject = %q, want the verdict %q", out.Routing.SubjectRef, out.Verdict.ID)
	}
}

// A passing gate routes nowhere and writes no routing record. A ledger row
// per green gate saying "nothing to do" is noise that makes the rows that
// mean something harder to find.
func TestAPassingGateWritesNoRouting(t *testing.T) {
	f := newRoutingFixture(t)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")
	lane := insertActorWithCapabilities(t, f, "codex-thor", thorCapabilities)
	seedAgentClaim(t, f, run.ID, lane)

	out := postVerdict(t, f, run.ID, passingGateReq(validator))

	if out.Routing != nil {
		t.Fatalf("a passing gate produced a routing record: %+v", out.Routing)
	}
	if out.Verdict.ID == "" {
		t.Fatal("the verdict itself was not returned")
	}
}

// The ceiling, through the real API and the real ledger: three rejecting
// gates, and the third one stops.
func TestTheRepairCeilingIsReachedThroughTheLedger(t *testing.T) {
	f := newRoutingFixture(t)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")
	lane := insertActorWithCapabilities(t, f, "codex-thor", thorCapabilities)
	seedAgentClaim(t, f, run.ID, lane)

	for round := 1; round <= repair.MaxAttempts; round++ {
		data := routingPayload(t, postVerdict(t, f, run.ID, rejectingGateReq(validator)).Routing)
		if data["selected"] != string(repair.DestinationRepair) {
			t.Fatalf("round %d: selected = %v, want a repair (%v)", round, data["selected"], data["rationale"])
		}
		if data["attempt_number"] != float64(round) {
			t.Fatalf("round %d: attempt_number = %v", round, data["attempt_number"])
		}
	}

	data := routingPayload(t, postVerdict(t, f, run.ID, rejectingGateReq(validator)).Routing)
	if data["selected"] != string(repair.DestinationHuman) {
		t.Fatalf("past the ceiling: selected = %v, want %q", data["selected"], repair.DestinationHuman)
	}
	if data["reason"] != string(repair.ReasonCeilingReached) {
		t.Fatalf("past the ceiling: reason = %v, want %q", data["reason"], repair.ReasonCeilingReached)
	}
	rationale, _ := data["rationale"].(string)
	if !strings.Contains(rationale, "2 of 2") {
		t.Fatalf("the ceiling rationale does not state the bound: %q", rationale)
	}
}

// The workflow-scope boundary, end to end: the control plane MEASURED that
// this run's handover commit touched CI configuration, so the failure goes to
// a person rather than at a dispatch that would be refused for touching it.
func TestAGateFailureOnACommitTouchingCIConfigurationGoesToAHuman(t *testing.T) {
	f := newRoutingFixture(t)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")
	lane := insertActorWithCapabilities(t, f, "codex-thor", thorCapabilities)
	seedAgentClaim(t, f, run.ID, lane)
	seedHandoverEvidencePaths(t, f, run.ID, gateCommit,
		[]string{"internal/api/server.go", ".github/workflows/tests.yml"})

	data := routingPayload(t, postVerdict(t, f, run.ID, rejectingGateReq(validator)).Routing)

	if data["selected"] != string(repair.DestinationHuman) {
		t.Fatalf("selected = %v, want %q", data["selected"], repair.DestinationHuman)
	}
	if data["reason"] != string(repair.ReasonOutOfWorkflowScope) {
		t.Fatalf("reason = %v, want %q", data["reason"], repair.ReasonOutOfWorkflowScope)
	}
	guarded, _ := data["guarded_paths"].([]any)
	if len(guarded) != 1 || guarded[0] != ".github/workflows/tests.yml" {
		t.Fatalf("guarded_paths = %v, want the one CI path named", guarded)
	}
	// And no repair attempt was spent on a route that was never legal.
	if data["attempt_number"] != float64(0) {
		t.Fatalf("attempt_number = %v, want 0", data["attempt_number"])
	}
}

// The recorded risk, end to end: a lane whose advertised surface does not
// show it can run the failing suite is not sent a repair.
func TestALaneThatCannotRunTheSuiteRoutesToAHuman(t *testing.T) {
	f := newRoutingFixture(t)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")
	lane := insertActorWithCapabilities(t, f, "codex-thor", thorCapabilities)
	seedAgentClaim(t, f, run.ID, lane)

	req := rejectingGateReq(validator)
	req.Suite = "uv run pytest -n auto"
	req.Command = []string{"uv", "run", "pytest", "-n", "auto"}

	data := routingPayload(t, postVerdict(t, f, run.ID, req).Routing)

	if data["reason"] != string(repair.ReasonLaneCannotVerify) {
		t.Fatalf("reason = %v, want %q (%v)", data["reason"], repair.ReasonLaneCannotVerify, data["rationale"])
	}
}

// #119, end to end: the gate declares that its suite needs the network, and
// the lane's posture grants none.
func TestASuiteNeedingAGrantTheLaneLacksRoutesToAHuman(t *testing.T) {
	f := newRoutingFixture(t)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")
	lane := insertActorWithCapabilities(t, f, "codex-thor", thorCapabilities)
	seedAgentClaim(t, f, run.ID, lane)

	req := rejectingGateReq(validator)
	req.RequiresGrants = []string{"network-egress"}

	data := routingPayload(t, postVerdict(t, f, run.ID, req).Routing)

	if data["reason"] != string(repair.ReasonLaneCannotVerify) {
		t.Fatalf("reason = %v, want %q", data["reason"], repair.ReasonLaneCannotVerify)
	}
	rationale, _ := data["rationale"].(string)
	if !strings.Contains(rationale, "network-egress") {
		t.Fatalf("rationale does not name the missing grant: %q", rationale)
	}
}

// A run whose actor advertised no capability surface fails closed.
func TestALaneWithNoAdvertisedSurfaceRoutesToAHuman(t *testing.T) {
	f := newRoutingFixture(t)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")
	seedAgentClaim(t, f, run.ID, f.insertActor("codex-nosurface"))

	data := routingPayload(t, postVerdict(t, f, run.ID, rejectingGateReq(validator)).Routing)

	if data["reason"] != string(repair.ReasonLaneUnknown) {
		t.Fatalf("reason = %v, want %q", data["reason"], repair.ReasonLaneUnknown)
	}
}

// A run with no agent claim at all has no lane to repair on.
func TestARunWithNoResolvableLaneRoutesToAHuman(t *testing.T) {
	f := newRoutingFixture(t)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")

	data := routingPayload(t, postVerdict(t, f, run.ID, rejectingGateReq(validator)).Routing)

	if data["reason"] != string(repair.ReasonNoLane) {
		t.Fatalf("reason = %v, want %q", data["reason"], repair.ReasonNoLane)
	}
}

// The routing is a real ledger record, readable by anyone reading the run —
// which is what makes "this package needed three repair rounds" a query
// rather than a memory (#28).
func TestTheRoutingIsReadableFromTheRunsLedger(t *testing.T) {
	f := newRoutingFixture(t)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")
	lane := insertActorWithCapabilities(t, f, "codex-thor", thorCapabilities)
	seedAgentClaim(t, f, run.ID, lane)

	out := postVerdict(t, f, run.ID, rejectingGateReq(validator))

	var records struct {
		Items []ledger.Record `json:"items"`
	}
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID+"/ledger"), nil, &records)
	requireStatus(t, resp, body, http.StatusOK)

	found := false
	for _, rec := range records.Items {
		if rec.ID == out.Routing.ID {
			found = true
			if rec.RecordType != ledger.RecordDecision {
				t.Errorf("record type = %q, want %q", rec.RecordType, ledger.RecordDecision)
			}
		}
	}
	if !found {
		t.Fatalf("the routing record %s is not in the run's ledger", out.Routing.ID)
	}
}

// The routing is a decision, never an execution. A reader who mistakes one
// for the other stops looking for the dispatch that never happened — which is
// #17's shape, one layer up.
func TestTheRoutingSaysItDispatchedNothing(t *testing.T) {
	f := newRoutingFixture(t)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")
	lane := insertActorWithCapabilities(t, f, "codex-thor", thorCapabilities)
	seedAgentClaim(t, f, run.ID, lane)

	data := routingPayload(t, postVerdict(t, f, run.ID, rejectingGateReq(validator)).Routing)

	if data["dispatched"] != false {
		t.Fatalf("dispatched = %v, want false", data["dispatched"])
	}
	note, _ := data["dispatch_note"].(string)
	if !strings.Contains(note, "did not dispatch") {
		t.Fatalf("dispatch_note = %q, want it to say plainly that nothing was dispatched", note)
	}
}

// A routing that cannot be appended must be REPORTED, not swallowed. Issue
// #120 is exactly the cost of a silent absence: a stale bridge and an honest
// refusal produced byte-identical evidence, and it took an ssh to notice.
func TestAnUnrecordableRoutingIsReportedRatherThanDropped(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret,
		apipkg.WithRepairRouterActorID("act_never_registered"))
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")
	lane := insertActorWithCapabilities(t, f, "codex-thor", thorCapabilities)
	seedAgentClaim(t, f, run.ID, lane)

	out := postVerdict(t, f, run.ID, rejectingGateReq(validator))

	if out.Verdict.ID == "" {
		t.Fatal("the verdict was lost along with the routing; the verdict is the primary record")
	}
	if out.Routing != nil {
		t.Fatalf("a routing was returned despite being unappendable: %+v", out.Routing)
	}
	if !strings.Contains(out.RoutingError, "act_never_registered") {
		t.Fatalf("routing_error = %q, want the unregistered router identity named", out.RoutingError)
	}
}

// seedHandoverEvidencePaths is seedHandoverEvidence with the measured path
// list under the test's control, so the scope boundary can be exercised
// against a real observed record rather than a hand-built payload.
func seedHandoverEvidencePaths(t *testing.T, f *fixture, runID, commit string, paths []string) ledger.Record {
	t.Helper()
	runnerActor := f.insertActorKind("handover-fetch", "runner")
	obs := &handover.Observer{ActorID: runnerActor}
	rec, manifest, err := obs.BuildRecord(
		handover.Claim{RunID: runID, Ref: gateHandoverR},
		handover.Measurement{
			Ref:          gateHandoverR,
			CommitSHA:    commit,
			ChangedPaths: paths,
			Source:       "ssh://example.invalid/repo",
		})
	if err != nil {
		t.Fatalf("BuildRecord: %v", err)
	}
	appended, err := f.api.Ledger.Append(context.Background(), rec, ledger.WithRunnerManifest(manifest))
	if err != nil {
		t.Fatalf("append handover evidence: %v", err)
	}
	return appended
}
