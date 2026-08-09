package ledger_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// fixture builds one run's worth of ledger records covering every record
// type, a supersession, and a committed review, and returns the store's view
// of them.
type fixture struct {
	ledger  *ledger.Ledger
	store   *memStore
	records []ledger.Record

	announcement    ledger.Record
	confirmedClaim  ledger.Record
	rejectedClaim   ledger.Record
	undecidedClaim  ledger.Record
	openAssumption  ledger.Record
	openQuestion    ledger.Record
	answeredQuest   ledger.Record
	readyTask       ledger.Record
	runningTask     ledger.Record
	blockedTask     ledger.Record
	completedTask   ledger.Record
	verifiedTask    ledger.Record
	staleDecision   ledger.Record
	currentDecision ledger.Record
	evidence        ledger.Record
	result          ledger.Record
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	l, store := newTestLedger(t)
	f := &fixture{ledger: l, store: store}

	f.announcement = mustAppend(t, l, ledger.Record{
		RecordType: ledger.RecordAnnouncement,
		RunID:      testRunID,
		Origin:     humanOrigin,
		Authority:  ledger.AuthorityProposed,
		Data:       mustJSON(t, map[string]any{"headline": "Deliver the change", "scope": "one repository"}),
	})

	f.confirmedClaim = mustAppend(t, l, claimRecord(t, "the pinned suite exited zero"))
	f.rejectedClaim = mustAppend(t, l, claimRecord(t, "the docs are complete"))
	f.undecidedClaim = mustAppend(t, l, claimRecord(t, "the migration is reversible"))

	f.openAssumption = mustAppend(t, l, ledger.Record{
		RecordType: ledger.RecordAssumption,
		RunID:      testRunID,
		Origin:     agentOrigin,
		Authority:  ledger.AuthorityProposed,
		Data:       mustJSON(t, map[string]any{"statement": "the pinned image is current"}),
	})
	f.openQuestion = mustAppend(t, l, ledger.Record{
		RecordType: ledger.RecordQuestion,
		RunID:      testRunID,
		Origin:     agentOrigin,
		Authority:  ledger.AuthorityProposed,
		Data:       mustJSON(t, map[string]any{"question": "which suite gates release?", "blocking": true}),
	})
	f.answeredQuest = mustAppend(t, l, ledger.Record{
		RecordType: ledger.RecordQuestion,
		RunID:      testRunID,
		Origin:     agentOrigin,
		Authority:  ledger.AuthorityProposed,
		Data:       mustJSON(t, map[string]any{"question": "which runner?", "answer": "headspace"}),
	})

	f.readyTask = mustAppend(t, l, taskRecord(t, "run the suite", "ready", "unverified"))
	f.runningTask = mustAppend(t, l, taskRecord(t, "build the image", "running", "unverified"))
	f.blockedTask = mustAppend(t, l, taskRecord(t, "publish", "blocked", "unverified"))
	f.completedTask = mustAppend(t, l, taskRecord(t, "write the migration", "completed", "evidence_attached"))
	f.verifiedTask = mustAppend(t, l, taskRecord(t, "review the ADR", "completed", "verified"))

	f.staleDecision = mustAppend(t, l, ledger.Record{
		RecordType: ledger.RecordDecision,
		RunID:      testRunID,
		Origin:     humanOrigin,
		Authority:  ledger.AuthorityProposed,
		Data:       mustJSON(t, map[string]any{"question": "which runner?", "selected": "docker"}),
	})

	f.evidence = mustAppend(t, l, ledger.Record{
		RecordType: ledger.RecordEvidence,
		RunID:      testRunID,
		Origin:     runnerOrigin,
		Authority:  ledger.AuthorityObserved,
		SubjectRef: ledger.NullableID(f.completedTask.ID),
		Data: mustJSON(t, map[string]any{
			"collection_method": "runner_wait_status",
			"completeness":      "partial",
			"measurements":      map[string]any{"exit_code": 0},
		}),
		ProvenanceRefs: []string{f.confirmedClaim.ID},
	}, ledger.WithRunnerManifest(ledger.RunnerManifest{
		ActorID:          testRunner,
		ObservableFields: []string{"/collection_method", "/completeness", "/measurements"},
	}))

	f.result = mustAppend(t, l, ledger.Record{
		RecordType: ledger.RecordResult,
		RunID:      testRunID,
		Origin:     agentOrigin,
		Authority:  ledger.AuthorityProposed,
		SubjectRef: ledger.NullableID(f.completedTask.ID),
		Data:       mustJSON(t, map[string]any{"outcome": "completed"}),
	})

	// A decision that replaces an earlier one.
	corrected, err := l.AppendSuperseding(ctx, ledger.Record{
		RecordType: ledger.RecordDecision,
		RunID:      testRunID,
		Origin:     humanOrigin,
		Authority:  ledger.AuthorityProposed,
		Data:       mustJSON(t, map[string]any{"question": "which runner?", "selected": "headspace"}),
	}, f.staleDecision.ID)
	if err != nil {
		t.Fatalf("supersede the stale decision: %v", err)
	}
	f.currentDecision = corrected

	// One committed review: confirm one claim, reject another.
	version := mustVersion(t, l, testRunID)
	req, err := l.CreateReviewRequest(ctx, testRunID,
		[]string{f.confirmedClaim.ID, f.rejectedClaim.ID}, version, ledger.WithReviewer(testHuman))
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}
	if _, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{
		f.confirmedClaim.ID: ledger.VerdictConfirm,
		f.rejectedClaim.ID:  ledger.VerdictReject,
	}, version); err != nil {
		t.Fatalf("CommitReview: %v", err)
	}

	f.records = store.all()
	return f
}

func TestProjections(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name    string
		project func([]ledger.Record) (ledger.Projection, error)
		want    []string
	}{
		{
			name:    "current scope is the announcements in force",
			project: ledger.CurrentScope,
			want:    []string{f.announcement.ID},
		},
		{
			name:    "confirmed claims come from live review records",
			project: ledger.ConfirmedClaims,
			want:    []string{f.confirmedClaim.ID},
		},
		{
			name:    "open assumptions and questions exclude decided and answered ones",
			project: ledger.OpenAssumptionsAndQuestions,
			want:    []string{f.openAssumption.ID, f.openQuestion.ID},
		},
		{
			name:    "ready tasks",
			project: ledger.ReadyTasks,
			want:    []string{f.readyTask.ID},
		},
		{
			name:    "active tasks are claimed or running, never blocked",
			project: ledger.ActiveTasks,
			want:    []string{f.runningTask.ID},
		},
		{
			name:    "verification queue is completed-but-unverified work and undecided results",
			project: ledger.VerificationQueue,
			want:    []string{f.completedTask.ID, f.result.ID},
		},
		{
			name:    "decision history holds the live decisions only",
			project: ledger.DecisionHistory,
			want:    []string{f.currentDecision.ID},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := tc.project(f.records)
			if err != nil {
				t.Fatalf("projection: %v", err)
			}
			assertIDs(t, p.Items, tc.want)
			if p.Digest == "" {
				t.Fatal("projection carries no digest")
			}
		})
	}
}

func TestEvidenceForSubject(t *testing.T) {
	f := newFixture(t)

	t.Run("by subject", func(t *testing.T) {
		p, err := ledger.EvidenceForSubject(f.records, f.completedTask.ID)
		if err != nil {
			t.Fatalf("EvidenceForSubject: %v", err)
		}
		assertIDs(t, p.Items, []string{f.evidence.ID})
		if p.Subject != f.completedTask.ID {
			t.Fatalf("subject = %q, want %q", p.Subject, f.completedTask.ID)
		}
	})

	t.Run("by provenance", func(t *testing.T) {
		p, err := ledger.EvidenceForSubject(f.records, f.confirmedClaim.ID)
		if err != nil {
			t.Fatalf("EvidenceForSubject: %v", err)
		}
		assertIDs(t, p.Items, []string{f.evidence.ID})
	})

	t.Run("nothing for an unrelated reference", func(t *testing.T) {
		p, err := ledger.EvidenceForSubject(f.records, f.readyTask.ID)
		if err != nil {
			t.Fatalf("EvidenceForSubject: %v", err)
		}
		assertIDs(t, p.Items, nil)
	})
}

func TestDeliverySummaryCountsHonestly(t *testing.T) {
	f := newFixture(t)

	p, err := ledger.DeliverySummary(f.records)
	if err != nil {
		t.Fatalf("DeliverySummary: %v", err)
	}
	got := p.Summary
	if got == nil {
		t.Fatal("delivery summary projection carries no summary")
	}

	if got.RunID != testRunID {
		t.Fatalf("run_id = %q, want %q", got.RunID, testRunID)
	}
	if got.SupersededRecords != 1 {
		t.Fatalf("superseded_records = %d, want 1", got.SupersededRecords)
	}
	if got.ConfirmedClaims != 1 || got.RejectedClaims != 1 || got.UndecidedClaims != 1 {
		t.Fatalf("claims confirmed/rejected/undecided = %d/%d/%d, want 1/1/1",
			got.ConfirmedClaims, got.RejectedClaims, got.UndecidedClaims)
	}
	if got.OpenAssumptions != 1 {
		t.Fatalf("open_assumptions = %d, want 1", got.OpenAssumptions)
	}
	if got.OpenQuestions != 1 || got.BlockingOpenQuestions != 1 {
		t.Fatalf("open/blocking questions = %d/%d, want 1/1", got.OpenQuestions, got.BlockingOpenQuestions)
	}
	if got.TasksByStatus["ready"] != 1 || got.TasksByStatus["running"] != 1 ||
		got.TasksByStatus["blocked"] != 1 || got.TasksByStatus["completed"] != 2 {
		t.Fatalf("tasks_by_status = %v", got.TasksByStatus)
	}
	if got.CompletedUnverifiedTasks != 1 {
		t.Fatalf("completed_unverified_tasks = %d, want 1 — the gap between an actor's completion and verification is the point",
			got.CompletedUnverifiedTasks)
	}
	if got.EvidenceRecords != 1 || got.EvidenceByCompleteness["partial"] != 1 {
		t.Fatalf("evidence = %d, by completeness = %v; partial coverage must stay visible",
			got.EvidenceRecords, got.EvidenceByCompleteness)
	}
	if got.ResultsAwaitingReview != 1 {
		t.Fatalf("results_awaiting_review = %d, want 1", got.ResultsAwaitingReview)
	}
}

// TestEvidenceWithoutDeclaredCompletenessIsCountedUnstated proves the summary
// never reads an omitted field as "complete".
func TestEvidenceWithoutDeclaredCompletenessIsCountedUnstated(t *testing.T) {
	l, store := newTestLedger(t)

	mustAppend(t, l, ledger.Record{
		RecordType: ledger.RecordEvidence,
		RunID:      testRunID,
		Origin:     runnerOrigin,
		Authority:  ledger.AuthorityObserved,
		Data:       mustJSON(t, map[string]any{"collection_method": "runner_wait_status"}),
	}, ledger.WithRunnerManifest(ledger.RunnerManifest{
		ActorID:          testRunner,
		ObservableFields: []string{"/collection_method"},
	}))

	p, err := ledger.DeliverySummary(store.all())
	if err != nil {
		t.Fatalf("DeliverySummary: %v", err)
	}
	if p.Summary.EvidenceByCompleteness["unstated"] != 1 {
		t.Fatalf("evidence_by_completeness = %v, want one 'unstated'", p.Summary.EvidenceByCompleteness)
	}
	if p.Summary.EvidenceByCompleteness["complete"] != 0 {
		t.Fatal("omitting completeness was read as complete")
	}
}

// TestProjectionDigestIgnoresStorageOrder is the determinism promise: a
// projection is a function of the record set, not of the order a store
// happened to return it in.
func TestProjectionDigestIgnoresStorageOrder(t *testing.T) {
	f := newFixture(t)

	reversed := make([]ledger.Record, len(f.records))
	for i, rec := range f.records {
		reversed[len(f.records)-1-i] = rec
	}

	for _, kind := range ledger.ProjectionKinds() {
		t.Run(string(kind), func(t *testing.T) {
			forward, err := ledger.Project(f.records, kind, f.completedTask.ID)
			if err != nil {
				t.Fatalf("project forward: %v", err)
			}
			backward, err := ledger.Project(reversed, kind, f.completedTask.ID)
			if err != nil {
				t.Fatalf("project reversed: %v", err)
			}
			if forward.Digest != backward.Digest {
				t.Fatalf("digest depends on storage order: %s != %s", forward.Digest, backward.Digest)
			}
		})
	}
}

// TestProjectionDigestSurvivesSerialisation proves the digest travels with
// the projection and can be re-derived from it, following the same
// content-minus-digest rule a record does.
func TestProjectionDigestSurvivesSerialisation(t *testing.T) {
	f := newFixture(t)

	p, err := ledger.DeliverySummary(f.records)
	if err != nil {
		t.Fatalf("DeliverySummary: %v", err)
	}
	if err := p.VerifyDigest(); err != nil {
		t.Fatalf("VerifyDigest: %v", err)
	}

	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal projection: %v", err)
	}
	var decoded ledger.Projection
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode projection: %v", err)
	}
	if decoded.Digest != p.Digest {
		t.Fatalf("digest = %q after a round trip, want %q", decoded.Digest, p.Digest)
	}
	if err := decoded.VerifyDigest(); err != nil {
		t.Fatalf("VerifyDigest after a round trip: %v", err)
	}

	tampered := decoded
	tampered.Summary.ConfirmedClaims += 7
	if err := tampered.VerifyDigest(); err == nil {
		t.Fatal("VerifyDigest accepted a projection whose counts were edited")
	}
}

func TestProjectRejectsAnUnknownKind(t *testing.T) {
	if _, err := ledger.Project(nil, "prophecy", ""); err == nil {
		t.Fatal("Project accepted an unknown projection kind")
	}
}

// TestProjectRunReadsAndProjects covers the store-backed convenience path.
func TestProjectRunReadsAndProjects(t *testing.T) {
	f := newFixture(t)

	p, err := f.ledger.ProjectRun(context.Background(), testRunID, ledger.KindConfirmedClaims, "")
	if err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}
	assertIDs(t, p.Items, []string{f.confirmedClaim.ID})
}

func assertIDs(t *testing.T, records []ledger.Record, want []string) {
	t.Helper()
	got := ids(records)
	if len(got) != len(want) {
		t.Fatalf("projected %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("projected %v, want %v", got, want)
		}
	}
}
