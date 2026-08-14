package api_test

import (
	"context"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The attempt read surface (task t26, issue #49, spec claim c32 / honesty
// h21): GET /v1alpha1/runs/{id} must return the preserve-on-failure branch
// task t25's bridges mint and migrations/0025 persists, so the run detail
// page can render it (web/src/components/NodeDetailPanel.tsx).

// seedAttemptWithPreserve is seedAttempt's twin (usage_test.go) for a
// preserve block, writing directly through InsertAttempt the same way.
func seedAttemptWithPreserve(t *testing.T, f *fixture, nodeRunID string, n int, status engine.TechStatus, preserve *engine.Preserve) {
	t.Helper()
	es, err := storepg.NewEngineStore(f.store, f.nsID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	err = es.InTx(context.Background(), func(ctx context.Context, tx engine.Tx) error {
		return tx.InsertAttempt(ctx, engine.Attempt{
			ID: store.NewULID(), NodeRunID: nodeRunID, Number: n, Status: status, Preserve: preserve,
		})
	})
	if err != nil {
		t.Fatalf("seed attempt %d on node run %s: %v", n, nodeRunID, err)
	}
}

// TestRunDetailExposesPreserveBranch is the acceptance test the plan asks
// for: "a failed run's detail page links the preserve branch; the DB row
// carries the branch name (store + web test)" — this is the store-through-
// API half. A failed attempt whose bridge committed and pushed a preserve
// branch renders that branch, pushed state, and remote on the attempt the
// run detail page reads.
func TestRunDetailExposesPreserveBranch(t *testing.T) {
	f := newFixture(t)
	run, nodeRunID := createMinimalRun(t, f)

	seedAttemptWithPreserve(t, f, nodeRunID, 1, engine.StatusFailed, &engine.Preserve{
		Branch: "preserve/run-01J-att-01K-20260813T120000Z-ab12cd",
		Pushed: true,
		Remote: "origin",
	})

	view := getRunView(t, f, run.ID)
	if len(view.NodeRuns) != 1 || len(view.NodeRuns[0].Attempts) != 1 {
		t.Fatalf("run view = %+v, want exactly 1 node run with 1 attempt", view)
	}
	attempt := view.NodeRuns[0].Attempts[0]
	if attempt.PreserveBranch != "preserve/run-01J-att-01K-20260813T120000Z-ab12cd" {
		t.Errorf("PreserveBranch = %q, want the minted branch name", attempt.PreserveBranch)
	}
	if attempt.PreservePushed == nil || !*attempt.PreservePushed {
		t.Errorf("PreservePushed = %v, want true", attempt.PreservePushed)
	}
	if attempt.PreserveRemote != "origin" {
		t.Errorf("PreserveRemote = %q, want origin", attempt.PreserveRemote)
	}
}

// TestRunDetailExposesLocalOnlyPreserveBranch pins the honest local-only
// rendering: PreservePushed is explicitly false (not omitted, not nil) so a
// reader — and the web client — can distinguish "pushed, go look at the
// remote" from "local-only, this exists only on the bridge host".
func TestRunDetailExposesLocalOnlyPreserveBranch(t *testing.T) {
	f := newFixture(t)
	run, nodeRunID := createMinimalRun(t, f)

	seedAttemptWithPreserve(t, f, nodeRunID, 1, engine.StatusFailed, &engine.Preserve{
		Branch: "preserve/run-01J-att-01L-20260813T120500Z-ef34gh",
		Pushed: false,
		Remote: "origin",
	})

	view := getRunView(t, f, run.ID)
	attempt := view.NodeRuns[0].Attempts[0]
	if attempt.PreserveBranch == "" {
		t.Fatal("PreserveBranch is empty, want the minted branch name")
	}
	if attempt.PreservePushed == nil {
		t.Fatal("PreservePushed = nil, want an explicit false for a local-only branch")
	}
	if *attempt.PreservePushed {
		t.Error("PreservePushed = true, want false: this branch was never pushed")
	}
}

// TestRunDetailOmitsPreserveWhenAttemptReportedNone proves the far more
// common case — a successful attempt, or a failed one with nothing to
// preserve — carries no preserve fields at all, never a fabricated empty
// branch.
func TestRunDetailOmitsPreserveWhenAttemptReportedNone(t *testing.T) {
	f := newFixture(t)
	run, nodeRunID := createMinimalRun(t, f)

	seedAttemptWithPreserve(t, f, nodeRunID, 1, engine.StatusSucceeded, nil)

	view := getRunView(t, f, run.ID)
	attempt := view.NodeRuns[0].Attempts[0]
	if attempt.PreserveBranch != "" {
		t.Errorf("PreserveBranch = %q, want empty for an attempt that reported none", attempt.PreserveBranch)
	}
	if attempt.PreservePushed != nil {
		t.Errorf("PreservePushed = %v, want nil for an attempt that reported none", *attempt.PreservePushed)
	}
	if attempt.PreserveRemote != "" {
		t.Errorf("PreserveRemote = %q, want empty for an attempt that reported none", attempt.PreserveRemote)
	}
}
