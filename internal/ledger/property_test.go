package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// The tests in this file are the property obligations PRD §22 places on the
// ledger. They are randomised over many generated histories rather than
// asserted on one example, because each states something that must hold for
// every history, not for a chosen one.
//
// The generator is seeded deterministically: a failure reproduces.

const propertyRuns = 200

func newPRNG(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, 0x1ed6e12026))
}

// --- (a) an agent-origin record never reaches confirmed ---------------------

// TestPropertyAgentRecordsNeverReachConfirmed drives randomised sequences of
// every exported write on the ledger and asserts, after each, that no
// agent-origin record carries confirmed authority and that no confirmation
// exists at all unless a human review transaction produced it.
//
// This is the invariant behind PRD §10.4's "no actor may promote its own
// proposal": an agent's claim stays a proposal forever, and the confirmation
// is a separate, human-attributed record pointing at it.
func TestPropertyAgentRecordsNeverReachConfirmed(t *testing.T) {
	for run := 0; run < propertyRuns; run++ {
		seed := uint64(run)
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			rng := newPRNG(seed)
			l, store := newTestLedger(t)
			history := driveRandomHistory(t, rng, l, store)

			var confirmations int
			for _, rec := range store.all() {
				if rec.Origin.Kind == ledger.OriginAgent && rec.Authority != ledger.AuthorityProposed {
					t.Fatalf("agent-origin record %s carries authority %q; agents may only propose",
						rec.ID, rec.Authority)
				}
				switch rec.Authority {
				case ledger.AuthorityConfirmed, ledger.AuthorityRejected:
					confirmations++
					if rec.Origin.Kind != ledger.OriginHuman {
						t.Fatalf("record %s carries authority %q with origin %q; only a human review may decide",
							rec.ID, rec.Authority, rec.Origin.Kind)
					}
					if rec.RecordType != ledger.RecordReview {
						t.Fatalf("record %s carries authority %q as a %s record; decisions are review records",
							rec.ID, rec.Authority, rec.RecordType)
					}
				case ledger.AuthoritySuperseded:
					t.Fatalf("record %s was stored with authority superseded, which is never appendable", rec.ID)
				}
			}

			if confirmations > 0 && history.committedReviews == 0 {
				t.Fatalf("%d decision records exist but no review transaction was committed", confirmations)
			}
			if confirmations != history.decidedRecords {
				t.Fatalf("%d decision records exist, but review transactions decided %d records",
					confirmations, history.decidedRecords)
			}

			// Whatever else happened, every record an agent proposed is
			// still exactly the record it proposed.
			for id, digest := range history.agentDigests {
				rec, err := l.Record(context.Background(), id)
				if err != nil {
					t.Fatalf("re-read agent record %s: %v", id, err)
				}
				if rec.ContentDigest != digest || rec.Authority != ledger.AuthorityProposed {
					t.Fatalf("agent record %s changed after the fact: authority %q, digest %q",
						id, rec.Authority, rec.ContentDigest)
				}
			}
		})
	}
}

// --- (b) runner records that are not evidence are always refused ------------

// TestPropertyRunnerMayOnlyProduceEvidence asserts a runner-origin record of
// any other type is refused, for every record type and every authority — a
// runner reports what it measured, and nothing else it might be asked to say.
func TestPropertyRunnerMayOnlyProduceEvidence(t *testing.T) {
	ctx := context.Background()
	manifest := ledger.RunnerManifest{
		ActorID:          testRunner,
		ObservableFields: []string{""}, // the whole payload: as permissive as a manifest gets
	}

	for _, recordType := range ledger.RecordTypes() {
		if recordType == ledger.RecordEvidence {
			continue
		}
		for _, authority := range ledger.Authorities() {
			name := fmt.Sprintf("%s_%s", recordType, authority)
			t.Run(name, func(t *testing.T) {
				l, store := newTestLedger(t)

				_, err := l.Append(ctx, ledger.Record{
					RecordType: recordType,
					RunID:      testRunID,
					Origin:     runnerOrigin,
					Authority:  authority,
					Data:       mustJSON(t, minimalPayload(recordType)),
				}, ledger.WithRunnerManifest(manifest))

				var authErr *ledger.AuthorityError
				if !errors.As(err, &authErr) {
					t.Fatalf("Append error = %v (%T), want an *AuthorityError refusal", err, err)
				}
				if authority != ledger.AuthoritySuperseded && authErr.Rule != ledger.RuleRunnerEvidenceOnly {
					t.Fatalf("rule = %q, want %q", authErr.Rule, ledger.RuleRunnerEvidenceOnly)
				}
				if store.count() != 0 {
					t.Fatalf("stored %d records after a refused append, want 0", store.count())
				}
			})
		}
	}
}

// --- (c) superseded records never appear in a projection --------------------

// TestPropertySupersededRecordsNeverAppearInAProjection drives randomised
// histories that include supersessions and asserts no superseded record
// reaches any of the standard projections.
func TestPropertySupersededRecordsNeverAppearInAProjection(t *testing.T) {
	for run := 0; run < propertyRuns; run++ {
		seed := uint64(1000 + run)
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			rng := newPRNG(seed)
			l, store := newTestLedger(t)
			driveRandomHistory(t, rng, l, store)

			records := store.all()
			superseded := map[string]bool{}
			for _, rec := range ledger.Superseded(records) {
				superseded[rec.ID] = true
			}
			if len(superseded) == 0 {
				t.Skip("this history produced no supersessions")
			}

			for _, kind := range ledger.ProjectionKinds() {
				for _, subject := range subjectsOf(records) {
					p, err := ledger.Project(records, kind, subject)
					if err != nil {
						t.Fatalf("project %s: %v", kind, err)
					}
					for _, item := range p.Items {
						if superseded[item.ID] {
							t.Fatalf("projection %s returned superseded record %s", kind, item.ID)
						}
					}
				}
			}
		})
	}
}

// --- (d) projection digests do not depend on storage order ------------------

// TestPropertyProjectionDigestsIgnoreStorageOrder shuffles the record stream
// and asserts every projection digests identically. Determinism here is what
// lets a projection digest be checkpointed and compared across processes.
func TestPropertyProjectionDigestsIgnoreStorageOrder(t *testing.T) {
	for run := 0; run < propertyRuns; run++ {
		seed := uint64(2000 + run)
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			rng := newPRNG(seed)
			l, store := newTestLedger(t)
			driveRandomHistory(t, rng, l, store)

			records := store.all()
			if len(records) < 2 {
				t.Skip("nothing to reorder")
			}

			subject := ""
			if subjects := subjectsOf(records); len(subjects) > 0 {
				subject = subjects[rng.IntN(len(subjects))]
			}

			baseline := map[ledger.ProjectionKind]string{}
			for _, kind := range ledger.ProjectionKinds() {
				p, err := ledger.Project(records, kind, subject)
				if err != nil {
					t.Fatalf("project %s: %v", kind, err)
				}
				baseline[kind] = p.Digest
			}

			for shuffle := 0; shuffle < 5; shuffle++ {
				scrambled := append([]ledger.Record(nil), records...)
				rng.Shuffle(len(scrambled), func(i, j int) {
					scrambled[i], scrambled[j] = scrambled[j], scrambled[i]
				})

				for _, kind := range ledger.ProjectionKinds() {
					p, err := ledger.Project(scrambled, kind, subject)
					if err != nil {
						t.Fatalf("project %s on a shuffled stream: %v", kind, err)
					}
					if p.Digest != baseline[kind] {
						t.Fatalf("projection %s digest changed with storage order: %s != %s",
							kind, p.Digest, baseline[kind])
					}
				}
			}
		})
	}
}

// --- (e) a stale review commit changes nothing ------------------------------

// TestPropertyStaleReviewCommitsChangeNothing asserts that when the ledger
// has moved under a review, committing it leaves the record count, every
// record digest, and the review's own status exactly as they were.
func TestPropertyStaleReviewCommitsChangeNothing(t *testing.T) {
	ctx := context.Background()

	for run := 0; run < propertyRuns; run++ {
		seed := uint64(3000 + run)
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			rng := newPRNG(seed)
			l, store := newTestLedger(t)

			// A handful of reviewable records.
			var targets []string
			for i := 0; i < 1+rng.IntN(4); i++ {
				rec := mustAppend(t, l, claimRecord(t, fmt.Sprintf("claim %d", i)))
				targets = append(targets, rec.ID)
			}

			version := mustVersion(t, l, testRunID)
			req, err := l.CreateReviewRequest(ctx, testRunID, targets, version, ledger.WithReviewer(testHuman))
			if err != nil {
				t.Fatalf("CreateReviewRequest: %v", err)
			}

			// The ledger moves: at least one append lands after the
			// reviewer read the frame.
			for i := 0; i < 1+rng.IntN(3); i++ {
				mustAppend(t, l, claimRecord(t, fmt.Sprintf("appended later %d", i)))
			}

			before := snapshotDigests(store)

			decisions := map[string]ledger.Verdict{}
			for _, id := range targets {
				if rng.IntN(2) == 0 {
					decisions[id] = ledger.VerdictConfirm
				} else {
					decisions[id] = ledger.VerdictReject
				}
			}

			_, err = l.CommitReview(ctx, req.ID, decisions, version)
			if !errors.Is(err, ledger.ErrStaleReview) {
				t.Fatalf("CommitReview error = %v, want ErrStaleReview", err)
			}

			after := snapshotDigests(store)
			if len(after) != len(before) {
				t.Fatalf("record count changed from %d to %d across a refused review", len(before), len(after))
			}
			for id, digest := range before {
				if after[id] != digest {
					t.Fatalf("record %s changed across a refused review", id)
				}
			}

			reread, err := l.ReviewRequest(ctx, req.ID)
			if err != nil {
				t.Fatalf("re-read review: %v", err)
			}
			if reread.Status != ledger.ReviewRequested {
				t.Fatalf("review status = %q, want it left at %q", reread.Status, ledger.ReviewRequested)
			}
		})
	}
}

// --- generator --------------------------------------------------------------

type historyStats struct {
	committedReviews int
	decidedRecords   int
	agentDigests     map[string]string
}

// driveRandomHistory applies a random sequence of exported ledger operations.
// Operations that the runtime is expected to refuse are attempted too — the
// point is to search for a sequence that sneaks past a rule, so a refusal is
// a normal outcome and only an unexpected acceptance is a failure.
func driveRandomHistory(t *testing.T, rng *rand.Rand, l *ledger.Ledger, store *memStore) historyStats {
	t.Helper()
	ctx := context.Background()
	stats := historyStats{agentDigests: map[string]string{}}

	steps := 4 + rng.IntN(16)
	for step := 0; step < steps; step++ {
		switch rng.IntN(7) {
		case 0, 1:
			// A well-formed agent proposal.
			rec, err := l.Append(ctx, randomAgentRecord(t, rng))
			if err != nil {
				t.Fatalf("Append of a well-formed agent proposal: %v", err)
			}
			stats.agentDigests[rec.ID] = rec.ContentDigest

		case 2:
			// An agent reaching for an authority it does not have. Always
			// refused; the history continues.
			authority := ledger.Authorities()[rng.IntN(len(ledger.Authorities()))]
			if authority == ledger.AuthorityProposed {
				authority = ledger.AuthorityConfirmed
			}
			rec := randomAgentRecord(t, rng)
			rec.Authority = authority
			if _, err := l.Append(ctx, rec); err == nil {
				t.Fatalf("Append accepted an agent record with authority %q", authority)
			}

		case 3:
			// A human reaching for confirmed/rejected outside a review.
			rec := randomAgentRecord(t, rng)
			rec.Origin = humanOrigin
			rec.RecordType = ledger.RecordReview
			rec.Data = mustJSON(t, map[string]any{"verdict": "confirm"})
			rec.Authority = ledger.AuthorityConfirmed
			if rng.IntN(2) == 0 {
				rec.Authority = ledger.AuthorityRejected
			}
			if _, err := l.Append(ctx, rec); err == nil {
				t.Fatalf("Append accepted a human %q record outside a review transaction", rec.Authority)
			}

		case 4:
			// Runner evidence, honestly scoped.
			rec := ledger.Record{
				RecordType: ledger.RecordEvidence,
				RunID:      testRunID,
				Origin:     runnerOrigin,
				Authority:  ledger.AuthorityObserved,
				Data: mustJSON(t, map[string]any{
					"collection_method": "runner_wait_status",
					"completeness":      []string{"complete", "partial", "unknown"}[rng.IntN(3)],
					"measurements":      map[string]any{"exit_code": rng.IntN(2)},
				}),
			}
			if live := ledger.Live(store.all()); len(live) > 0 {
				rec.SubjectRef = ledger.NullableID(live[rng.IntN(len(live))].ID)
			}
			if _, err := l.Append(ctx, rec, ledger.WithRunnerManifest(ledger.RunnerManifest{
				ActorID:          testRunner,
				ObservableFields: []string{"/collection_method", "/completeness", "/measurements"},
			})); err != nil {
				t.Fatalf("Append of honestly-scoped runner evidence: %v", err)
			}

		case 5:
			// Supersede a live record.
			live := ledger.Live(store.all())
			if len(live) == 0 {
				continue
			}
			target := live[rng.IntN(len(live))]
			replacement := randomAgentRecord(t, rng)
			replacement.RecordType = target.RecordType
			replacement.Data = target.Data
			rec, err := l.AppendSuperseding(ctx, replacement, target.ID)
			if err != nil {
				if errors.Is(err, ledger.ErrAlreadySuperseded) {
					continue
				}
				t.Fatalf("AppendSuperseding: %v", err)
			}
			stats.agentDigests[rec.ID] = rec.ContentDigest

		case 6:
			// A complete review transaction over a subset of live records.
			live := ledger.Live(store.all())
			if len(live) == 0 {
				continue
			}
			var targets []string
			for _, rec := range live {
				if rec.RecordType != ledger.RecordReview && rng.IntN(3) == 0 {
					targets = append(targets, rec.ID)
				}
			}
			if len(targets) == 0 {
				continue
			}

			version := mustVersion(t, l, testRunID)
			req, err := l.CreateReviewRequest(ctx, testRunID, targets, version, ledger.WithReviewer(testHuman))
			if err != nil {
				t.Fatalf("CreateReviewRequest: %v", err)
			}

			decisions := map[string]ledger.Verdict{}
			for _, id := range req.RecordIDs {
				if rng.IntN(2) == 0 {
					decisions[id] = ledger.VerdictConfirm
				} else {
					decisions[id] = ledger.VerdictReject
				}
			}
			if _, err := l.CommitReview(ctx, req.ID, decisions, version); err != nil {
				t.Fatalf("CommitReview: %v", err)
			}
			stats.committedReviews++
			stats.decidedRecords += len(decisions)
		}
	}

	return stats
}

func randomAgentRecord(t *testing.T, rng *rand.Rand) ledger.Record {
	t.Helper()

	type shape struct {
		recordType ledger.RecordType
		payload    map[string]any
	}
	shapes := []shape{
		{ledger.RecordAnnouncement, map[string]any{"headline": "deliver the change"}},
		{ledger.RecordClaim, map[string]any{"statement": "the suite exited zero", "kind": "completion"}},
		{ledger.RecordAssumption, map[string]any{"statement": "the pinned image is current"}},
		{ledger.RecordQuestion, map[string]any{"question": "which suite gates release?", "blocking": rng.IntN(2) == 0}},
		{ledger.RecordTask, map[string]any{
			"goal":            "run the suite",
			"status":          []string{"proposed", "ready", "claimed", "running", "blocked", "completed", "failed", "cancelled"}[rng.IntN(8)],
			"assurance_state": []string{"unverified", "evidence_attached", "verified", "rejected"}[rng.IntN(4)],
		}},
		{ledger.RecordDecision, map[string]any{"question": "which runner?", "selected": "headspace"}},
		{ledger.RecordSuccessSignal, map[string]any{"statement": "suite exits zero", "mechanical": true}},
		{ledger.RecordResult, map[string]any{"outcome": []string{"completed", "changes_required", "failed"}[rng.IntN(3)]}},
	}

	chosen := shapes[rng.IntN(len(shapes))]
	return ledger.Record{
		RecordType: chosen.recordType,
		RunID:      testRunID,
		Origin:     agentOrigin,
		Authority:  ledger.AuthorityProposed,
		Data:       mustJSON(t, chosen.payload),
	}
}

// minimalPayload returns a schema-valid `data` payload for recordType. Every
// Phase 0 record type's payload is documentary and optional, so `{}`
// satisfies it; `grade` is the one type with required fields (rating,
// rationale, evaluated_actor_id), so it needs an actual minimal payload. The
// evaluated actor is a fixture id no test origin ever writes as, so this
// helper never accidentally trips the self-grade refusal for a test whose
// point is a different rule entirely.
func minimalPayload(recordType ledger.RecordType) map[string]any {
	if recordType != ledger.RecordGrade {
		return map[string]any{}
	}
	return map[string]any{
		"rating":             3,
		"rationale":          "Minimal payload for a property test that is not about grade content.",
		"evaluated_actor_id": "actor_property_fixture_evaluated",
	}
}

// subjectsOf returns references worth asking the subject-scoped projection
// about: every record id, plus the empty subject.
func subjectsOf(records []ledger.Record) []string {
	out := make([]string, 0, len(records)+1)
	out = append(out, "")
	for _, rec := range records {
		out = append(out, rec.ID)
	}
	return out
}

func snapshotDigests(store *memStore) map[string]string {
	out := map[string]string{}
	for _, rec := range store.all() {
		out[rec.ID] = rec.ContentDigest
	}
	return out
}
