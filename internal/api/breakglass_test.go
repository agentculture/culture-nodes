package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/auth"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	pgtest "github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// Task t22 of login-from-anywhere (deviation d2; spec claims c20 and c48,
// honesty conditions h11 and h34): the LAN break-glass.
//
// Task t13 wrote the operator recipe and found the gap it could not close
// from a document (docs/operations/people.md): an issued `cnd_` dial-in
// credential was verified in exactly one place — authenticateInbound, for
// the two machine surfaces — so minting one for the operator's human actor
// produced a live secret that opened nothing, and a decision carrying it
// answered `401 no_principal`. The only LAN break-glass left was the shared
// `NODES_HUMAN_DECISION_TOKEN_SECRET`, which h25 removes from every hand.
//
// These tests are that gap closed, from the outside: the credential is
// minted through the shipped issuance path, presented by a plain HTTP client
// on the LAN listener with no Access header and no decision secret
// configured at all, and the record it lands names the operator's actor
// because the CREDENTIAL said so — no `decider_actor_id` is typed anywhere.

// breakGlassFixtures builds the pair every test here needs: a gated LAN
// listener (the principal middleware active, its verifier refusing every
// assertion — a misconfigured Access front is exactly the situation the
// break-glass exists for) and an ungated server over the SAME namespace
// used only to publish the workflow and drive the run to its human task,
// which are protected routes this task is not about.
func breakGlassFixtures(t *testing.T, extra ...apipkg.Option) (gated, ungated *fixture) {
	t.Helper()
	s := requireStore(t)
	nsID := pgtest.MustNamespace(t, s, "break-glass").ID

	options := append([]apipkg.Option{
		apipkg.WithPollInterval(30 * time.Millisecond),
		apipkg.WithPrincipalVerifier(verifierFunc(func(context.Context, string) (auth.Principal, error) {
			return auth.Principal{}, &auth.VerificationError{Reason: "malformed"}
		})),
	}, extra...)
	srv, err := apipkg.NewServer(s, nsID, options...)
	if err != nil {
		t.Fatalf("api.NewServer (gated): %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	open, err := apipkg.NewServer(s, nsID, apipkg.WithPollInterval(30*time.Millisecond))
	if err != nil {
		t.Fatalf("api.NewServer (ungated): %v", err)
	}
	openServer := httptest.NewServer(open.Handler())
	t.Cleanup(openServer.Close)

	return &fixture{t: t, server: ts, api: srv, store: s, nsID: nsID, client: ts.Client()},
		&fixture{t: t, server: openServer, api: open, store: s, nsID: nsID, client: openServer.Client()}
}

// insertActorRevision inserts one revision of an actor under an actor-key
// shaped party key (namespace/name — migration 0031's own shape), which is
// what a dial-in credential can be issued for.
func insertActorRevision(t *testing.T, s *storepg.Store, nsID, key, kind string, revision int) string {
	t.Helper()
	id := store.NewULID()
	if _, err := s.Pool().Exec(context.Background(),
		`INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		 VALUES ($1, $2, $3, $4, $5, 'http')`, id, nsID, key, revision, kind); err != nil {
		t.Fatalf("insert %s actor revision %d: %v", kind, revision, err)
	}
	return id
}

// issueBreakGlassCredential mints one through the shipped issuance path —
// crypto/rand value, digest at rest, revealed once — and returns the
// plaintext the operator would keep on the host.
func issueBreakGlassCredential(t *testing.T, s *storepg.Store, partyKey string) string {
	t.Helper()
	secret, issued, err := actors.MintInboundCredential("actor", partyKey)
	if err != nil {
		t.Fatalf("MintInboundCredential: %v", err)
	}
	if _, err := s.IssueInboundCredential(context.Background(), issued); err != nil {
		t.Fatalf("IssueInboundCredential: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool().Exec(context.Background(),
			`DELETE FROM inbound_authentication WHERE party_kind='actor' AND party_key=$1`, partyKey)
	})
	plaintext, err := secret.Reveal()
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	return plaintext
}

// TestBreakGlassCredentialDecidesAHumanTaskFromTheLAN is c48's break-glass
// in one request: a credential issued for the operator's HUMAN actor, on the
// LAN listener, with no Access assertion and no decision secret configured,
// decides a human task — and the ledger names the actor the credential is
// bound to, at its newest revision, with nothing in the body claiming it.
func TestBreakGlassCredentialDecidesAHumanTaskFromTheLAN(t *testing.T) {
	gated, open := breakGlassFixtures(t)
	partyKey := "company/operator-" + strings.ToLower(store.NewULID())
	insertActorRevision(t, gated.store, gated.nsID, partyKey, "human", 1)
	newest := insertActorRevision(t, gated.store, gated.nsID, partyKey, "human", 2)
	credential := issueBreakGlassCredential(t, gated.store, partyKey)

	run, task := advanceToReview(t, open)

	var result apipkg.HumanTaskDecisionResultOut
	resp, body := authedDecide(t, gated, task.ID, credential, decideHumanTaskReq{
		Outcome:               "approved",
		ExpectedLedgerVersion: 0,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("decide under the break-glass credential: status = %d, body = %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode decision result: %v (%s)", err, body)
	}

	records, err := gated.api.Ledger.Records(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var decision *ledger.Record
	for i := range records {
		if records[i].RecordType == ledger.RecordDecision {
			decision = &records[i]
		}
	}
	if decision == nil {
		t.Fatalf("no decision record in the run's ledger: %+v", records)
	}
	if decision.Origin.ActorID != newest {
		t.Errorf("decision origin actor = %q, want the credential's newest actor revision %q",
			decision.Origin.ActorID, newest)
	}
	if decision.Origin.Kind != ledger.OriginHuman {
		t.Errorf("decision origin kind = %q, want human", decision.Origin.Kind)
	}
}

// The same credential is legible before an incident: whoami on the LAN
// listener names the party it is bound to and the role it carries, so the
// operator can prove the break-glass works without deciding anything.
func TestBreakGlassCredentialIsVisibleOnWhoami(t *testing.T) {
	gated, _ := breakGlassFixtures(t)
	partyKey := "company/operator-" + strings.ToLower(store.NewULID())
	actorID := insertActorRevision(t, gated.store, gated.nsID, partyKey, "human", 1)
	credential := issueBreakGlassCredential(t, gated.store, partyKey)

	var out map[string]any
	resp, body := doJSONBearer(t, gated.client, http.MethodGet, gated.url("/v1alpha1/whoami"), credential, nil, &out)
	requireStatus(t, resp, body, http.StatusOK)
	if out["actor_id"] != actorID {
		t.Errorf("whoami actor_id = %v, want %q", out["actor_id"], actorID)
	}
	principal, _ := out["principal"].(map[string]any)
	if principal["subject"] != partyKey {
		t.Errorf("whoami subject = %v, want the party key %q", principal["subject"], partyKey)
	}
	roles, _ := out["roles"].([]any)
	if len(roles) != 1 || roles[0] != string(auth.RoleApprover) {
		t.Errorf("whoami roles = %v, want [approver]", roles)
	}
}

// TestRevokedBreakGlassCredentialIsRefusedWithItsOwnReasonClass covers the
// offboarding half and c48's logging clause: revocation is the one action
// that retires the credential, and the refusal it produces is classified as
// what it is — not as "no principal", which would read as a broken site.
func TestRevokedBreakGlassCredentialIsRefusedWithItsOwnReasonClass(t *testing.T) {
	var logs bytes.Buffer
	gated, open := breakGlassFixtures(t,
		apipkg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	partyKey := "company/operator-" + strings.ToLower(store.NewULID())
	insertActorRevision(t, gated.store, gated.nsID, partyKey, "human", 1)
	credential := issueBreakGlassCredential(t, gated.store, partyKey)
	_, task := advanceToReview(t, open)

	if _, err := gated.store.RevokeInboundCredential(context.Background(), "actor", partyKey); err != nil {
		t.Fatalf("RevokeInboundCredential: %v", err)
	}

	resp, body := authedDecide(t, gated, task.ID, credential, decideHumanTaskReq{
		Outcome:               "approved",
		ExpectedLedgerVersion: 0,
	})
	requireStatus(t, resp, body, http.StatusUnauthorized)
	var refused map[string]any
	if err := json.Unmarshal(body, &refused); err != nil {
		t.Fatalf("decode refusal: %v (%s)", err, body)
	}
	if refused["reason"] != "credential_revoked" {
		t.Fatalf("reason = %v, want credential_revoked: %s", refused["reason"], body)
	}
	if !strings.Contains(logs.String(), "credential_revoked") || !strings.Contains(logs.String(), partyKey) {
		t.Errorf("refusal log line lacks the class or the subject: %s", logs.String())
	}
	if strings.Contains(logs.String(), credential) {
		t.Fatalf("the credential reached the log sink: %s", logs.String())
	}
}

// An issued credential for an AGENT actor keeps today's agent semantics: it
// is a machine principal, and a human decision under it is refused by role
// (PRD §10.4 — an agent may propose, never confirm). c48's break-glass is a
// PERSON's credential, and only a person's.
func TestAgentBreakGlassCredentialCannotDecideAHumanTask(t *testing.T) {
	gated, open := breakGlassFixtures(t)
	partyKey := "company/bridge-" + strings.ToLower(store.NewULID())
	insertActorRevision(t, gated.store, gated.nsID, partyKey, "agent", 1)
	credential := issueBreakGlassCredential(t, gated.store, partyKey)
	_, task := advanceToReview(t, open)

	resp, body := authedDecide(t, gated, task.ID, credential, decideHumanTaskReq{
		Outcome:               "approved",
		ExpectedLedgerVersion: 0,
	})
	requireStatus(t, resp, body, http.StatusForbidden)
	var refused map[string]any
	if err := json.Unmarshal(body, &refused); err != nil {
		t.Fatalf("decode refusal: %v (%s)", err, body)
	}
	if refused["reason"] != "forbidden_role" {
		t.Fatalf("reason = %v, want forbidden_role: %s", refused["reason"], body)
	}
}

// A `cnd_`-shaped bearer no credential record matches is still an unknown
// bearer: the existing 401 no_principal path is unchanged, and no admission
// state is touched for a party that does not exist.
func TestUnknownDialInBearerStillAnswersNoPrincipal(t *testing.T) {
	gated, open := breakGlassFixtures(t)
	_, task := advanceToReview(t, open)

	resp, body := authedDecide(t, gated, task.ID,
		actors.InboundCredentialPrefix+"not-a-credential-this-plane-ever-minted", decideHumanTaskReq{
			Outcome:               "approved",
			ExpectedLedgerVersion: 0,
		})
	requireStatus(t, resp, body, http.StatusUnauthorized)
	var refused map[string]any
	if err := json.Unmarshal(body, &refused); err != nil {
		t.Fatalf("decode refusal: %v (%s)", err, body)
	}
	if refused["reason"] != "no_principal" {
		t.Fatalf("reason = %v, want no_principal: %s", refused["reason"], body)
	}
}

// TestBreakGlassCredentialIsApproverAndNothingMore is h11 for this
// principal: `approver` is the role, so it decides, reviews and replies —
// and is refused on the namespace-administrator routes exactly as any other
// approver is. An operator locked out of Access can unblock the lane; they
// cannot register actors or publish workflows with a break-glass credential.
func TestBreakGlassCredentialIsApproverAndNothingMore(t *testing.T) {
	gated, _ := breakGlassFixtures(t)
	partyKey := "company/operator-" + strings.ToLower(store.NewULID())
	insertActorRevision(t, gated.store, gated.nsID, partyKey, "human", 1)
	credential := issueBreakGlassCredential(t, gated.store, partyKey)

	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/v1alpha1/actors"},
		{http.MethodPost, "/v1alpha1/workflows"},
		{http.MethodPost, "/v1alpha1/runs"},
		{http.MethodPost, "/v1alpha1/inbound/credentials"},
	} {
		var refused map[string]any
		resp, body := doJSONBearer(t, gated.client, route.method, gated.url(route.path),
			credential, map[string]any{}, &refused)
		requireStatus(t, resp, body, http.StatusForbidden)
		if refused["reason"] != "forbidden_role" {
			t.Errorf("%s %s: reason = %v, want forbidden_role", route.method, route.path, refused["reason"])
		}
	}
}

// A credential issued for a party this control plane registers no actor for
// binds to nothing, and says so: the same visible `unbound` refusal a person
// who passed Access with no binding gets (spec c46), never a silent pass.
func TestBreakGlassCredentialWithoutAnActorIsUnbound(t *testing.T) {
	gated, open := breakGlassFixtures(t)
	partyKey := "company/nobody-" + strings.ToLower(store.NewULID())
	credential := issueBreakGlassCredential(t, gated.store, partyKey)
	_, task := advanceToReview(t, open)

	resp, body := authedDecide(t, gated, task.ID, credential, decideHumanTaskReq{
		Outcome:               "approved",
		ExpectedLedgerVersion: 0,
	})
	requireStatus(t, resp, body, http.StatusForbidden)
	var refused map[string]any
	if err := json.Unmarshal(body, &refused); err != nil {
		t.Fatalf("decode refusal: %v (%s)", err, body)
	}
	if refused["reason"] != "unbound" {
		t.Fatalf("reason = %v, want unbound: %s", refused["reason"], body)
	}
}
