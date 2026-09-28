package api_test

// Task t30 (#328; spec q22), reworked by t30b (d4): publish warns on every
// widening reference with its exposure state, the owner's approval inbox
// (GET /v1alpha1/sensitivity-approvals, POST .../{id}/decision) accepts a
// decision only from the owner as a human, and repository visibility
// (GET/POST /v1alpha1/repository-visibility) is set by an operator, never an agent.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/declengine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// announceDeclSource renders a Jira-triggered Discord post carrying text,
// listing exposes.
func announceDeclSource(name, text string, exposes ...string) string {
	with, _ := json.Marshal(map[string]any{"uses": "actor://discord", "input": map[string]string{"text": text}})
	b, _ := json.Marshal(struct {
		declBody
		Exposes []string `json:"exposes,omitempty"`
	}{declBody{
		Name:        name,
		Trigger:     declTrigger{Kind: "jira.issue.created"},
		Action:      declAction{Kind: "discord.post", With: with},
		StartNode:   declNode{Name: "ready", Deadline: "none"},
		LandingNode: declNode{Name: "posted", Deadline: "none"},
	}, exposes})
	return string(b)
}

func sensitivityWarningsOf(ws []string) []string {
	var out []string
	for _, w := range ws {
		if strings.HasPrefix(w, "sensitivity: ") {
			out = append(out, w)
		}
	}
	return out
}

type sensitivityApprovalResp struct {
	ID     string `json:"id"`
	Owner  string `json:"owner"`
	Status string `json:"status"`
}

// openApproval opens the task a blocked firing of version v would open.
func openApproval(t *testing.T, s *storepg.Store, nsID, versionID, declarationID, variable, owner string) string {
	t.Helper()
	return openApprovalNamed(t, s, nsID, "announce", versionID, declarationID, variable, owner)
}

// openApprovalNamed is openApproval for a firing declaration called name.
func openApprovalNamed(t *testing.T, s *storepg.Store, nsID, name, versionID, declarationID, variable, owner string) string {
	t.Helper()
	a, err := (declengine.PostgresBackend{Store: s}).RequestSensitivityApproval(context.Background(), declengine.SensitivityApprovalRequest{
		NamespaceID: nsID, DeclarationName: name, Variable: variable, Owner: owner,
		DeclarationID: declarationID, DeclarationVersionID: versionID,
		SourceDeclarationID: declarationID, SourceVersionID: versionID, EventID: "evt",
		Source: decl.SourceSensitivity("jira.issue.created"), Target: decl.TargetSensitivity("discord.post"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func TestPublishWarnsOnWideningReferenceAndOwnerDecides(t *testing.T) {
	srv, nsID, token, humanActorID := newDeclarationHumanFixture(t)

	var published declarationVersionResp
	rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token,
		declarationSourceReq{Format: "json", Source: announceDeclSource("announce", "New: {summary} {summary}")}, &published)
	if rr.Code != http.StatusCreated {
		t.Fatalf("publish: status = %d: %s", rr.Code, rr.Body.String())
	}
	sensitivity := sensitivityWarningsOf(published.Warnings)
	if len(sensitivity) != 1 || !strings.Contains(sensitivity[0], "discord (public audience)") || !strings.Contains(sensitivity[0], `add "summary" to exposes`) {
		t.Fatalf("publish warnings = %q, want exactly one unlisted sensitivity warning for {summary}", published.Warnings)
	}

	// Listing the entry turns the warning into its approval state.
	rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token,
		declarationSourceReq{Format: "json", Source: announceDeclSource("announce", "New: {summary} {summary}", "summary")}, &published)
	if rr.Code != http.StatusCreated {
		t.Fatalf("republish: status = %d: %s", rr.Code, rr.Body.String())
	}
	if sensitivity := sensitivityWarningsOf(published.Warnings); len(sensitivity) != 1 || !strings.Contains(sensitivity[0], "exposure listed, no approval task yet") {
		t.Fatalf("listed publish warnings = %q", published.Warnings)
	}

	id := openApproval(t, requireStore(t), nsID, published.ID, published.DeclarationID, "summary", humanActorID)
	var inbox struct {
		Items []sensitivityApprovalResp `json:"items"`
	}
	rr = doAccess(t, srv, http.MethodGet, "/v1alpha1/sensitivity-approvals?status=pending&owner="+humanActorID, "", nil, &inbox)
	if rr.Code != http.StatusOK || len(inbox.Items) != 1 || inbox.Items[0].ID != id {
		t.Fatalf("inbox: %d %s", rr.Code, rr.Body.String())
	}

	var decision struct {
		Decision string `json:"decision"`
		Decider  string `json:"decider"`
	}
	rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/sensitivity-approvals/"+id+"/decision", token,
		map[string]string{"decision": "approved", "note": "ok"}, &decision)
	if rr.Code != http.StatusCreated || decision.Decision != "approved" || decision.Decider != humanActorID {
		t.Fatalf("owner decision: %d %s", rr.Code, rr.Body.String())
	}
	var validation declarationValidationResp
	doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/validate", token,
		declarationSourceReq{Format: "json", Source: announceDeclSource("announce", "New: {summary} {summary}", "summary")}, &validation)
	if sensitivity := sensitivityWarningsOf(validation.Warnings); len(sensitivity) != 1 || !strings.Contains(sensitivity[0], "exposure listed, approved") {
		t.Fatalf("approved validate warnings = %q", validation.Warnings)
	}
	rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/sensitivity-approvals/"+id+"/decision", token,
		map[string]string{"decision": "maybe"}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown decision: status = %d, want 400", rr.Code)
	}
	rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/sensitivity-approvals/nope/decision", token,
		map[string]string{"decision": "refused"}, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown task: status = %d, want 404", rr.Code)
	}

	// A different owner's task: this human is refused, nothing is appended.
	other := openApproval(t, requireStore(t), nsID, published.ID, published.DeclarationID, "reporter", "someone-else")
	rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/sensitivity-approvals/"+other+"/decision", token,
		map[string]string{"decision": "approved"}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-owner decision: status = %d, want 403: %s", rr.Code, rr.Body.String())
	}
}

// An agent -- even the owner of the variable -- cannot decide.
func TestAgentCannotDecideSensitivityApproval(t *testing.T) {
	f, agent := newAgentBearerFixture(t)
	var published declarationVersionResp
	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/declarations"), mergeGateToken,
		declarationSourceReq{Format: "json", Source: announceDeclSource("agent-announce", "{summary}")}, &published)
	requireStatus(t, resp, body, http.StatusCreated)
	if published.Author != agent {
		t.Fatalf("agent-published author = %q, want %q", published.Author, agent)
	}
	id := openApproval(t, f.store, f.nsID, published.ID, published.DeclarationID, "summary", agent)
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/sensitivity-approvals/"+id+"/decision"), mergeGateToken,
		map[string]string{"decision": "approved"}, nil)
	if resp.StatusCode < 400 {
		t.Fatalf("agent decision: status = %d, want a refusal: %s", resp.StatusCode, body)
	}
	// Naming a head (the t40b correction path) does not let an agent in.
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/sensitivity-approvals/"+id+"/decision"), mergeGateToken,
		map[string]string{"decision": "approved", "supersedes": "01ANYDECISION"}, nil)
	if resp.StatusCode < 400 {
		t.Fatalf("agent correction: status = %d, want a refusal: %s", resp.StatusCode, body)
	}
	ds, err := declengine.ListSensitivityDecisions(context.Background(), f.store, f.nsID, id)
	if err != nil || len(ds) != 0 {
		t.Fatalf("agent decision was recorded: %+v %v", ds, err)
	}
}

// Repository visibility (t30b, d4): a person or a registered agent actor
// records it, the record names the authenticated principal, the newest row
// per repository is the one listed, and a malformed request is refused.
func TestRepositoryVisibilityRoutes(t *testing.T) {
	srv, _, token, humanActorID := newDeclarationHumanFixture(t)
	var rec struct {
		Repository string `json:"repository"`
		Visibility string `json:"visibility"`
		SetBy      string `json:"set_by"`
	}
	rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/repository-visibility", token,
		map[string]string{"repository": "AgentCulture/Culture-Nodes", "visibility": "public", "note": "open source"}, &rec)
	if rr.Code != http.StatusCreated || rec.Repository != "agentculture/culture-nodes" || rec.SetBy != humanActorID {
		t.Fatalf("set: %d %s", rr.Code, rr.Body.String())
	}
	doAccess(t, srv, http.MethodPost, "/v1alpha1/repository-visibility", token,
		map[string]string{"repository": "agentculture/culture-nodes", "visibility": "private"}, nil)
	for _, bad := range []map[string]string{{"repository": "nope", "visibility": "public"}, {"repository": "a/b", "visibility": "internal"}} {
		if rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/repository-visibility", token, bad, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("malformed %v: status = %d, want 400", bad, rr.Code)
		}
	}
	var list struct {
		Items []struct {
			Repository string `json:"repository"`
			Visibility string `json:"visibility"`
		} `json:"items"`
	}
	rr = doAccess(t, srv, http.MethodGet, "/v1alpha1/repository-visibility", "", nil, &list)
	if rr.Code != http.StatusOK || len(list.Items) != 1 || list.Items[0].Visibility != "private" {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/repository-visibility", "", map[string]string{"repository": "a/b", "visibility": "public"}, nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated set: status = %d, want 401", rr.Code)
	}
}

// An agent's own bearer may not set visibility: marking a private
// repository public would rank its data public as a source, so exposing it
// would never widen and never open the owner's approval task (d4).
func TestAgentCannotSetRepositoryVisibility(t *testing.T) {
	f, _ := newAgentBearerFixture(t)
	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/repository-visibility"), mergeGateToken,
		map[string]string{"repository": "acme/widgets", "visibility": "public"}, nil)
	// Never resolved on an agent-less route, as elsewhere: 401, nothing written.
	requireStatus(t, resp, body, http.StatusUnauthorized)
	var list struct {
		Items []struct {
			Repository string `json:"repository"`
		} `json:"items"`
	}
	resp, body = doJSONBearer(t, f.client, http.MethodGet, f.url("/v1alpha1/repository-visibility"), mergeGateToken, nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	for _, item := range list.Items {
		if item.Repository == "acme/widgets" {
			t.Fatalf("a refused agent write was recorded: %+v", list.Items)
		}
	}
}
