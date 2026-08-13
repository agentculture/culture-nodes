package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
)

// adhocRunReq mirrors components.schemas.AdhocRunRequest the way any real
// client would encode it (see api_test.go's note on why this package never
// imports internal/api's unexported wire types).
type adhocRunReq struct {
	Instruction    string `json:"instruction,omitempty"`
	ActorRef       string `json:"actor_ref,omitempty"`
	Repo           string `json:"repo,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
	SuccessOutcome string `json:"success_outcome,omitempty"`
	Timeout        string `json:"timeout,omitempty"`
}

// testAdhocActorRef mirrors the nodes-operator skill's assign refs: the
// digest suffix is a revision marker, not resolvable content, so run
// creation must accept it without the actor being registered.
const testAdhocActorRef = "actor://company/codex-thor@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestAdhocRunCreatesNormalPinnedRun is t19's first acceptance criterion:
// one API call takes an instruction and yields a normal, pinned-digest run
// whose workflow was published through the ordinary path (readable via GET
// /v1alpha1/workflows/{digest}) and whose run/ledger surfaces behave exactly
// like a hand-published run's.
func TestAdhocRunCreatesNormalPinnedRun(t *testing.T) {
	f := newFixture(t)

	var run apipkg.RunOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/adhoc-runs"),
		adhocRunReq{Instruction: "review the README for stale commands", ActorRef: testAdhocActorRef, Repo: "/tmp/culture-nodes"}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	if run.ID == "" {
		t.Fatal("run id is empty")
	}
	if !strings.HasPrefix(run.WorkflowDigest, "sha256:") {
		t.Fatalf("workflow_digest = %q, want a sha256: digest", run.WorkflowDigest)
	}
	if run.State != string(engine.RunRunning) {
		t.Fatalf("run state = %q, want %q", run.State, engine.RunRunning)
	}

	// The rendered workflow is a real published version, addressable by its
	// digest like any hand-published one.
	var version apipkg.WorkflowVersionOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/workflows/"+run.WorkflowDigest), nil, &version)
	requireStatus(t, resp, body, http.StatusOK)
	if version.Digest != run.WorkflowDigest {
		t.Fatalf("published version digest = %q, want %q", version.Digest, run.WorkflowDigest)
	}

	// The instruction and the defaulted fields all ride as run input — the
	// template's four required input bindings.
	var input map[string]string
	if err := json.Unmarshal(run.Input, &input); err != nil {
		t.Fatalf("decode run input %s: %v", run.Input, err)
	}
	want := map[string]string{
		"instruction":     "review the README for stale commands",
		"repo":            "/tmp/culture-nodes",
		"sandbox":         "read-only",
		"success_outcome": "completed",
	}
	for k, v := range want {
		if input[k] != v {
			t.Fatalf("run input %s = %q, want %q (full input: %s)", k, input[k], v, run.Input)
		}
	}

	// Run view: one active token, one ready node run at the template's
	// entry node — the same shape a hand-published single-node run has.
	var view apipkg.RunViewOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID), nil, &view)
	requireStatus(t, resp, body, http.StatusOK)
	if len(view.Tokens) != 1 || view.Tokens[0].State != "active" {
		t.Fatalf("expected exactly one active token, got %+v", view.Tokens)
	}
	if len(view.NodeRuns) != 1 || view.NodeRuns[0].NodeID != "task" || view.NodeRuns[0].State != "ready" {
		t.Fatalf("expected exactly one ready node run at 'task', got %+v", view.NodeRuns)
	}

	// Ledger surface answers for it like for any run.
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID+"/ledger"), nil, nil)
	requireStatus(t, resp, body, http.StatusOK)

	// And it lists among running runs.
	var runList apipkg.RunListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs?state=running"), nil, &runList)
	requireStatus(t, resp, body, http.StatusOK)
	if !containsRunID(runList.Items, run.ID) {
		t.Fatalf("adhoc run %s not present in ?state=running list", run.ID)
	}
}

// TestAdhocRunPublishIsIdempotentByDigest: identical parameters re-render
// byte-identically, so a second call reuses the same published workflow
// version (same digest, still exactly one version under the derived key)
// while still creating a fresh run. A changed render parameter produces a
// different digest under the same key.
func TestAdhocRunPublishIsIdempotentByDigest(t *testing.T) {
	f := newFixture(t)
	req := adhocRunReq{Instruction: "first pass", ActorRef: testAdhocActorRef, Repo: "/tmp/repo"}

	var first apipkg.RunOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/adhoc-runs"), req, &first)
	requireStatus(t, resp, body, http.StatusCreated)

	// The instruction is run input, not workflow content: even a different
	// instruction re-renders to the identical workflow, so the digest is
	// stable across assignments to the same actor.
	req.Instruction = "second pass, different instruction"
	var second apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/adhoc-runs"), req, &second)
	requireStatus(t, resp, body, http.StatusCreated)

	if second.ID == first.ID {
		t.Fatal("second call reused the first run id; every call must create a fresh run")
	}
	if second.WorkflowDigest != first.WorkflowDigest {
		t.Fatalf("digest changed across identical renders: %q vs %q", second.WorkflowDigest, first.WorkflowDigest)
	}

	var list apipkg.WorkflowVersionListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/workflows?workflow_key=adhoc-codex-thor"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	if len(list.Items) != 1 {
		t.Fatalf("expected exactly one published version under adhoc-codex-thor after two identical renders, got %d: %+v", len(list.Items), list.Items)
	}

	// A different timeout is workflow content — new digest, second version.
	req.Timeout = "30m"
	var third apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/adhoc-runs"), req, &third)
	requireStatus(t, resp, body, http.StatusCreated)
	if third.WorkflowDigest == first.WorkflowDigest {
		t.Fatal("digest unchanged although the rendered timeout differs")
	}
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/workflows?workflow_key=adhoc-codex-thor"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	if len(list.Items) != 2 {
		t.Fatalf("expected two published versions under adhoc-codex-thor after a changed render, got %d", len(list.Items))
	}
}

// TestAdhocRunValidation: every malformed request is refused with 400 in
// the documented Error shape before anything is rendered or published.
func TestAdhocRunValidation(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name string
		req  adhocRunReq
	}{
		{"missing_instruction", adhocRunReq{ActorRef: testAdhocActorRef, Repo: "/tmp/repo"}},
		{"missing_actor_ref", adhocRunReq{Instruction: "do a thing", Repo: "/tmp/repo"}},
		{"missing_repo", adhocRunReq{Instruction: "do a thing", ActorRef: testAdhocActorRef}},
		{"malformed_actor_ref", adhocRunReq{Instruction: "do a thing", ActorRef: "codex-thor", Repo: "/tmp/repo"}},
		{"malformed_success_outcome", adhocRunReq{Instruction: "do a thing", ActorRef: testAdhocActorRef, Repo: "/tmp/repo", SuccessOutcome: "Not-Valid"}},
		{"malformed_timeout", adhocRunReq{Instruction: "do a thing", ActorRef: testAdhocActorRef, Repo: "/tmp/repo", Timeout: "15 minutes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/adhoc-runs"), tc.req, nil)
			requireStatus(t, resp, body, http.StatusBadRequest)
			decodeAPIError(t, body)
		})
	}
}
