package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestAppendSupersedingLeavesTheTargetIntact is the shape of a correction in
// an append-only ledger: the old record is still there, byte for byte, and
// the new one names it.
func TestAppendSupersedingLeavesTheTargetIntact(t *testing.T) {
	ctx := context.Background()
	l, store := newTestLedger(t)

	original := mustAppend(t, l, taskRecord(t, "run the suite", "ready", "unverified"))

	replacement, err := l.AppendSuperseding(ctx, taskRecord(t, "run the suite", "running", "unverified"), original.ID)
	if err != nil {
		t.Fatalf("AppendSuperseding: %v", err)
	}
	if replacement.Supersedes.String() != original.ID {
		t.Fatalf("supersedes = %q, want %q", replacement.Supersedes, original.ID)
	}

	stored, err := l.Record(ctx, original.ID)
	if err != nil {
		t.Fatalf("read the superseded record: %v", err)
	}
	if stored.ContentDigest != original.ContentDigest {
		t.Fatal("the superseded record changed; supersession must never mutate its target")
	}
	if stored.Authority != ledger.AuthorityProposed {
		t.Fatalf("superseded record authority = %q, want it untouched at %q", stored.Authority, ledger.AuthorityProposed)
	}

	if store.count() != 2 {
		t.Fatalf("stored %d records, want 2 (the original and its replacement)", store.count())
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

func TestAppendSupersedingRefusesAnUnknownTarget(t *testing.T) {
	l, store := newTestLedger(t)

	_, err := l.AppendSuperseding(context.Background(), claimRecord(t, "replacement"), "ledger_DOESNOTEXIST0000000000001")
	if !errors.Is(err, ledger.ErrRecordNotFound) {
		t.Fatalf("AppendSuperseding error = %v, want ErrRecordNotFound", err)
	}
	if store.count() != 0 {
		t.Fatalf("stored %d records, want 0", store.count())
	}
}

// TestAppendSupersedingRefusesASecondLiveReplacement keeps the projections
// answerable: two live records both claiming to replace the same one would
// leave no defensible answer about which holds.
func TestAppendSupersedingRefusesASecondLiveReplacement(t *testing.T) {
	ctx := context.Background()
	l, store := newTestLedger(t)

	original := mustAppend(t, l, claimRecord(t, "original"))
	if _, err := l.AppendSuperseding(ctx, claimRecord(t, "first correction"), original.ID); err != nil {
		t.Fatalf("first AppendSuperseding: %v", err)
	}

	_, err := l.AppendSuperseding(ctx, claimRecord(t, "second correction"), original.ID)
	if !errors.Is(err, ledger.ErrAlreadySuperseded) {
		t.Fatalf("second AppendSuperseding error = %v, want ErrAlreadySuperseded", err)
	}
	if store.count() != 2 {
		t.Fatalf("stored %d records, want 2 — the refused append must have written nothing", store.count())
	}
}

// TestAppendSupersedingAllowsReplacingAnAlreadyReplacedAncestor states the
// other half of that rule: a correction chain moves forward, and the record
// at the head of it is the one that cannot be replaced twice. An ancestor
// whose replacement has itself been replaced has no live replacement, so it
// may be corrected again.
func TestAppendSupersedingAllowsReplacingAnAlreadyReplacedAncestor(t *testing.T) {
	ctx := context.Background()
	l, store := newTestLedger(t)

	first := mustAppend(t, l, claimRecord(t, "first"))
	second, err := l.AppendSuperseding(ctx, claimRecord(t, "second"), first.ID)
	if err != nil {
		t.Fatalf("supersede first: %v", err)
	}
	if _, err := l.AppendSuperseding(ctx, claimRecord(t, "third"), second.ID); err != nil {
		t.Fatalf("supersede second: %v", err)
	}

	if _, err := l.AppendSuperseding(ctx, claimRecord(t, "fourth"), first.ID); err != nil {
		t.Fatalf("supersede the already-replaced ancestor: %v", err)
	}

	live := ledger.Live(store.all())
	if len(live) != 2 {
		t.Fatalf("live records = %v, want the two heads of the chain", ids(live))
	}
}

func TestAppendSupersedingRefusesACrossRunReplacement(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLedger(t)

	original := mustAppend(t, l, claimRecord(t, "belongs to the test run"))

	other := claimRecord(t, "belongs elsewhere")
	other.RunID = "run_01TESTRUN0000000000000002"

	_, err := l.AppendSuperseding(ctx, other, original.ID)
	if err == nil {
		t.Fatal("AppendSuperseding accepted a replacement from another run")
	}
}

// TestAppendSupersedingInheritsTheTargetsRun lets a caller correct a record
// without restating which run it belongs to.
func TestAppendSupersedingInheritsTheTargetsRun(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLedger(t)

	original := mustAppend(t, l, claimRecord(t, "original"))

	replacement := claimRecord(t, "correction")
	replacement.RunID = ""

	out, err := l.AppendSuperseding(ctx, replacement, original.ID)
	if err != nil {
		t.Fatalf("AppendSuperseding: %v", err)
	}
	if out.RunID != testRunID {
		t.Fatalf("run_id = %q, want the target's %q", out.RunID, testRunID)
	}
}

// TestAppendSupersedingStillEnforcesAuthority proves supersession is not a
// side door around the producer matrix.
func TestAppendSupersedingStillEnforcesAuthority(t *testing.T) {
	ctx := context.Background()
	l, store := newTestLedger(t)

	original := mustAppend(t, l, claimRecord(t, "original"))

	promoted := claimRecord(t, "same claim, now asserting itself confirmed")
	promoted.Authority = ledger.AuthorityConfirmed

	_, err := l.AppendSuperseding(ctx, promoted, original.ID)
	var authErr *ledger.AuthorityError
	if !errors.As(err, &authErr) || authErr.Rule != ledger.RuleAgentProposesOnly {
		t.Fatalf("AppendSuperseding error = %v, want rule %s", err, ledger.RuleAgentProposesOnly)
	}
	if store.count() != 1 {
		t.Fatalf("stored %d records, want 1", store.count())
	}
}

func ids(records []ledger.Record) []string {
	out := make([]string, 0, len(records))
	for _, rec := range records {
		out = append(out, rec.ID)
	}
	return out
}
