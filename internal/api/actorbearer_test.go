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
	"github.com/agentculture/culture-nodes/internal/auth"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
)

// Task t11 of login-from-anywhere (spec c45 / h31): the merge-gate scripts
// and the developer lane stop carrying the human decision secret and
// authenticate as their OWN registered agent actor. The credential is the
// same one the worker registry already knows how to find — the actor row's
// metadata.auth_token_env names an environment variable, and the control
// plane reads it from its own environment — so no new table and no new
// secret class exist for this. A record posted under that credential is the
// agent's own claim: origin agent, authority proposed.

const (
	mergeGateTokenEnv = "NODES_ACTOR_MERGE_GATE_TOKEN"
	mergeGateToken    = "merge-gate-bearer-for-tests"
)

// insertAgentActorWithTokenEnv inserts an agent actor whose row names the
// environment variable its bearer lives in — the shape
// deploy/prod/register-actor.sh writes for `company/merge-gate`.
func insertAgentActorWithTokenEnv(f *fixture, key, envName string) string {
	f.t.Helper()
	id := store.NewULID()
	metadata, _ := json.Marshal(map[string]string{"auth_token_env": envName})
	_, err := f.store.Pool().Exec(context.Background(),
		`INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref, metadata)
		 VALUES ($1, $2, $3, 1, 'agent', 'http', 'http://127.0.0.1:1', $4::jsonb)`,
		id, f.nsID, key+"-"+id, metadata)
	if err != nil {
		f.t.Fatalf("insert agent actor with token env: %v", err)
	}
	return id
}

func tokenLookup(values map[string]string) apipkg.Option {
	return apipkg.WithActorTokenLookup(func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	})
}

func newAgentBearerFixture(t *testing.T, extra ...apipkg.Option) (*fixture, string) {
	t.Helper()
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret,
		append([]apipkg.Option{tokenLookup(map[string]string{mergeGateTokenEnv: mergeGateToken})}, extra...)...)
	agent := insertAgentActorWithTokenEnv(f, "merge-gate-agent", mergeGateTokenEnv)
	return f, agent
}

// TestSuiteVerdictUnderAnAgentBearerLandsAgentProposed is h31 in one
// request: the verdict is posted with the agent credential, names no
// validator of its own, and the stored record is origin agent / proposed
// attributed to that actor.
func TestSuiteVerdictUnderAnAgentBearerLandsAgentProposed(t *testing.T) {
	f, agent := newAgentBearerFixture(t)
	run, _ := createMinimalRun(t, f)

	req := passingGateReq("")
	var out suiteVerdictOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), mergeGateToken, req, &out)
	requireStatus(t, resp, body, http.StatusCreated)

	if out.Verdict.Origin.Kind != ledger.OriginAgent {
		t.Errorf("origin kind = %q, want %q", out.Verdict.Origin.Kind, ledger.OriginAgent)
	}
	if out.Verdict.Origin.ActorID != agent {
		t.Errorf("origin actor = %q, want the bearer's actor %q", out.Verdict.Origin.ActorID, agent)
	}
	if out.Verdict.Authority != ledger.AuthorityProposed {
		t.Errorf("authority = %q, want %q", out.Verdict.Authority, ledger.AuthorityProposed)
	}
	if out.Verdict.RecordType != ledger.RecordReview {
		t.Errorf("record type = %q, want review (the verdict shape is unchanged)", out.Verdict.RecordType)
	}
	stored, err := f.api.Ledger.Records(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range stored {
		if rec.ID == out.Verdict.ID {
			found = true
			if rec.Origin.Kind != ledger.OriginAgent || rec.Authority != ledger.AuthorityProposed {
				t.Errorf("stored record = %s/%s, want agent/proposed", rec.Origin.Kind, rec.Authority)
			}
		}
	}
	if !found {
		t.Fatalf("verdict %s is not in the run's ledger", out.Verdict.ID)
	}
}

// A body that names some other validator does not get to pick the record's
// origin: the credential decides, and the override is stated in the reply.
func TestSuiteVerdictUnderAnAgentBearerOverridesTheNamedValidator(t *testing.T) {
	f, agent := newAgentBearerFixture(t)
	run, _ := createMinimalRun(t, f)
	other := f.insertActorKind("some-validator", "validator")

	var out map[string]any
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), mergeGateToken, passingGateReq(other), &out)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	warning, _ := out["warning"].(string)
	if !strings.Contains(warning, other) || !strings.Contains(warning, agent) {
		t.Errorf("warning = %q, want it to name the supplied validator and the authenticated actor", warning)
	}
	verdict, _ := out["verdict"].(map[string]any)
	origin, _ := verdict["origin"].(map[string]any)
	if origin["actor_id"] != agent {
		t.Errorf("origin actor = %v, want %s", origin["actor_id"], agent)
	}
}

// The decision bearer keeps working exactly as before for the human
// operator: validator origin, derived authority.
func TestSuiteVerdictUnderTheDecisionBearerStaysDerived(t *testing.T) {
	f, _ := newAgentBearerFixture(t)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")

	var out suiteVerdictOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), decisionAuthSecret, passingGateReq(validator), &out)
	requireStatus(t, resp, body, http.StatusCreated)
	if out.Verdict.Origin.Kind != ledger.OriginValidator || out.Verdict.Authority != ledger.AuthorityDerived {
		t.Errorf("record = %s/%s, want validator/derived", out.Verdict.Origin.Kind, out.Verdict.Authority)
	}
}

// A bearer that matches no actor's configured variable — including an actor
// whose row names a variable the control plane does not carry — is refused,
// not treated as anonymous.
func TestSuiteVerdictRefusesABearerNoActorHolds(t *testing.T) {
	f, _ := newAgentBearerFixture(t)
	run, _ := createMinimalRun(t, f)
	insertAgentActorWithTokenEnv(f, "unconfigured-agent", "NODES_ACTOR_UNCONFIGURED_TOKEN")

	for _, bearer := range []string{"not-anyone's-token", ""} {
		var out map[string]any
		resp, body := doJSONBearer(t, f.client, http.MethodPost,
			f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), bearer, passingGateReq(""), &out)
		requireStatus(t, resp, body, http.StatusUnauthorized)
	}
}

// TestGateReportUnderAnAgentBearerLandsEveryRecordAgentProposed covers the
// gate-report half: per-gate rows AND the aggregate carry the agent's own
// origin and proposed authority, and the counts are still the control
// plane's.
func TestGateReportUnderAnAgentBearerLandsEveryRecordAgentProposed(t *testing.T) {
	f, agent := newAgentBearerFixture(t)
	run, _ := createMinimalRun(t, f)

	req := gateReportReq{
		CommitSHA: gateCommit,
		Gates: []gateEntryReq{
			{Gate: "go-test", Suite: "go test ./...", ExitCode: exitCode(0)},
			{Gate: "webglass", NotApplicable: &gateNAReq{Reason: "instrument_not_reaching_tree", UncoveredPaths: []string{"web/"}}},
		},
	}
	var out gateReportOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/gate-reports"), mergeGateToken, req, &out)
	requireStatus(t, resp, body, http.StatusCreated)

	for i, rec := range append(out.Gates, out.Aggregate) {
		if rec.Origin.Kind != ledger.OriginAgent || rec.Authority != ledger.AuthorityProposed {
			t.Errorf("record %d = %s/%s, want agent/proposed", i, rec.Origin.Kind, rec.Authority)
		}
		if rec.Origin.ActorID != agent {
			t.Errorf("record %d origin actor = %q, want %q", i, rec.Origin.ActorID, agent)
		}
	}
	if out.Outcome != "gates_passed" {
		t.Errorf("outcome = %q, want gates_passed (still computed here)", out.Outcome)
	}
}

// The developer lane posts ticket frames with its own credential too; the
// stored posted_by is the credential's actor, whatever the body said.
func TestTicketFrameUnderAnAgentBearerIsPostedByThatActor(t *testing.T) {
	f, agent := newAgentBearerFixture(t)

	var out apipkg.TicketFrameOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/tickets/SCRUM-11/frame"), mergeGateToken,
		map[string]any{"frame": map[string]any{"state": "ready"}, "posted_by": "actor://company/developer"}, &out)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var stored string
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT posted_by FROM ticket_frames WHERE namespace_id=$1 AND ticket_id='SCRUM-11'`, f.nsID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != agent {
		t.Errorf("posted_by = %q, want the bearer's actor %q", stored, agent)
	}
}

// With the principal gate on (the Access-era listener), an agent credential
// still opens the routes an agent may write and nothing else: a human
// decision under it is refused outright, never admitted as a synthetic admin.
func TestAgentBearerUnderThePrincipalGateWritesOnlyWhereAgentsMay(t *testing.T) {
	verifier := apipkg.WithPrincipalVerifier(verifierFunc(func(context.Context, string) (auth.Principal, error) {
		return auth.Principal{}, &auth.VerificationError{Reason: "malformed"}
	}))
	f, agent := newAgentBearerFixture(t, verifier)
	// The run is minted through an ungated server over the same namespace:
	// publishing a workflow is a protected route, and this test is about the
	// agent bearer, not about who may publish.
	ungated, err := apipkg.NewServer(f.store, f.nsID, apipkg.WithPollInterval(30*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ungatedServer := httptest.NewServer(ungated.Handler())
	t.Cleanup(ungatedServer.Close)
	run, _ := createMinimalRun(t, &fixture{t: t, server: ungatedServer, api: ungated, store: f.store, nsID: f.nsID, client: ungatedServer.Client()})

	var out suiteVerdictOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), mergeGateToken, passingGateReq(""), &out)
	requireStatus(t, resp, body, http.StatusCreated)
	if out.Verdict.Origin.ActorID != agent || out.Verdict.Authority != ledger.AuthorityProposed {
		t.Errorf("verdict = %+v / %s, want the agent's proposed record", out.Verdict.Origin, out.Verdict.Authority)
	}

	var frame apipkg.TicketFrameOut
	resp, body = doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/tickets/SCRUM-12/frame"), mergeGateToken,
		map[string]any{"frame": map[string]any{"state": "ready"}}, &frame)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("frame status = %d: %s", resp.StatusCode, body)
	}

	// On a human decision route the agent credential is not even resolved:
	// the request has no principal at all, rather than a principal that
	// lacks a role.
	var refused map[string]any
	resp, body = doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/human-tasks/ht_x/decision"), mergeGateToken,
		map[string]any{"decision": "approve"}, &refused)
	requireStatus(t, resp, body, http.StatusUnauthorized)
	if refused["reason"] != "no_principal" {
		t.Errorf("reason = %v, want no_principal", refused["reason"])
	}
}
