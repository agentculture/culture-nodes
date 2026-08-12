package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestGradeSelfRefusal is the acceptance case at the heart of t14: a grade
// whose grading origin actor names itself as the evaluated actor is refused
// with a structured error, before a single record is written. This is the
// PRD §10.4 "no actor promotes its own proposal" rule extended to opinion
// records (issue #28 item 1).
func TestGradeSelfRefusal(t *testing.T) {
	l, store := newTestLedger(t)

	_, err := l.Append(context.Background(), gradeRecord(t, agentOrigin, ledger.AuthorityProposed, testAgent, 5))

	var authErr *ledger.AuthorityError
	if !errors.As(err, &authErr) {
		t.Fatalf("Append error = %v (%T), want *ledger.AuthorityError with rule %s", err, err, ledger.RuleNoSelfGrade)
	}
	if authErr.Rule != ledger.RuleNoSelfGrade {
		t.Fatalf("refused by rule %q, want %q (error: %v)", authErr.Rule, ledger.RuleNoSelfGrade, authErr)
	}
	if authErr.Field != "/data/evaluated_actor_id" {
		t.Fatalf("Field = %q, want %q", authErr.Field, "/data/evaluated_actor_id")
	}
	if store.count() != 0 {
		t.Fatalf("stored %d records after a refused self-grade, want 0", store.count())
	}
}

// TestGradeSelfRefusalAppliesRegardlessOfOrigin proves the rule is not
// agent-specific: a human naming themselves as the evaluated actor is
// refused the same way. Nothing about the rule turns on producer kind.
func TestGradeSelfRefusalAppliesRegardlessOfOrigin(t *testing.T) {
	l, store := newTestLedger(t)

	_, err := l.Append(context.Background(), gradeRecord(t, humanOrigin, ledger.AuthorityConfirmed, testHuman, 5))

	var authErr *ledger.AuthorityError
	if !errors.As(err, &authErr) || authErr.Rule != ledger.RuleNoSelfGrade {
		t.Fatalf("Append error = %v, want rule %s", err, ledger.RuleNoSelfGrade)
	}
	if store.count() != 0 {
		t.Fatalf("stored %d records after a refused self-grade, want 0", store.count())
	}
}

// TestGradeCrossActorAllowed is the other half of the acceptance criteria:
// a grade naming an actor other than the grading origin is admitted
// normally through the ordinary producer/authority matrix.
func TestGradeCrossActorAllowed(t *testing.T) {
	l, store := newTestLedger(t)

	out, err := l.Append(context.Background(), gradeRecord(t, agentOrigin, ledger.AuthorityProposed, "actor_being_graded", 5))
	if err != nil {
		t.Fatalf("Append: %v, want a cross-actor grade to be accepted", err)
	}
	if out.Authority != ledger.AuthorityProposed {
		t.Fatalf("authority = %q, want proposed", out.Authority)
	}
	if store.count() != 1 {
		t.Fatalf("stored %d records, want 1", store.count())
	}
}

// TestGradeCheckAuthorityMatchesAppendForSelfGrade proves the exported pure
// check (used by the engine's ledger-delta pre-flight, internal/engine
// prepareRecord) answers the same question about a self-grade that the real
// Append path does, so a bad delta is caught before it is ever attempted.
func TestGradeCheckAuthorityMatchesAppendForSelfGrade(t *testing.T) {
	l, _ := newTestLedger(t)
	rec := gradeRecord(t, agentOrigin, ledger.AuthorityProposed, testAgent, 5)

	checkErr := ledger.CheckAuthority(rec, nil)
	_, appendErr := l.Append(context.Background(), rec)

	var checked, appended *ledger.AuthorityError
	if !errors.As(checkErr, &checked) {
		t.Fatalf("CheckAuthority error = %v, want *ledger.AuthorityError", checkErr)
	}
	if !errors.As(appendErr, &appended) {
		t.Fatalf("Append error = %v, want *ledger.AuthorityError", appendErr)
	}
	if checked.Rule != ledger.RuleNoSelfGrade || appended.Rule != ledger.RuleNoSelfGrade {
		t.Fatalf("CheckAuthority rule %q, Append rule %q, want both %q", checked.Rule, appended.Rule, ledger.RuleNoSelfGrade)
	}
}

// TestGradeHumanDirectConfirmWithoutReview is the other authority rule t14
// adds: a human's own grade is already their confirmed judgment, so it may
// land confirmed authority as an ordinary append — it does not have to
// review itself through a review transaction the way a claim would.
func TestGradeHumanDirectConfirmWithoutReview(t *testing.T) {
	l, store := newTestLedger(t)

	out, err := l.Append(context.Background(), gradeRecord(t, humanOrigin, ledger.AuthorityConfirmed, "actor_being_graded", 5))
	if err != nil {
		t.Fatalf("Append: %v, want a human's direct grade confirmation to be accepted without a review transaction", err)
	}
	if out.Authority != ledger.AuthorityConfirmed {
		t.Fatalf("authority = %q, want confirmed", out.Authority)
	}
	if store.count() != 1 {
		t.Fatalf("stored %d records, want 1", store.count())
	}
}

// TestGradeHumanCannotRejectDirectlyOutsideReview keeps the direct-confirm
// carve-out narrow: it does not extend to `rejected`, which still requires
// the ordinary review transaction (rejecting a grade means reviewing
// someone else's proposed grade, not asserting one's own).
func TestGradeHumanCannotRejectDirectlyOutsideReview(t *testing.T) {
	l, store := newTestLedger(t)

	_, err := l.Append(context.Background(), gradeRecord(t, humanOrigin, ledger.AuthorityRejected, "actor_being_graded", 1))

	var authErr *ledger.AuthorityError
	if !errors.As(err, &authErr) || authErr.Rule != ledger.RuleHumanReviewOnly {
		t.Fatalf("Append error = %v, want rule %s", err, ledger.RuleHumanReviewOnly)
	}
	if store.count() != 0 {
		t.Fatalf("stored %d records, want 0", store.count())
	}
}

// TestGradeAgentCannotConfirmDirectly proves the human-only carve-out does
// not leak to agent origin: an agent still only ever proposes, grade
// included.
func TestGradeAgentCannotConfirmDirectly(t *testing.T) {
	l, store := newTestLedger(t)

	_, err := l.Append(context.Background(), gradeRecord(t, agentOrigin, ledger.AuthorityConfirmed, "actor_being_graded", 5))

	var authErr *ledger.AuthorityError
	if !errors.As(err, &authErr) || authErr.Rule != ledger.RuleAgentProposesOnly {
		t.Fatalf("Append error = %v, want rule %s", err, ledger.RuleAgentProposesOnly)
	}
	if store.count() != 0 {
		t.Fatalf("stored %d records, want 0", store.count())
	}
}

// TestGradeNeverObservedOrDerived proves a grade+observed or grade+derived
// record is refused for every origin, whichever rule ends up naming the
// refusal. Most origins already refuse it through their ordinary rule
// (agents may only propose, a runner may only write evidence, a human's
// observed/derived is limited) independent of record type; the deterministic
// producers are the one case whose ordinary rule (derived, and only derived)
// would otherwise admit it, so that path names the grade-specific rule.
func TestGradeNeverObservedOrDerived(t *testing.T) {
	cases := []struct {
		name      string
		origin    ledger.Origin
		authority ledger.Authority
		manifest  *ledger.RunnerManifest
		wantRule  string
	}{
		{"agent observes", agentOrigin, ledger.AuthorityObserved, nil, ledger.RuleAgentProposesOnly},
		{"agent derives", agentOrigin, ledger.AuthorityDerived, nil, ledger.RuleAgentProposesOnly},
		{"human observes", humanOrigin, ledger.AuthorityObserved, nil, ledger.RuleHumanAuthorityLimited},
		{"human derives", humanOrigin, ledger.AuthorityDerived, nil, ledger.RuleHumanAuthorityLimited},
		{"runner observes", runnerOrigin, ledger.AuthorityObserved, &ledger.RunnerManifest{ActorID: testRunner, ObservableFields: []string{""}}, ledger.RuleRunnerEvidenceOnly},
		{"engine derives", engineOrigin, ledger.AuthorityDerived, nil, ledger.RuleGradeNeverObservedOrDerived},
		{"validator derives", validatorOrigin, ledger.AuthorityDerived, nil, ledger.RuleGradeNeverObservedOrDerived},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, store := newTestLedger(t)
			rec := gradeRecord(t, tc.origin, tc.authority, "actor_being_graded", 5)

			var opts []ledger.AppendOption
			if tc.manifest != nil {
				opts = append(opts, ledger.WithRunnerManifest(*tc.manifest))
			}
			_, err := l.Append(context.Background(), rec, opts...)

			var authErr *ledger.AuthorityError
			if !errors.As(err, &authErr) {
				t.Fatalf("Append error = %v (%T), want *ledger.AuthorityError", err, err)
			}
			if authErr.Rule != tc.wantRule {
				t.Fatalf("rule = %q, want %q", authErr.Rule, tc.wantRule)
			}
			if store.count() != 0 {
				t.Fatalf("stored %d records, want 0", store.count())
			}
		})
	}
}

// TestGradeCorrectionAppendsWithSupersedes proves a grade correction follows
// the same append-only supersession semantics as every other record type:
// the original is untouched and the replacement names it.
func TestGradeCorrectionAppendsWithSupersedes(t *testing.T) {
	ctx := context.Background()
	l, store := newTestLedger(t)

	original := mustAppend(t, l, gradeRecord(t, agentOrigin, ledger.AuthorityProposed, "actor_being_graded", 3))

	replacement, err := l.AppendSuperseding(ctx,
		gradeRecord(t, agentOrigin, ledger.AuthorityProposed, "actor_being_graded", 4),
		original.ID)
	if err != nil {
		t.Fatalf("AppendSuperseding: %v", err)
	}
	if replacement.Supersedes.String() != original.ID {
		t.Fatalf("supersedes = %q, want %q", replacement.Supersedes, original.ID)
	}

	stored, err := l.Record(ctx, original.ID)
	if err != nil {
		t.Fatalf("read the superseded grade: %v", err)
	}
	if stored.ContentDigest != original.ContentDigest {
		t.Fatal("the superseded grade changed; supersession must never mutate its target")
	}
	if store.count() != 2 {
		t.Fatalf("stored %d records, want 2 (the original grade and its correction)", store.count())
	}

	live := ledger.Live(store.all())
	if len(live) != 1 || live[0].ID != replacement.ID {
		t.Fatalf("live records = %v, want only %s", ids(live), replacement.ID)
	}
	superseded := ledger.Superseded(store.all())
	if len(superseded) != 1 || superseded[0].ID != original.ID {
		t.Fatalf("superseded records = %v, want only %s", ids(superseded), original.ID)
	}
}

// TestGradeCorrectionStillEnforcesSelfGrade proves supersession is not a
// side door around the self-grade rule either.
func TestGradeCorrectionStillEnforcesSelfGrade(t *testing.T) {
	ctx := context.Background()
	l, store := newTestLedger(t)

	original := mustAppend(t, l, gradeRecord(t, agentOrigin, ledger.AuthorityProposed, "actor_being_graded", 3))

	_, err := l.AppendSuperseding(ctx,
		gradeRecord(t, agentOrigin, ledger.AuthorityProposed, testAgent, 1),
		original.ID)

	var authErr *ledger.AuthorityError
	if !errors.As(err, &authErr) || authErr.Rule != ledger.RuleNoSelfGrade {
		t.Fatalf("AppendSuperseding error = %v, want rule %s", err, ledger.RuleNoSelfGrade)
	}
	if store.count() != 1 {
		t.Fatalf("stored %d records, want 1 — the refused correction must have written nothing", store.count())
	}
}
