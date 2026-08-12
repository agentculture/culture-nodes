package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// createCategorizedRun mirrors usage_test.go's createMinimalRun but sets an
// optional category (task t3) through createRunWithMetadataReq
// (runmetadata_test.go), so an actor-stats test can control exactly which
// runs.category bucket a run's node run/attempts land in. minimal.workflow.yaml
// carries exactly one node, so the returned node run id is the run's only
// one -- the same single-node-run shape createMinimalRun relies on.
func createCategorizedRun(t *testing.T, f *fixture, category string) (apipkg.RunOut, string) {
	t.Helper()
	source := readFixtureWorkflow(t, "minimal.workflow.yaml")
	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	// Publishing the same source twice is idempotent-by-digest (200, not
	// 201, the second time — see handlePublishWorkflow's doc comment);
	// this helper is called more than once per test with the identical
	// fixture source, so either status is the correct, already-durable
	// outcome.
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("publish minimal.workflow.yaml: status = %d, want 200 or 201; body = %s", resp.StatusCode, body)
	}

	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunWithMetadataReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`), Category: category}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	view := getRunView(t, f, run.ID)
	if len(view.NodeRuns) != 1 {
		t.Fatalf("run %s: got %d node runs, want 1", run.ID, len(view.NodeRuns))
	}
	return run, view.NodeRuns[0].ID
}

// seedActorAttempt writes one attempt directly through
// postgres.EngineStore's InsertAttempt (the same bypass usage_test.go's
// seedAttempt uses), additionally naming the dispatched actor and explicit
// started_at/completed_at so a test can produce a known attempt duration —
// attempts.completed_at is otherwise defaulted to "now" by InsertAttempt
// (engine_store.go's tsOrNow), which would make duration assertions flaky.
func seedActorAttempt(t *testing.T, f *fixture, nodeRunID, actorID string, number int, status engine.TechStatus, startedAt, completedAt time.Time, usage *engine.Usage) {
	t.Helper()
	es, err := storepg.NewEngineStore(f.store, f.nsID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	err = es.InTx(context.Background(), func(ctx context.Context, tx engine.Tx) error {
		return tx.InsertAttempt(ctx, engine.Attempt{
			ID: store.NewULID(), NodeRunID: nodeRunID, Number: number, ActorID: actorID,
			Status: status, StartedAt: startedAt, CompletedAt: completedAt, Usage: usage,
		})
	})
	if err != nil {
		t.Fatalf("seed attempt %d on node run %s: %v", number, nodeRunID, err)
	}
}

// setNodeRunOutcome writes node_runs.status/outcome directly, bypassing the
// real §12.5 completion transaction -- this package's actor-stats tests
// only need the resulting row shape (attempts -> node_runs -> runs joins),
// not a genuinely driven workflow transition; TestRunLifecycle-style tests
// elsewhere in this package already exercise the real transition path. An
// empty outcome writes SQL NULL, matching "no domain outcome recorded" the
// same way runMetadata's NULLIF convention does elsewhere in this package.
func setNodeRunOutcome(t *testing.T, f *fixture, nodeRunID, status, outcome string) {
	t.Helper()
	var outcomeArg any
	if outcome != "" {
		outcomeArg = outcome
	}
	_, err := f.store.Pool().Exec(context.Background(),
		`UPDATE node_runs SET status = $2, outcome = $3, updated_at = now(), completed_at = now() WHERE id = $1`,
		nodeRunID, status, outcomeArg)
	if err != nil {
		t.Fatalf("set node run %s status/outcome: %v", nodeRunID, err)
	}
}

func getActorStats(t *testing.T, f *fixture, actorID string) apipkg.ActorStatsOut {
	t.Helper()
	var stats apipkg.ActorStatsOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors/"+actorID+"/stats"), nil, &stats)
	requireStatus(t, resp, body, http.StatusOK)
	return stats
}

func findCategoryBucket(stats apipkg.ActorStatsOut, category string) (apipkg.ActorCategoryBucketOut, bool) {
	for _, c := range stats.Categories {
		if c.Category == category {
			return c, true
		}
	}
	return apipkg.ActorCategoryBucketOut{}, false
}

// TestActorsListAndGetActor proves GET /v1alpha1/actors and GET
// /v1alpha1/actors/{id} render the registered rows the actors table
// actually holds -- the read surface that replaces nodes-op's ssh+psql
// actors verb (task t15 acceptance criterion 1).
func TestActorsListAndGetActor(t *testing.T) {
	f := newFixture(t)
	id1 := f.insertActor("alpha")
	id2 := f.insertActor("beta")

	var list apipkg.ActorListOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)

	found := map[string]apipkg.ActorOut{}
	for _, a := range list.Items {
		found[a.ID] = a
	}
	for _, id := range []string{id1, id2} {
		a, ok := found[id]
		if !ok {
			t.Fatalf("list actors = %+v, want %s present", list.Items, id)
		}
		if a.Kind != "agent" || a.Protocol != "http" {
			t.Errorf("actor %s kind/protocol = %s/%s, want agent/http", id, a.Kind, a.Protocol)
		}
		if a.CreatedAt.IsZero() {
			t.Errorf("actor %s created_at is zero", id)
		}
	}

	var single apipkg.ActorOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors/"+id1), nil, &single)
	requireStatus(t, resp, body, http.StatusOK)
	if single.ID != id1 {
		t.Fatalf("get actor id = %q, want %q", single.ID, id1)
	}
}

// TestGetActorNotFound and TestGetActorStatsNotFound prove both single-actor
// endpoints 404 in the documented Error shape for an unknown id.
func TestGetActorNotFound(t *testing.T) {
	f := newFixture(t)
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors/does-not-exist"), nil, nil)
	requireStatus(t, resp, body, http.StatusNotFound)
	decodeAPIError(t, body)
}

func TestGetActorStatsNotFound(t *testing.T) {
	f := newFixture(t)
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors/does-not-exist/stats"), nil, nil)
	requireStatus(t, resp, body, http.StatusNotFound)
	decodeAPIError(t, body)
}

// TestActorStatsPresentButEmptyForIdleActor is the "empty DB" acceptance
// scenario: a registered actor with zero runs, zero ledger records, zero
// attempts anywhere in this (freshly provisioned, otherwise empty)
// namespace still gets 200 with a present-but-entirely-empty payload --
// never absent fields standing in for "not computed", and never a
// fabricated zero where "no data" must be structural (task t15 acceptance
// criterion 3; the same convention RunOut.Usage already uses for a
// freshly created run).
func TestActorStatsPresentButEmptyForIdleActor(t *testing.T) {
	f := newFixture(t)
	actorID := f.insertActor("idle")

	stats := getActorStats(t, f, actorID)
	if stats.ActorID != actorID {
		t.Fatalf("actor_id = %q, want %q", stats.ActorID, actorID)
	}
	if len(stats.Categories) != 0 {
		t.Fatalf("categories = %+v, want none (idle actor touched no category)", stats.Categories)
	}
	total := stats.Total
	if len(total.RunsByOutcome) != 0 {
		t.Errorf("runs_by_outcome = %+v, want empty", total.RunsByOutcome)
	}
	if len(total.ClaimsByAuthority) != 0 {
		t.Errorf("claims_by_authority = %+v, want empty", total.ClaimsByAuthority)
	}
	if total.RetryBurn.Attempts != 0 || total.RetryBurn.CompletedNodeRuns != 0 {
		t.Errorf("retry_burn = %+v, want 0/0", total.RetryBurn)
	}
	if total.RetryBurn.AttemptsPerCompletion != nil {
		t.Errorf("attempts_per_completion = %v, want nil (never a fabricated division by zero)", *total.RetryBurn.AttemptsPerCompletion)
	}
	if total.DurationPercentiles != nil {
		t.Errorf("duration_percentiles = %+v, want nil (no completed attempt)", total.DurationPercentiles)
	}
	if total.Usage == nil {
		t.Fatal("usage is nil, want a present-but-empty rollup")
	}
	if total.Usage.AttemptsReported != 0 || total.Usage.AttemptsNotReported != 0 {
		t.Errorf("usage attempts reported/not_reported = %d/%d, want 0/0", total.Usage.AttemptsReported, total.Usage.AttemptsNotReported)
	}
	if total.Grades.Proposed.Count != 0 || total.Grades.Confirmed.Count != 0 {
		t.Errorf("grades = %+v, want zero counts", total.Grades)
	}
	if total.Grades.Proposed.MeanRating != nil || total.Grades.Confirmed.MeanRating != nil {
		t.Errorf("grade mean ratings = %+v, want nil (an average of zero opinions is undefined, not 0)", total.Grades)
	}
}

// TestActorStatsMixedOutcomeRunsAcrossCategoriesAndRetryBurn is task t15's
// central aggregation proof: one actor dispatched across two categories
// ("review": two node runs, one attempt then two attempts; "docs": one
// failed node run) plus one uncategorized run, asserting runs_by_outcome,
// retry burn (attempts/completed_node_runs), and duration percentiles are
// each computed per category AND as an independent Total -- never a
// client-side sum of the category rows (percentiles are not additive).
func TestActorStatsMixedOutcomeRunsAcrossCategoriesAndRetryBurn(t *testing.T) {
	f := newFixture(t)
	actorID := f.insertActor("worker")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// "review" category, run 1: one succeeded attempt, node run completes
	// with domain outcome "approved". Duration: 10s.
	_, nr1 := createCategorizedRun(t, f, "review")
	seedActorAttempt(t, f, nr1, actorID, 1, engine.StatusSucceeded, t0, t0.Add(10*time.Second), nil)
	setNodeRunOutcome(t, f, nr1, "completed", "approved")

	// "review" category, run 2: a failed attempt (5s) then a succeeded
	// retry (15s) by the same actor, node run completes "approved" too --
	// so the "review" bucket's runs_by_outcome collapses both node runs
	// into one (completed, approved, count 2), while retry burn sees 3
	// attempts against 2 completed node runs.
	_, nr2 := createCategorizedRun(t, f, "review")
	seedActorAttempt(t, f, nr2, actorID, 1, engine.StatusFailed, t0, t0.Add(5*time.Second), nil)
	seedActorAttempt(t, f, nr2, actorID, 2, engine.StatusSucceeded, t0, t0.Add(15*time.Second), nil)
	setNodeRunOutcome(t, f, nr2, "completed", "approved")

	// "docs" category: one failed attempt, node run ends failed with no
	// domain outcome at all -- proving status and outcome are reported
	// side by side, never conflated (a failed node run is not "the domain
	// outcome changes_required", it has none).
	_, nr3 := createCategorizedRun(t, f, "docs")
	seedActorAttempt(t, f, nr3, actorID, 1, engine.StatusFailed, t0, t0.Add(2*time.Second), nil)
	setNodeRunOutcome(t, f, nr3, "failed", "")

	// Uncategorized: one succeeded attempt, node run completes "approved".
	_, nr4 := createCategorizedRun(t, f, "")
	seedActorAttempt(t, f, nr4, actorID, 1, engine.StatusSucceeded, t0, t0.Add(1*time.Second), nil)
	setNodeRunOutcome(t, f, nr4, "completed", "approved")

	stats := getActorStats(t, f, actorID)

	// --- "review" category ---
	review, ok := findCategoryBucket(stats, "review")
	if !ok {
		t.Fatalf("no 'review' category bucket in %+v", stats.Categories)
	}
	if len(review.RunsByOutcome) != 1 || review.RunsByOutcome[0].Status != "completed" ||
		review.RunsByOutcome[0].Outcome != "approved" || review.RunsByOutcome[0].Count != 2 {
		t.Fatalf("review runs_by_outcome = %+v, want [{completed approved 2}]", review.RunsByOutcome)
	}
	if review.RetryBurn.Attempts != 3 || review.RetryBurn.CompletedNodeRuns != 2 {
		t.Fatalf("review retry_burn = %+v, want attempts=3 completed_node_runs=2", review.RetryBurn)
	}
	if review.RetryBurn.AttemptsPerCompletion == nil || *review.RetryBurn.AttemptsPerCompletion < 1.49 || *review.RetryBurn.AttemptsPerCompletion > 1.51 {
		t.Fatalf("review attempts_per_completion = %v, want ~1.5", review.RetryBurn.AttemptsPerCompletion)
	}
	if review.DurationPercentiles == nil || review.DurationPercentiles.Count != 3 {
		t.Fatalf("review duration_percentiles = %+v, want count=3", review.DurationPercentiles)
	}
	if p50 := review.DurationPercentiles.P50Seconds; p50 < 9.99 || p50 > 10.01 {
		t.Errorf("review p50 = %v, want ~10 (durations 5,10,15)", p50)
	}

	// --- "docs" category ---
	docs, ok := findCategoryBucket(stats, "docs")
	if !ok {
		t.Fatalf("no 'docs' category bucket in %+v", stats.Categories)
	}
	if len(docs.RunsByOutcome) != 1 || docs.RunsByOutcome[0].Status != "failed" || docs.RunsByOutcome[0].Outcome != "" {
		t.Fatalf("docs runs_by_outcome = %+v, want [{failed \"\" 1}]", docs.RunsByOutcome)
	}
	if docs.RetryBurn.Attempts != 1 || docs.RetryBurn.CompletedNodeRuns != 0 {
		t.Fatalf("docs retry_burn = %+v, want attempts=1 completed_node_runs=0", docs.RetryBurn)
	}
	if docs.RetryBurn.AttemptsPerCompletion != nil {
		t.Errorf("docs attempts_per_completion = %v, want nil (zero completions, never a fabricated division)", *docs.RetryBurn.AttemptsPerCompletion)
	}

	// --- uncategorized ("") ---
	uncategorized, ok := findCategoryBucket(stats, "")
	if !ok {
		t.Fatalf("no uncategorized (\"\") bucket in %+v", stats.Categories)
	}
	if len(uncategorized.RunsByOutcome) != 1 || uncategorized.RunsByOutcome[0].Count != 1 {
		t.Fatalf("uncategorized runs_by_outcome = %+v, want one row of count 1", uncategorized.RunsByOutcome)
	}

	// --- Total: 5 attempts (3+1+1), 4 completed node runs (2+0+1... wait
	// docs contributes 0 completed node runs) -- 3(review)+1(docs)+1
	// (uncategorized) attempts = 5; 2(review)+0(docs)+1(uncategorized)
	// completed node runs = 3.
	if stats.Total.RetryBurn.Attempts != 5 {
		t.Errorf("total attempts = %d, want 5", stats.Total.RetryBurn.Attempts)
	}
	if stats.Total.RetryBurn.CompletedNodeRuns != 3 {
		t.Errorf("total completed_node_runs = %d, want 3", stats.Total.RetryBurn.CompletedNodeRuns)
	}
	if stats.Total.DurationPercentiles == nil || stats.Total.DurationPercentiles.Count != 5 {
		t.Fatalf("total duration_percentiles = %+v, want count=5", stats.Total.DurationPercentiles)
	}
}

// TestActorStatsGradesProposedVsConfirmedNeverBlended proves grade.schema.json
// aggregates (task t14/t15) keep an agent's proposed opinions and a
// human's confirmed opinion as two separate counts and means -- never
// averaged together into one number (task t15 acceptance criterion 2).
func TestActorStatsGradesProposedVsConfirmedNeverBlended(t *testing.T) {
	f := newFixture(t)
	evaluatedActor := f.insertActor("evaluated")
	graderAgent := f.insertActor("grader-agent")
	humanGrader := f.insertActor("human-grader")

	run, _ := createCategorizedRun(t, f, "review")

	gradeData := func(rating int) json.RawMessage {
		b, _ := json.Marshal(map[string]any{
			"rating":             rating,
			"rationale":          "test grade",
			"evaluated_actor_id": evaluatedActor,
		})
		return b
	}

	// Two proposed (agent-origin) grades: ratings 4 and 2, mean 3.
	for _, rating := range []int{4, 2} {
		if _, err := f.api.Ledger.Append(context.Background(), ledger.Record{
			RecordType: ledger.RecordGrade,
			RunID:      run.ID,
			Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: graderAgent},
			Authority:  ledger.AuthorityProposed,
			Data:       gradeData(rating),
		}); err != nil {
			t.Fatalf("append proposed grade %d: %v", rating, err)
		}
	}

	// One confirmed (human-origin, direct write -- see
	// internal/ledger/authority.go's checkHumanAuthority) grade: rating 5.
	if _, err := f.api.Ledger.Append(context.Background(), ledger.Record{
		RecordType: ledger.RecordGrade,
		RunID:      run.ID,
		Origin:     ledger.Origin{Kind: ledger.OriginHuman, ActorID: humanGrader},
		Authority:  ledger.AuthorityConfirmed,
		Data:       gradeData(5),
	}); err != nil {
		t.Fatalf("append confirmed grade: %v", err)
	}

	stats := getActorStats(t, f, evaluatedActor)

	if stats.Total.Grades.Proposed.Count != 2 {
		t.Fatalf("proposed count = %d, want 2", stats.Total.Grades.Proposed.Count)
	}
	if mean := stats.Total.Grades.Proposed.MeanRating; mean == nil || *mean < 2.99 || *mean > 3.01 {
		t.Fatalf("proposed mean_rating = %v, want ~3", mean)
	}
	if stats.Total.Grades.Confirmed.Count != 1 {
		t.Fatalf("confirmed count = %d, want 1", stats.Total.Grades.Confirmed.Count)
	}
	if mean := stats.Total.Grades.Confirmed.MeanRating; mean == nil || *mean < 4.99 || *mean > 5.01 {
		t.Fatalf("confirmed mean_rating = %v, want ~5", mean)
	}

	// The same split holds inside the "review" category bucket, since
	// every grade above referenced the one "review"-tagged run.
	review, ok := findCategoryBucket(stats, "review")
	if !ok {
		t.Fatalf("no 'review' category bucket in %+v", stats.Categories)
	}
	if review.Grades.Proposed.Count != 2 || review.Grades.Confirmed.Count != 1 {
		t.Fatalf("review grades = %+v, want proposed=2 confirmed=1", review.Grades)
	}
}

// TestActorStatsUsageNeverSumsAcrossCurrencies proves the actor-level usage
// rollup keeps task t2's honest-currency rule: attempts that priced their
// work in different currencies are never collapsed into one summed number
// (task t15 acceptance criterion "no-cross-currency rule").
func TestActorStatsUsageNeverSumsAcrossCurrencies(t *testing.T) {
	f := newFixture(t)
	actorID := f.insertActor("worker")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	_, nr := createCategorizedRun(t, f, "")

	usd, eur := "USD", "EUR"
	usdCost, eurCost := 1.5, 2.5
	seedActorAttempt(t, f, nr, actorID, 1, engine.StatusSucceeded, t0, t0.Add(time.Second),
		&engine.Usage{InputTokens: 10, OutputTokens: 5, Cost: &usdCost, Currency: &usd})
	seedActorAttempt(t, f, nr, actorID, 2, engine.StatusFailed, t0, t0.Add(time.Second),
		&engine.Usage{InputTokens: 20, OutputTokens: 8, Cost: &eurCost, Currency: &eur})
	// A third attempt that reported no usage at all -- must count toward
	// attempts_not_reported, never fold in as a zero-cost, zero-token row.
	seedActorAttempt(t, f, nr, actorID, 3, engine.StatusFailed, t0, t0.Add(time.Second), nil)

	stats := getActorStats(t, f, actorID)
	usage := stats.Total.Usage
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.InputTokens != 30 || usage.OutputTokens != 13 {
		t.Fatalf("tokens = %d/%d, want 30/13 (both reporting attempts summed regardless of currency)", usage.InputTokens, usage.OutputTokens)
	}
	if usage.AttemptsReported != 2 || usage.AttemptsNotReported != 1 {
		t.Fatalf("attempts reported/not_reported = %d/%d, want 2/1", usage.AttemptsReported, usage.AttemptsNotReported)
	}
	if usage.Cost != nil || usage.Currency != "" {
		t.Fatalf("cost/currency = %v/%q, want both unset when currencies differ", usage.Cost, usage.Currency)
	}
	if len(usage.CostByCurrency) != 2 {
		t.Fatalf("cost_by_currency = %+v, want exactly 2 entries (USD, EUR), never summed together", usage.CostByCurrency)
	}
	seen := map[string]float64{}
	for _, cc := range usage.CostByCurrency {
		seen[cc.Currency] = cc.Cost
	}
	if c := seen["USD"]; c < 1.49 || c > 1.51 {
		t.Errorf("USD cost = %v, want ~1.5", c)
	}
	if c := seen["EUR"]; c < 2.49 || c > 2.51 {
		t.Errorf("EUR cost = %v, want ~2.5", c)
	}
}
