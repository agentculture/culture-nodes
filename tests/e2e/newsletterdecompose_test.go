package e2etest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// newsletterWorkflowPath is task t24's second instance of the generic
// decompose pipeline (issue #45): a non-code domain, proving the same
// document -> claims (with sources) -> connected decisions and actions ->
// verified-in-the-end shape task t22 proved on a devague plan import.
const newsletterWorkflowPath = "../../examples/newsletter-decompose/workflow.yaml"

// TestNewsletterDecomposeWorkflowCompilesCleanlyAndDeterministically needs no
// database, mirroring reference_test.go's check on examples/delivery-loop:
// the example this task ships is a real, valid Culture Nodes workflow, not
// illustrative-only YAML nobody has run through the compiler.
func TestNewsletterDecomposeWorkflowCompilesCleanlyAndDeterministically(t *testing.T) {
	source, err := os.ReadFile(filepath.Clean(newsletterWorkflowPath))
	if err != nil {
		t.Fatalf("read %s: %v", newsletterWorkflowPath, err)
	}

	compiled, diags, err := compiler.Compile(source, compiler.FormatYAML)
	if err != nil {
		t.Fatalf("Compile returned an internal error: %v", err)
	}
	if compiled == nil {
		t.Fatalf("the newsletter-decompose workflow did not compile: %+v", diags)
	}
	for _, d := range diags {
		t.Errorf("unexpected %s diagnostic at %s: %s — %s", d.Level, d.Path, d.Code, d.Message)
	}
	if compiled.Name != "newsletter-decompose" || compiled.Version != "1.0.0" {
		t.Errorf("name/version = %q/%q, want newsletter-decompose/1.0.0", compiled.Name, compiled.Version)
	}

	second, _, err := compiler.Compile(source, compiler.FormatYAML)
	if err != nil || second == nil {
		t.Fatalf("second Compile: %v", err)
	}
	if second.Digest != compiled.Digest {
		t.Errorf("digest differs between compilations: %s vs %s", compiled.Digest, second.Digest)
	}
	if !bytes.Equal(second.Normalized, compiled.Normalized) {
		t.Error("normalized IR differs between compilations of identical source")
	}
}

// newsletterClaim is one real, web-sourced claim this test drives through
// the workflow's `scope` node. Gathered live (WebSearch, August 2026) for
// task t24's delivery artifact — see docs/deliveries/2026-08-14-t24-newsletter-decompose.md
// for the full record of what ran live versus what this fixture scripts.
type newsletterClaim struct {
	id, statement, sourceURL, sourceTitle string
}

var newsletterClaims = []newsletterClaim{
	{
		id:          "c1",
		statement:   "Amazon renamed Bedrock Agents to \"Bedrock Agents Classic\" and closed it to new customers as of July 30, 2026, while systems integrators push customers toward cross-cloud, vendor-neutral multi-agent workflows.",
		sourceURL:   "https://aiagentstore.ai/ai-agent-news/this-week",
		sourceTitle: "AI Agents News — Week of August 13, 2026",
	},
	{
		id:          "c2",
		statement:   "High-risk obligations under the EU AI Act became enforceable August 2, 2026, and the May 2026 Digital Omnibus agreement clarified that a multi-agent system is treated as a single regulated system for liability, even though each agent is still classified individually.",
		sourceURL:   "https://the-agent-report.com/2026/06/eu-ai-act-agent-regulation/",
		sourceTitle: "EU AI Act and Autonomous Agents: The Regulatory Reckoning Arrives August 2026",
	},
	{
		id:          "c3",
		statement:   "Article 14 of the EU AI Act requires high-risk AI systems to be designed with human-machine interface tools enabling effective human oversight, including a functional \"stop button\", with deployers assigning oversight to competent, trained, authorized personnel.",
		sourceURL:   "https://artificialintelligenceact.eu/article/14/",
		sourceTitle: "Article 14: Human Oversight | EU Artificial Intelligence Act",
	},
}

// newsletterAgents is the scripted stand-in for the workflow's four agent
// actors, mirroring harness_test.go's deliveryAgents but for a single,
// straight-line pass (no loop-back scripting: verify always passes). The
// claim/decision/result content each node proposes is the REAL,
// web-search-sourced content in newsletterClaims — the pipeline's
// structure and ledger authority are exercised live by the real engine,
// worker, and PostgreSQL; the agents' own "judgement" is the only scripted
// part, exactly as deliveryAgents' business judgement is the only scripted
// part of examples/delivery-loop's own e2e proof.
type newsletterAgents struct {
	server *httptest.Server

	mu       sync.Mutex
	actorIDs map[string]string
	requests []actors.InvocationRequest
	failures []string
}

func newNewsletterAgents(t *testing.T, actorIDs map[string]string) *newsletterAgents {
	t.Helper()
	a := &newsletterAgents{actorIDs: actorIDs}
	a.server = httptest.NewServer(http.HandlerFunc(a.handle))
	t.Cleanup(a.server.Close)
	return a
}

func (a *newsletterAgents) URL() string { return a.server.URL }

func (a *newsletterAgents) handle(w http.ResponseWriter, r *http.Request) {
	var req actors.InvocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad invocation", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	a.requests = append(a.requests, req)
	actorID := a.actorIDs[req.Node.ID]
	a.mu.Unlock()

	result, err := newsletterScript(req, actorID)
	if err != nil {
		a.mu.Lock()
		a.failures = append(a.failures, err.Error())
		a.mu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (a *newsletterAgents) scriptFailures() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.failures...)
}

func newsletterScript(req actors.InvocationRequest, actorID string) (actors.InvocationResult, error) {
	propose := func(recordType ledger.RecordType, data any, subjectRef string, provenance ...string) ledger.Record {
		payload, _ := json.Marshal(data)
		rec := ledger.Record{
			RecordType: recordType,
			Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: actorID},
			Authority:  ledger.AuthorityProposed,
			Data:       payload,
		}
		if subjectRef != "" {
			rec.SubjectRef = ledger.NullableID(subjectRef)
		}
		if len(provenance) > 0 {
			rec.ProvenanceRefs = provenance
		}
		return rec
	}

	switch req.Node.ID {
	case "scope":
		records := make([]ledger.Record, 0, len(newsletterClaims))
		claimIDs := make([]string, 0, len(newsletterClaims))
		for _, c := range newsletterClaims {
			id := "newsletter_claim_" + c.id
			rec := propose(ledger.RecordClaim, map[string]any{
				"statement": c.statement,
				"kind":      "requirement",
				"sources": []map[string]string{
					{"url": c.sourceURL, "title": c.sourceTitle},
				},
			}, "")
			// scope mints its own claim ids explicitly (a caller-supplied
			// ledger.Record id is honored unchanged — internal/engine's
			// prepareRecord only assigns one when empty) so `plan` can bind
			// /nodes/scope/output/claim_ids directly rather than re-deriving
			// which record is which from a projection.
			rec.ID = id
			records = append(records, rec)
			claimIDs = append(claimIDs, id)
		}
		out, _ := json.Marshal(map[string]any{
			"summary":   "scoped 3 real, sourced claims about AI agent orchestration news, August 2026",
			"claim_ids": claimIDs,
		})
		return actors.InvocationResult{
			Outcome:     "completed",
			Output:      out,
			LedgerDelta: &actors.LedgerDelta{Records: records},
		}, nil

	case "plan":
		// One decision per claim, connected through provenance_refs — the
		// exact mechanism MapPlanShow's coveredClaimRefs already proves for
		// the code-plan instance (internal/devague/plan_show.go).
		var scopeOutput struct {
			ClaimIDs []string `json:"claim_ids"`
		}
		if err := json.Unmarshal(req.Input, &scopeOutput); err != nil {
			return actors.InvocationResult{}, err
		}
		if len(scopeOutput.ClaimIDs) != len(newsletterClaims) {
			return actors.InvocationResult{}, fmt.Errorf("plan invocation received %d claim ids, want %d", len(scopeOutput.ClaimIDs), len(newsletterClaims))
		}
		records := make([]ledger.Record, 0, len(scopeOutput.ClaimIDs))
		articles := make([]map[string]any, 0, len(scopeOutput.ClaimIDs))
		for _, claimID := range scopeOutput.ClaimIDs {
			records = append(records, propose(ledger.RecordDecision, map[string]any{
				"selected": "write a short explainer article",
				"question": "which claim does this article explain?",
			}, "", claimID))
			articles = append(articles, map[string]any{"claim_id": claimID})
		}
		out, _ := json.Marshal(map[string]any{"articles": articles})
		return actors.InvocationResult{
			Outcome:     "completed",
			Output:      out,
			LedgerDelta: &actors.LedgerDelta{Records: records},
		}, nil

	case "write":
		records := make([]ledger.Record, 0, len(newsletterClaims))
		for _, c := range newsletterClaims {
			records = append(records, propose(ledger.RecordResult, map[string]any{
				"summary": "drafted a short blurb citing " + c.sourceURL,
			}, ""))
		}
		return actors.InvocationResult{
			Outcome:     "completed",
			Output:      json.RawMessage(`{"drafts":[{"count":3}]}`),
			LedgerDelta: &actors.LedgerDelta{Records: records},
		}, nil

	case "verify":
		return actors.InvocationResult{
			Outcome: "passed",
			Output:  json.RawMessage(`{"verdict":"passed"}`),
			LedgerDelta: &actors.LedgerDelta{Records: []ledger.Record{
				propose(ledger.RecordEvidence, map[string]any{
					"collection_method": "model_review",
					"covered_scope":     "every scoped claim carries a source; every planned article traces to the claim that motivates it",
				}, ""),
			}},
		}, nil
	}

	return actors.InvocationResult{}, fmt.Errorf("no script for node %q", req.Node.ID)
}

// TestNewsletterDecomposeRunsEndToEndWithRealSourcedContent is the t24
// engine-surface proof: the newsletter-decompose workflow, published and
// started through the real HTTP API, driven by the real engine and a real
// worker against a real PostgreSQL — the same fidelity slice_test.go proves
// for the code-repo instance. Every claim the run's ledger holds carries a
// REAL source gathered live via web search (July/August 2026 AI-agent news);
// only the agents' own judgement is scripted, exactly as
// examples/delivery-loop's own proof scripts its four agents' judgement and
// nothing else.
//
// This test is honestly labelled in the delivery artifact as exercising the
// pipeline through the real engine/worker/ledger with SCRIPTED actors, not
// through a live dispatch to a registered actor on the deployed control
// plane (codex-thor/codex-orin) — see
// docs/deliveries/2026-08-14-t24-newsletter-decompose.md.
func TestNewsletterDecomposeRunsEndToEndWithRealSourcedContent(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "e2e-newsletter")

	agentIDs := map[string]string{}
	agents := newNewsletterAgents(t, agentIDs)
	registered := registerNewsletterActors(t, s, ns.ID, agents.URL())
	for node, id := range registered {
		agentIDs[node] = id
	}

	stack := startStack(t, stackConfig{
		namespaceID: ns.ID,
		agentsURL:   agents.URL(),
		runner:      &scriptedRunner{},
	})
	defer stack.stop()

	digest := stack.publishWorkflowFile(t, newsletterWorkflowPath)
	runID := stack.createRun(t, digest, json.RawMessage(`{"topic":"AI agent orchestration news, August 2026"}`))

	view := stack.waitForTerminal(t, runID, 60*time.Second)
	if failures := agents.scriptFailures(); len(failures) > 0 {
		t.Fatalf("the scripted agents refused an invocation: %v", failures)
	}
	if view.Run.State != string(engine.RunCompleted) {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", view.Run.State, stack.errors())
	}
	if !bytes.Contains(view.Run.Output, []byte(`"passed"`)) {
		t.Errorf("run output = %s, want the verifier's passing verdict", view.Run.Output)
	}

	led := ledgerFor(t, stack.db, ns.ID)
	records, err := led.Records(context.Background(), runID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	assertClaimChainIsSourcedAndMotivated(t, records)
	assertVerifyEvidenceStaysProposed(t, records)
	assertEveryClaimCarriesItsSourceURL(t, records)
}

// assertClaimChainIsSourcedAndMotivated checks h19's own acceptance bullet,
// exactly the way `nodes chain-verify` checks it against a live run: every
// claim sourced, every decision motivated.
func assertClaimChainIsSourcedAndMotivated(t *testing.T, records []ledger.Record) {
	t.Helper()
	verdict := ledger.VerifyClaimChain(records)
	if !verdict.Passed {
		t.Fatalf("VerifyClaimChain = %+v, want passed: this run's claims are real and sourced", verdict)
	}
	if len(verdict.Claims) != len(newsletterClaims) {
		t.Fatalf("verdict.Claims = %+v, want %d (one per real claim)", verdict.Claims, len(newsletterClaims))
	}
	for _, c := range verdict.Claims {
		if !c.Sourced {
			t.Errorf("claim %s = %+v, want sourced", c.ClaimID, c)
		}
	}
	if len(verdict.Motivated) != len(newsletterClaims) {
		t.Fatalf("verdict.Motivated = %+v, want %d (one decision per claim)", verdict.Motivated, len(newsletterClaims))
	}
	for _, m := range verdict.Motivated {
		if !m.Motivated {
			t.Errorf("decision %s = %+v, want motivated", m.RecordID, m)
		}
	}
}

// assertVerifyEvidenceStaysProposed checks that a verification node which asks
// a model produces `proposed` (CLAUDE.md's ledger authority model) — the verify
// node's own evidence record must never claim more than that.
func assertVerifyEvidenceStaysProposed(t *testing.T, records []ledger.Record) {
	t.Helper()
	var evidenceCount int
	for _, rec := range records {
		if rec.RecordType != ledger.RecordEvidence {
			continue
		}
		evidenceCount++
		if rec.Authority != ledger.AuthorityProposed {
			t.Errorf("verify's evidence record authority = %q, want proposed: it asked a model, it did not compute a check", rec.Authority)
		}
		if rec.Origin.Kind != ledger.OriginAgent {
			t.Errorf("verify's evidence record origin = %q, want agent", rec.Origin.Kind)
		}
	}
	if evidenceCount != 1 {
		t.Fatalf("evidence records = %d, want 1", evidenceCount)
	}
}

// assertEveryClaimCarriesItsSourceURL checks that every source URL a real web
// search returned actually made it into the ledger's own claim payloads — the
// content is real, not a placeholder.
func assertEveryClaimCarriesItsSourceURL(t *testing.T, records []ledger.Record) {
	t.Helper()
	for _, c := range newsletterClaims {
		found := false
		for _, rec := range records {
			if rec.RecordType != ledger.RecordClaim {
				continue
			}
			if bytes.Contains(rec.Data, []byte(c.sourceURL)) {
				found = true
			}
		}
		if !found {
			t.Errorf("no claim record carries source URL %s", c.sourceURL)
		}
	}
}

// publishWorkflowFile is publishWorkflow generalized to an arbitrary path —
// harness_test.go's own publishWorkflow is hardcoded to
// referenceWorkflowPath (examples/delivery-loop).
func (s *stack) publishWorkflowFile(t *testing.T, path string) string {
	t.Helper()
	source, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var published struct {
		Digest string `json:"digest"`
	}
	status := s.postJSON("/v1alpha1/workflows", map[string]string{
		"format": "yaml",
		"source": string(source),
	}, &published)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("publish workflow %s: status %d", path, status)
	}
	if published.Digest == "" {
		t.Fatalf("publish workflow %s returned no digest", path)
	}
	return published.Digest
}

// registerNewsletterActors inserts the actors rows the newsletter-decompose
// workflow's `uses` references resolve against — the newsletter-specific
// twin of harness_test.go's registerActors (which is hardcoded to
// examples/delivery-loop's agentActorKeys).
func registerNewsletterActors(t *testing.T, db *postgres.Store, namespaceID, agentsURL string) map[string]string {
	t.Helper()
	keys := map[string]string{
		"scope":  "company/newsletter-scout",
		"plan":   "company/newsletter-editor",
		"write":  "company/newsletter-writer",
		"verify": "company/newsletter-verifier",
	}
	agents := map[string]string{}
	for nodeID, key := range keys {
		id := "actor_" + idstore.NewULID()
		if _, err := db.Pool().Exec(context.Background(), `
			INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref)
			VALUES ($1, $2, $3, 1, 'agent', 'http', $4)
		`, id, namespaceID, key, agentsURL); err != nil {
			t.Fatalf("register actor %s: %v", key, err)
		}
		agents[nodeID] = id
	}
	return agents
}
