package api_test

// Task t30 (#328; spec q22): publish warns on every widening reference, and
// the owner's sensitivity approval inbox (GET /v1alpha1/sensitivity-approvals,
// POST .../{id}/decision) accepts a decision only from the owner as a human.

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

// announceDeclSource renders a Jira-triggered Discord post carrying text.
func announceDeclSource(name, text string) string {
	with, _ := json.Marshal(map[string]any{"uses": "actor://discord", "input": map[string]string{"text": text}})
	b, _ := json.Marshal(declBody{
		Name:        name,
		Trigger:     declTrigger{Kind: "jira.issue.created"},
		Action:      declAction{Kind: "discord.post", With: with},
		StartNode:   declNode{Name: "ready", Deadline: "none"},
		LandingNode: declNode{Name: "posted", Deadline: "none"},
	})
	return string(b)
}

type sensitivityApprovalResp struct {
	ID     string `json:"id"`
	Owner  string `json:"owner"`
	Status string `json:"status"`
}

// openApproval opens the task a blocked firing of version v would open.
func openApproval(t *testing.T, s *storepg.Store, nsID, versionID, declarationID, variable, owner string) string {
	t.Helper()
	a, err := (declengine.PostgresBackend{Store: s}).RequestSensitivityApproval(context.Background(), declengine.SensitivityApprovalRequest{
		NamespaceID: nsID, DeclarationID: declarationID, DeclarationVersionID: versionID,
		SourceDeclarationID: declarationID, SourceVersionID: versionID, Variable: variable, Owner: owner, EventID: "evt",
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
	var sensitivity []string
	for _, w := range published.Warnings {
		if strings.HasPrefix(w, "sensitivity: ") {
			sensitivity = append(sensitivity, w)
		}
	}
	if len(sensitivity) != 1 || !strings.Contains(sensitivity[0], "discord (public audience)") {
		t.Fatalf("publish warnings = %q, want exactly one sensitivity warning for {summary}", published.Warnings)
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
	ds, err := declengine.ListSensitivityDecisions(context.Background(), f.store, f.nsID, id)
	if err != nil || len(ds) != 0 {
		t.Fatalf("agent decision was recorded: %+v %v", ds, err)
	}
}
