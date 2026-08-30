package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// seedAttempt writes one attempt directly through postgres.EngineStore's
// InsertAttempt, bypassing the real claim/complete flow entirely. These
// tests are about the *shape* GET /v1alpha1/runs/{id} and GET
// /v1alpha1/node-runs render the §13.2 rollup in (task t2's acceptance
// criterion 3) — the aggregation rules themselves (retry burn included,
// unreported attempts excluded, cost never summed across currencies) are
// proven exhaustively at the store layer in
// internal/store/postgres/usage_rollup_test.go. Writing attempts directly
// here, the same way that package's own tests do, keeps this file focused
// on "does the JSON look right" without needing a retry-policy-enabled
// workflow fixture just to get two attempts onto one node run.
func seedAttempt(t *testing.T, f *fixture, nodeRunID string, n int, status engine.TechStatus, usage *engine.Usage) {
	t.Helper()
	es, err := storepg.NewEngineStore(f.store, f.nsID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	err = es.InTx(context.Background(), func(ctx context.Context, tx engine.Tx) error {
		return tx.InsertAttempt(ctx, engine.Attempt{
			ID: store.NewULID(), NodeRunID: nodeRunID, Number: n, Status: status, Usage: usage,
		})
	})
	if err != nil {
		t.Fatalf("seed attempt %d on node run %s: %v", n, nodeRunID, err)
	}
}

// createMinimalRun publishes minimal.workflow.yaml and creates one run
// against it, returning the created run and its single "start" node run's
// id (the same fixture shape TestListNodeRunsAcrossRuns uses).
func createMinimalRun(t *testing.T, f *fixture) (apipkg.RunOut, string) {
	t.Helper()
	source := readFixtureWorkflow(t, "minimal.workflow.yaml")
	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	view := getRunView(t, f, run.ID)
	if len(view.NodeRuns) != 1 {
		t.Fatalf("run %s: got %d node runs, want 1", run.ID, len(view.NodeRuns))
	}
	return run, view.NodeRuns[0].ID
}

// TestCreateRunUsageIsPresentButEmpty proves a just-created run (zero
// attempts, let alone ones that reported usage) still carries a non-nil
// usage block on POST /v1alpha1/runs — "not yet computed" is never how a
// caller learns nothing has happened; "computed, and it says zero" is.
func TestCreateRunUsageIsPresentButEmpty(t *testing.T) {
	f := newFixture(t)
	run, _ := createMinimalRun(t, f)

	if run.Usage == nil {
		t.Fatal("Usage is nil on a freshly created run, want a present-but-empty rollup")
	}
	if run.Usage.AttemptsReported != 0 || run.Usage.AttemptsNotReported != 0 {
		t.Errorf("AttemptsReported/NotReported = %d/%d, want 0/0 (no attempts exist yet)",
			run.Usage.AttemptsReported, run.Usage.AttemptsNotReported)
	}
	if run.Usage.InputTokens != 0 || run.Usage.OutputTokens != 0 {
		t.Errorf("tokens = %d/%d, want 0/0", run.Usage.InputTokens, run.Usage.OutputTokens)
	}
	if run.Usage.Cost != nil || run.Usage.Currency != "" || len(run.Usage.CostByCurrency) != 0 {
		t.Errorf("cost fields = %+v, want all unset", run.Usage)
	}
}

// TestRunDetailUsageSumsAllAttemptsIncludingFailed is the API-level
// retry-burn shape check: GET /v1alpha1/runs/{id} must report a failed
// attempt's tokens alongside a subsequent succeeded attempt's, both summed
// into run.usage, with a single coherent currency rendered as scalar
// cost+currency.
func TestRunDetailUsageSumsAllAttemptsIncludingFailed(t *testing.T) {
	f := newFixture(t)
	run, nodeRunID := createMinimalRun(t, f)

	cost1, cost2 := 0.01, 0.02
	usd := "USD"
	seedAttempt(t, f, nodeRunID, 1, engine.StatusFailed, &engine.Usage{InputTokens: 100, OutputTokens: 40, Cost: &cost1, Currency: &usd})
	seedAttempt(t, f, nodeRunID, 2, engine.StatusSucceeded, &engine.Usage{InputTokens: 20, OutputTokens: 10, Cost: &cost2, Currency: &usd})

	view := getRunView(t, f, run.ID)
	usage := view.Run.Usage
	if usage == nil {
		t.Fatal("run detail Usage is nil")
	}
	if usage.InputTokens != 120 || usage.OutputTokens != 50 {
		t.Fatalf("tokens = %d/%d, want 120/50 (the failed attempt's usage must still count)", usage.InputTokens, usage.OutputTokens)
	}
	if usage.AttemptsReported != 2 || usage.AttemptsNotReported != 0 {
		t.Fatalf("reported/not_reported = %d/%d, want 2/0", usage.AttemptsReported, usage.AttemptsNotReported)
	}
	if usage.Cost == nil || *usage.Cost < 0.0299 || *usage.Cost > 0.0301 {
		t.Fatalf("Cost = %v, want ~0.03", usage.Cost)
	}
	if usage.Currency != "USD" {
		t.Fatalf("Currency = %q, want USD", usage.Currency)
	}
	if len(usage.CostByCurrency) != 0 {
		t.Fatalf("CostByCurrency = %+v, want empty for a single coherent currency", usage.CostByCurrency)
	}
}

// TestRunDetailUsageDistinguishesUnreportedFromZero proves the API shape
// keeps "nobody reported usage" (attempts_reported: 0) visibly distinct
// from a token sum that merely happens to equal zero.
func TestRunDetailUsageDistinguishesUnreportedFromZero(t *testing.T) {
	f := newFixture(t)
	run, nodeRunID := createMinimalRun(t, f)

	seedAttempt(t, f, nodeRunID, 1, engine.StatusFailed, nil)
	seedAttempt(t, f, nodeRunID, 2, engine.StatusCancelled, nil)

	view := getRunView(t, f, run.ID)
	usage := view.Run.Usage
	if usage == nil {
		t.Fatal("run detail Usage is nil")
	}
	if usage.AttemptsReported != 0 || usage.AttemptsNotReported != 2 {
		t.Fatalf("reported/not_reported = %d/%d, want 0/2", usage.AttemptsReported, usage.AttemptsNotReported)
	}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Fatalf("tokens = %d/%d, want 0/0", usage.InputTokens, usage.OutputTokens)
	}
}

// TestRunDetailUsageMixedCurrenciesExposedSeparately proves a run whose
// attempts priced their work in two different currencies never collapses
// into one summed number: the API renders cost_by_currency (a list) and
// leaves the scalar cost/currency fields unset.
func TestRunDetailUsageMixedCurrenciesExposedSeparately(t *testing.T) {
	f := newFixture(t)
	run, nodeRunID := createMinimalRun(t, f)

	usd, jpy := "USD", "JPY"
	costUSD, costJPY := 1.0, 100.0
	seedAttempt(t, f, nodeRunID, 1, engine.StatusSucceeded, &engine.Usage{InputTokens: 10, OutputTokens: 5, Cost: &costUSD, Currency: &usd})
	seedAttempt(t, f, nodeRunID, 2, engine.StatusSucceeded, &engine.Usage{InputTokens: 10, OutputTokens: 5, Cost: &costJPY, Currency: &jpy})

	view := getRunView(t, f, run.ID)
	usage := view.Run.Usage
	if usage == nil {
		t.Fatal("run detail Usage is nil")
	}
	if usage.Cost != nil {
		t.Fatalf("Cost = %v, want unset (mixed currencies must never be summed into one scalar)", *usage.Cost)
	}
	if usage.Currency != "" {
		t.Fatalf("Currency = %q, want empty when mixed", usage.Currency)
	}
	if len(usage.CostByCurrency) != 2 {
		t.Fatalf("CostByCurrency = %+v, want two entries", usage.CostByCurrency)
	}
	byCurrency := map[string]float64{}
	for _, cc := range usage.CostByCurrency {
		byCurrency[cc.Currency] = cc.Cost
	}
	if byCurrency["USD"] != 1.0 || byCurrency["JPY"] != 100.0 {
		t.Fatalf("byCurrency = %+v, want USD=1 JPY=100", byCurrency)
	}
}

// TestRunDetailUsageCacheRatioComputedHonestly proves GET
// /v1alpha1/runs/{id} renders task t2/ADR 0009's cached_input_tokens,
// reasoning_tokens, and a computed cache_ratio (cached/input, only when
// input_tokens > 0) — and that an attempt reporting tokens with no cache
// telemetry at all contributes nothing to the cached sum, never a
// fabricated zero standing in for "unmeasurable".
func TestRunDetailUsageCacheRatioComputedHonestly(t *testing.T) {
	f := newFixture(t)
	run, nodeRunID := createMinimalRun(t, f)

	cachedTokens, reasoningTokens := int64(9984), int64(500)
	seedAttempt(t, f, nodeRunID, 1, engine.StatusSucceeded,
		&engine.Usage{InputTokens: 13880, OutputTokens: 200, CachedInputTokens: &cachedTokens, ReasoningTokens: &reasoningTokens})
	// A second attempt reports usage but no cache telemetry (e.g. a backend
	// whose contract exposes none) -- must not pollute the sum with a zero.
	seedAttempt(t, f, nodeRunID, 2, engine.StatusSucceeded,
		&engine.Usage{InputTokens: 100, OutputTokens: 20})

	view := getRunView(t, f, run.ID)
	usage := view.Run.Usage
	if usage == nil {
		t.Fatal("run detail Usage is nil")
	}
	if usage.InputTokens != 13980 {
		t.Fatalf("InputTokens = %d, want 13980", usage.InputTokens)
	}
	if usage.CachedInputTokens != 9984 {
		t.Fatalf("CachedInputTokens = %d, want 9984 (only attempt 1 reported any)", usage.CachedInputTokens)
	}
	if usage.ReasoningTokens != 500 {
		t.Fatalf("ReasoningTokens = %d, want 500", usage.ReasoningTokens)
	}
	if usage.CacheRatio == nil {
		t.Fatal("CacheRatio is nil, want computed (input+cached > 0)")
	}
	// Issue #47's real codex numbers: 9984 cache reads beside 13880 uncached
	// input tokens. The whole prompt is 23964 tokens, so 41.7% of it was
	// served from cache -- 9984/13980 would claim 71.4% of a prompt that
	// never existed.
	wantRatio := 9984.0 / 23964.0
	if *usage.CacheRatio < wantRatio-0.0001 || *usage.CacheRatio > wantRatio+0.0001 {
		t.Fatalf("CacheRatio = %v, want ~%v", *usage.CacheRatio, wantRatio)
	}
}

// TestRunDetailUsageCacheRatioOmittedWhenNoInputTokensReported proves
// cache_ratio never renders as a fabricated 0 when no attempt in scope
// reported any prompt tokens at all -- the boundary case a naive
// division would get wrong (0/0).
func TestRunDetailUsageCacheRatioOmittedWhenNoInputTokensReported(t *testing.T) {
	f := newFixture(t)
	run, _ := createMinimalRun(t, f)

	view := getRunView(t, f, run.ID)
	usage := view.Run.Usage
	if usage == nil {
		t.Fatal("run detail Usage is nil")
	}
	if usage.CacheRatio != nil {
		t.Fatalf("CacheRatio = %v, want nil (no attempt reported any prompt tokens)", *usage.CacheRatio)
	}
	if usage.CachedInputTokens != 0 || usage.ReasoningTokens != 0 {
		t.Fatalf("cached/reasoning tokens = %d/%d, want 0/0 (sum of nothing)", usage.CachedInputTokens, usage.ReasoningTokens)
	}
}

// TestNodeRunsListingCarriesPerNodeRunUsage proves GET /v1alpha1/node-runs
// (the cross-run listing) carries each row's own usage rollup: a
// dispatched-and-completed node run's usage reflects what that attempt
// reported, and a never-dispatched node run reports zero attempts either
// way (reported and not_reported both 0), not a missing/null usage block.
func TestNodeRunsListingCarriesPerNodeRunUsage(t *testing.T) {
	f := newFixture(t)
	source := readFixtureWorkflow(t, "minimal.workflow.yaml")
	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	var dispatchedRun, undispatchedRun apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &dispatchedRun)
	requireStatus(t, resp, body, http.StatusCreated)
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &undispatchedRun)
	requireStatus(t, resp, body, http.StatusCreated)

	dispatchedNodeRunID := getRunView(t, f, dispatchedRun.ID).NodeRuns[0].ID
	undispatchedNodeRunID := getRunView(t, f, undispatchedRun.ID).NodeRuns[0].ID

	actorID := f.insertActor("worker")
	claimed := f.claim("worker-1", dispatchedNodeRunID)
	cost := 0.5
	usd := "USD"
	if _, err := f.api.Engine.CompleteAttempt(context.Background(), engine.CompletionRequest{
		WorkID:       claimed.ID,
		WorkerID:     "worker-1",
		FencingToken: claimed.FencingToken,
		Attempt:      int(claimed.Attempt),
		TechStatus:   engine.StatusSucceeded,
		Outcome:      "completed",
		Output:       json.RawMessage(`{}`),
		ActorID:      actorID,
		Usage:        &engine.Usage{InputTokens: 42, OutputTokens: 8, Cost: &cost, Currency: &usd},
	}); err != nil {
		t.Fatalf("complete node run %s: %v", dispatchedNodeRunID, err)
	}

	var list apipkg.NodeRunListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/node-runs?limit=50"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)

	byID := make(map[string]apipkg.NodeRunListItemOut)
	for _, item := range list.Items {
		byID[item.ID] = item
	}

	dispatched, ok := byID[dispatchedNodeRunID]
	if !ok {
		t.Fatalf("dispatched node run %s not found in listing", dispatchedNodeRunID)
	}
	if dispatched.Usage == nil {
		t.Fatal("dispatched node run's Usage is nil")
	}
	if dispatched.Usage.InputTokens != 42 || dispatched.Usage.OutputTokens != 8 {
		t.Fatalf("dispatched tokens = %d/%d, want 42/8", dispatched.Usage.InputTokens, dispatched.Usage.OutputTokens)
	}
	if dispatched.Usage.AttemptsReported != 1 || dispatched.Usage.AttemptsNotReported != 0 {
		t.Fatalf("dispatched reported/not_reported = %d/%d, want 1/0", dispatched.Usage.AttemptsReported, dispatched.Usage.AttemptsNotReported)
	}
	if dispatched.Usage.Cost == nil || *dispatched.Usage.Cost != 0.5 || dispatched.Usage.Currency != "USD" {
		t.Fatalf("dispatched cost/currency = %v/%q, want 0.5/USD", dispatched.Usage.Cost, dispatched.Usage.Currency)
	}

	undispatched, ok := byID[undispatchedNodeRunID]
	if !ok {
		t.Fatalf("undispatched node run %s not found in listing", undispatchedNodeRunID)
	}
	if undispatched.Usage == nil {
		t.Fatal("undispatched node run's Usage is nil (want a present-but-empty rollup, not absent)")
	}
	if undispatched.Usage.AttemptsReported != 0 || undispatched.Usage.AttemptsNotReported != 0 {
		t.Fatalf("undispatched reported/not_reported = %d/%d, want 0/0 (no attempts exist at all)",
			undispatched.Usage.AttemptsReported, undispatched.Usage.AttemptsNotReported)
	}
}

// TestRunsListDoesNotComputeUsage locks in the documented scope boundary:
// GET /v1alpha1/runs (the bulk listing) does not pay for a per-row rollup
// query, unlike run detail and the node-runs listing. Its RunOut.Usage
// stays nil/omitted, distinguishable on the wire from a computed-but-empty
// rollup by the key's absence entirely.
func TestRunsListDoesNotComputeUsage(t *testing.T) {
	f := newFixture(t)
	_, _ = createMinimalRun(t, f)

	var list apipkg.RunListOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	if len(list.Items) == 0 {
		t.Fatal("expected at least one run in the list")
	}
	for _, r := range list.Items {
		if r.Usage != nil {
			t.Fatalf("run %s: Usage = %+v, want nil on the bulk listing", r.ID, r.Usage)
		}
	}
}
