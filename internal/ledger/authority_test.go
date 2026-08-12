package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestAppendEnforcesProducerAuthorityMatrix walks the whole PRD §10.4
// producer/authority matrix through the real Append path. Every cell is
// stated: the ones that are permitted, and the ones that are refused with the
// rule that refuses them.
func TestAppendEnforcesProducerAuthorityMatrix(t *testing.T) {
	runnerManifest := ledger.RunnerManifest{
		ActorID:          testRunner,
		ObservableFields: []string{"/collection_method", "/measurements"},
	}

	cases := []struct {
		name       string
		origin     ledger.Origin
		recordType ledger.RecordType
		authority  ledger.Authority
		manifest   *ledger.RunnerManifest
		wantRule   string // "" means the append must be accepted
	}{
		// Agents propose, and only propose.
		{"agent proposes a claim", agentOrigin, ledger.RecordClaim, ledger.AuthorityProposed, nil, ""},
		{"agent proposes a task", agentOrigin, ledger.RecordTask, ledger.AuthorityProposed, nil, ""},
		{"agent proposes evidence", agentOrigin, ledger.RecordEvidence, ledger.AuthorityProposed, nil, ""},
		{"agent confirms", agentOrigin, ledger.RecordClaim, ledger.AuthorityConfirmed, nil, ledger.RuleAgentProposesOnly},
		{"agent rejects", agentOrigin, ledger.RecordClaim, ledger.AuthorityRejected, nil, ledger.RuleAgentProposesOnly},
		{"agent observes", agentOrigin, ledger.RecordEvidence, ledger.AuthorityObserved, nil, ledger.RuleAgentProposesOnly},
		{"agent derives", agentOrigin, ledger.RecordResult, ledger.AuthorityDerived, nil, ledger.RuleAgentProposesOnly},

		// Humans propose freely; confirmation and rejection are review
		// transactions, not appends.
		{"human proposes a decision", humanOrigin, ledger.RecordDecision, ledger.AuthorityProposed, nil, ""},
		{"human confirms directly", humanOrigin, ledger.RecordReview, ledger.AuthorityConfirmed, nil, ledger.RuleHumanReviewOnly},
		{"human rejects directly", humanOrigin, ledger.RecordReview, ledger.AuthorityRejected, nil, ledger.RuleHumanReviewOnly},
		{"human observes", humanOrigin, ledger.RecordEvidence, ledger.AuthorityObserved, nil, ledger.RuleHumanAuthorityLimited},
		{"human derives", humanOrigin, ledger.RecordResult, ledger.AuthorityDerived, nil, ledger.RuleHumanAuthorityLimited},

		// Runners observe evidence, and nothing else.
		{"runner observes evidence", runnerOrigin, ledger.RecordEvidence, ledger.AuthorityObserved, &runnerManifest, ""},
		{"runner observes a claim", runnerOrigin, ledger.RecordClaim, ledger.AuthorityObserved, &runnerManifest, ledger.RuleRunnerEvidenceOnly},
		{"runner observes a result", runnerOrigin, ledger.RecordResult, ledger.AuthorityObserved, &runnerManifest, ledger.RuleRunnerEvidenceOnly},
		{"runner proposes evidence", runnerOrigin, ledger.RecordEvidence, ledger.AuthorityProposed, &runnerManifest, ledger.RuleRunnerObservedOnly},
		{"runner confirms evidence", runnerOrigin, ledger.RecordEvidence, ledger.AuthorityConfirmed, &runnerManifest, ledger.RuleRunnerObservedOnly},
		{"runner observes without a manifest", runnerOrigin, ledger.RecordEvidence, ledger.AuthorityObserved, nil, ledger.RuleRunnerManifestRequired},

		// Deterministic producers derive, and only derive.
		{"engine derives", engineOrigin, ledger.RecordResult, ledger.AuthorityDerived, nil, ""},
		{"validator derives", validatorOrigin, ledger.RecordSuccessSignal, ledger.AuthorityDerived, nil, ""},
		{"engine proposes", engineOrigin, ledger.RecordResult, ledger.AuthorityProposed, nil, ledger.RuleDeterministicDerivedOnly},
		{"validator confirms", validatorOrigin, ledger.RecordClaim, ledger.AuthorityConfirmed, nil, ledger.RuleDeterministicDerivedOnly},

		// No producer rule exists for a service origin.
		{"service proposes", serviceOrigin, ledger.RecordClaim, ledger.AuthorityProposed, nil, ledger.RuleUnknownOrigin},
		{"service derives", serviceOrigin, ledger.RecordResult, ledger.AuthorityDerived, nil, ledger.RuleUnknownOrigin},

		// `superseded` is read from later records, never written.
		{"agent declares itself superseded", agentOrigin, ledger.RecordClaim, ledger.AuthoritySuperseded, nil, ledger.RuleSupersededNotAppendable},
		{"engine declares superseded", engineOrigin, ledger.RecordResult, ledger.AuthoritySuperseded, nil, ledger.RuleSupersededNotAppendable},

		// A grade is an opinion record. Agents propose it like anything
		// else; a human grading directly lands it confirmed without a
		// review transaction (see grade_test.go for the dedicated
		// coverage). No origin may make it observed or derived: a runner
		// is refused for not being evidence at all (its own rule fires
		// first), and the deterministic producers -- the one origin whose
		// ordinary rule would otherwise admit it -- are refused by the
		// grade-specific rule (see TestGradeNeverObservedOrDerived for the
		// full per-origin picture).
		{"agent proposes a grade", agentOrigin, ledger.RecordGrade, ledger.AuthorityProposed, nil, ""},
		{"agent confirms a grade directly", agentOrigin, ledger.RecordGrade, ledger.AuthorityConfirmed, nil, ledger.RuleAgentProposesOnly},
		{"human confirms a grade directly", humanOrigin, ledger.RecordGrade, ledger.AuthorityConfirmed, nil, ""},
		{"runner observes a grade", runnerOrigin, ledger.RecordGrade, ledger.AuthorityObserved, &runnerManifest, ledger.RuleRunnerEvidenceOnly},
		{"engine derives a grade", engineOrigin, ledger.RecordGrade, ledger.AuthorityDerived, nil, ledger.RuleGradeNeverObservedOrDerived},
		{"validator derives a grade", validatorOrigin, ledger.RecordGrade, ledger.AuthorityDerived, nil, ledger.RuleGradeNeverObservedOrDerived},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, store := newTestLedger(t)

			rec := ledger.Record{
				RecordType: tc.recordType,
				RunID:      testRunID,
				Origin:     tc.origin,
				Authority:  tc.authority,
			}
			if tc.recordType == ledger.RecordEvidence {
				rec.Data = mustJSON(t, map[string]any{
					"collection_method": "runner_wait_status",
					"measurements":      map[string]any{"exit_code": 0},
				})
			}
			if tc.recordType == ledger.RecordGrade {
				// A schema-valid payload naming an evaluated actor distinct
				// from every origin fixture, so the matrix cases exercise
				// the producer/authority rule under test and never trip the
				// self-grade refusal by accident; TestGradeSelfRefusal
				// covers that rule directly.
				rec.Data = mustJSON(t, map[string]any{
					"rating":             4,
					"rationale":          "Delivered the change set with clear evidence and no rework.",
					"evaluated_actor_id": "actor_being_graded",
				})
			}

			var opts []ledger.AppendOption
			if tc.manifest != nil {
				opts = append(opts, ledger.WithRunnerManifest(*tc.manifest))
			}

			_, err := l.Append(context.Background(), rec, opts...)

			if tc.wantRule == "" {
				if err != nil {
					t.Fatalf("Append: %v, want the append to be accepted", err)
				}
				if store.count() != 1 {
					t.Fatalf("stored %d records, want 1", store.count())
				}
				return
			}

			var authErr *ledger.AuthorityError
			if !errors.As(err, &authErr) {
				t.Fatalf("Append error = %v (%T), want *ledger.AuthorityError with rule %s", err, err, tc.wantRule)
			}
			if authErr.Rule != tc.wantRule {
				t.Fatalf("refused by rule %q, want %q (error: %v)", authErr.Rule, tc.wantRule, authErr)
			}
			if store.count() != 0 {
				t.Fatalf("stored %d records after a refused append, want 0", store.count())
			}
		})
	}
}

// TestRunnerManifestBoundsWhatCanBeObserved is the honesty rule from PRD
// §10.5 in code: a runner observes the exit status it waited on; the text the
// process printed about itself is not an observation, and a runner cannot
// launder it into one by putting it in an evidence record.
func TestRunnerManifestBoundsWhatCanBeObserved(t *testing.T) {
	manifest := ledger.RunnerManifest{
		ActorID: testRunner,
		ObservableFields: []string{
			"/collection_method",
			"/observed_at",
			"/covered_scope",
			"/completeness",
			"/measurements/exit_code",
			"/measurements/duration_ms",
		},
	}

	cases := []struct {
		name      string
		payload   map[string]any
		wantField string // "" means accepted
	}{
		{
			name: "declared measurements are observed",
			payload: map[string]any{
				"collection_method": "runner_wait_status",
				"covered_scope":     "Exit status of the pinned test command.",
				"completeness":      "complete",
				"measurements":      map[string]any{"exit_code": 0, "duration_ms": 72000},
			},
		},
		{
			name: "process-reported test outcome is not observed",
			payload: map[string]any{
				"collection_method": "runner_wait_status",
				"measurements":      map[string]any{"exit_code": 0, "tests_passed": 41},
			},
			wantField: "/measurements/tests_passed",
		},
		{
			name: "process stdout is not observed",
			payload: map[string]any{
				"collection_method": "runner_wait_status",
				"stdout":            "ok  all tests passed",
			},
			wantField: "/stdout",
		},
		{
			name:    "empty payload observes nothing and claims nothing",
			payload: map[string]any{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newTestLedger(t)

			_, err := l.Append(context.Background(), ledger.Record{
				RecordType: ledger.RecordEvidence,
				RunID:      testRunID,
				Origin:     runnerOrigin,
				Authority:  ledger.AuthorityObserved,
				Data:       mustJSON(t, tc.payload),
			}, ledger.WithRunnerManifest(manifest))

			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("Append: %v, want acceptance", err)
				}
				return
			}

			var authErr *ledger.AuthorityError
			if !errors.As(err, &authErr) {
				t.Fatalf("Append error = %v (%T), want *ledger.AuthorityError", err, err)
			}
			if authErr.Rule != ledger.RuleRunnerFieldNotDeclared {
				t.Fatalf("rule = %q, want %q", authErr.Rule, ledger.RuleRunnerFieldNotDeclared)
			}
			if authErr.Field != tc.wantField {
				t.Fatalf("refused field = %q, want %q", authErr.Field, tc.wantField)
			}
		})
	}
}

// TestRunnerManifestMustBelongToTheRecordsActor keeps one runner's manifest
// from covering another runner's evidence.
func TestRunnerManifestMustBelongToTheRecordsActor(t *testing.T) {
	l, _ := newTestLedger(t)

	_, err := l.Append(context.Background(), ledger.Record{
		RecordType: ledger.RecordEvidence,
		RunID:      testRunID,
		Origin:     runnerOrigin,
		Authority:  ledger.AuthorityObserved,
		Data:       mustJSON(t, map[string]any{"collection_method": "runner_wait_status"}),
	}, ledger.WithRunnerManifest(ledger.RunnerManifest{
		ActorID:          "runner_someone_else",
		ObservableFields: []string{"/collection_method"},
	}))

	var authErr *ledger.AuthorityError
	if !errors.As(err, &authErr) || authErr.Rule != ledger.RuleRunnerActorMismatch {
		t.Fatalf("Append error = %v, want rule %s", err, ledger.RuleRunnerActorMismatch)
	}
}

// TestCheckAuthorityMatchesAppend proves the exported pure check answers the
// same question the append path asks, so an API layer can pre-flight a record
// without a store and get the real answer.
func TestCheckAuthorityMatchesAppend(t *testing.T) {
	l, _ := newTestLedger(t)

	rec := ledger.Record{
		RecordType: ledger.RecordClaim,
		RunID:      testRunID,
		Origin:     agentOrigin,
		Authority:  ledger.AuthorityConfirmed,
	}

	checkErr := ledger.CheckAuthority(rec, nil)
	_, appendErr := l.Append(context.Background(), rec)

	var checked, appended *ledger.AuthorityError
	if !errors.As(checkErr, &checked) {
		t.Fatalf("CheckAuthority error = %v, want *ledger.AuthorityError", checkErr)
	}
	if !errors.As(appendErr, &appended) {
		t.Fatalf("Append error = %v, want *ledger.AuthorityError", appendErr)
	}
	if checked.Rule != appended.Rule {
		t.Fatalf("CheckAuthority rule %q != Append rule %q", checked.Rule, appended.Rule)
	}
}

// TestAppendRejectsSchemaInvalidRecords proves schema validation is not
// skipped for a record whose authority happens to be permitted.
func TestAppendRejectsSchemaInvalidRecords(t *testing.T) {
	l, store := newTestLedger(t)

	_, err := l.Append(context.Background(), ledger.Record{
		RecordType: "prophecy", // not a registered record type
		RunID:      testRunID,
		Origin:     agentOrigin,
		Authority:  ledger.AuthorityProposed,
	})
	if err == nil {
		t.Fatal("Append accepted an unregistered record type, want a schema rejection")
	}
	if store.count() != 0 {
		t.Fatalf("stored %d records, want 0", store.count())
	}
}
