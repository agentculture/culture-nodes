package declengine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
)

// overlapCandidate builds a minimal ActiveDeclaration for overlap-analysis
// tests: same shape as engine_test.go's active() helper, but with the node,
// trigger kind and condition all controllable per test case.
func overlapCandidate(id, node, triggerKind, condition string) ActiveDeclaration {
	return ActiveDeclaration{
		ID:        id,
		VersionID: id + "-v1",
		Declaration: decl.Declaration{
			Name:        id,
			Trigger:     decl.Trigger{Kind: triggerKind, ReentryLimit: 3, HopLimit: 20, RateCeiling: "30/h"},
			Condition:   condition,
			Action:      decl.Action{Kind: "agent.work", With: json.RawMessage(`{"uses":"actor://test","input":{}}`)},
			StartNode:   decl.Node{Name: node, Deadline: "none"},
			LandingNode: decl.Node{Name: "waiting", Deadline: "1h"},
		},
	}
}

// Acceptance criterion 1: priority=='High' and priority!='Low' overlap and
// must be reported; priority=='High' and priority=='Low' are a
// contradiction and must NOT be reported.
func TestDetectOverlapsFixturesFromTheBrief(t *testing.T) {
	t.Run("high and not-low is reported overlapping", func(t *testing.T) {
		a := overlapCandidate("decl-a", "ready", "jira.issue.created", "event.priority == 'High'")
		b := overlapCandidate("decl-b", "ready", "jira.issue.created", "event.priority != 'Low'")
		pairs := DetectOverlaps([]ActiveDeclaration{a, b})
		if len(pairs) != 1 {
			t.Fatalf("got %d pairs, want 1: %+v", len(pairs), pairs)
		}
		if pairs[0].Status != OverlapConfirmed {
			t.Fatalf("status = %q, want %q", pairs[0].Status, OverlapConfirmed)
		}
	})

	t.Run("high and low is a contradiction, not reported", func(t *testing.T) {
		a := overlapCandidate("decl-a", "ready", "jira.issue.created", "event.priority == 'High'")
		b := overlapCandidate("decl-b", "ready", "jira.issue.created", "event.priority == 'Low'")
		pairs := DetectOverlaps([]ActiveDeclaration{a, b})
		if len(pairs) != 0 {
			t.Fatalf("got %d pairs, want 0 (decided non-overlap): %+v", len(pairs), pairs)
		}
	})
}

func TestDetectOverlapsUndecidableIsPossiblyOverlapping(t *testing.T) {
	for _, source := range []string{
		"event.priority == 'High' || event.priority == 'Low'", // disjunction
		"event.a == event.b",             // field vs field
		"event.priority.startsWith('H')", // function call
		"lineage.x == 'y'",               // not an event field
	} {
		a := overlapCandidate("decl-a", "ready", "pr-upkeep.pr", source)
		b := overlapCandidate("decl-b", "ready", "pr-upkeep.pr", "event.priority == 'High'")
		pairs := DetectOverlaps([]ActiveDeclaration{a, b})
		if len(pairs) != 1 {
			t.Fatalf("source %q: got %d pairs, want 1", source, len(pairs))
		}
		if pairs[0].Status != OverlapPossible {
			t.Fatalf("source %q: status = %q, want %q", source, pairs[0].Status, OverlapPossible)
		}
	}
}

func TestDetectOverlapsRequiresSameNodeAndTrigger(t *testing.T) {
	base := overlapCandidate("decl-a", "ready", "pr-upkeep.pr", "true")
	diffNode := overlapCandidate("decl-b", "other", "pr-upkeep.pr", "true")
	diffTrigger := overlapCandidate("decl-c", "ready", "jira.issue.created", "true")
	if pairs := DetectOverlaps([]ActiveDeclaration{base, diffNode}); len(pairs) != 0 {
		t.Fatalf("declarations on different nodes must not pair: %+v", pairs)
	}
	if pairs := DetectOverlaps([]ActiveDeclaration{base, diffTrigger}); len(pairs) != 0 {
		t.Fatalf("declarations on different triggers must not pair: %+v", pairs)
	}
}

func TestDetectOverlapsUnconditionalDeclarationsAlwaysOverlap(t *testing.T) {
	a := overlapCandidate("decl-a", "ready", "pr-upkeep.pr", "true")
	b := overlapCandidate("decl-b", "ready", "pr-upkeep.pr", "true")
	pairs := DetectOverlaps([]ActiveDeclaration{a, b})
	if len(pairs) != 1 || pairs[0].Status != OverlapConfirmed {
		t.Fatalf("two unconditional declarations on the same node/trigger must be a confirmed overlap: %+v", pairs)
	}
}

func TestDetectOverlapsLiteralFalseNeverOverlaps(t *testing.T) {
	a := overlapCandidate("decl-a", "ready", "pr-upkeep.pr", "false")
	b := overlapCandidate("decl-b", "ready", "pr-upkeep.pr", "true")
	pairs := DetectOverlaps([]ActiveDeclaration{a, b})
	if len(pairs) != 0 {
		t.Fatalf("a declaration whose condition is literally false can never overlap: %+v", pairs)
	}
}

func TestDetectOverlapsPairOrderingIsStable(t *testing.T) {
	a := overlapCandidate("decl-z", "ready", "pr-upkeep.pr", "true")
	b := overlapCandidate("decl-a", "ready", "pr-upkeep.pr", "true")
	pairs1 := DetectOverlaps([]ActiveDeclaration{a, b})
	pairs2 := DetectOverlaps([]ActiveDeclaration{b, a})
	if len(pairs1) != 1 || len(pairs2) != 1 {
		t.Fatalf("expected exactly one pair each way, got %d and %d", len(pairs1), len(pairs2))
	}
	if pairs1[0].A.DeclarationID != "decl-a" || pairs1[0].B.DeclarationID != "decl-z" {
		t.Fatalf("pair not ordered by declaration id: %+v", pairs1[0])
	}
	if pairs1[0] != pairs2[0] {
		t.Fatalf("scan order changed the reported pair: %+v vs %+v", pairs1[0], pairs2[0])
	}
}

func TestDetectOverlapsIgnoresSelfPairing(t *testing.T) {
	// Defensive: Active() never returns two rows for the same
	// declaration_id, but DetectOverlaps must not pair a declaration with
	// itself if it ever did.
	a := overlapCandidate("decl-a", "ready", "pr-upkeep.pr", "true")
	dup := a
	pairs := DetectOverlaps([]ActiveDeclaration{a, dup})
	if len(pairs) != 0 {
		t.Fatalf("a declaration must never pair with itself: %+v", pairs)
	}
}

func TestFormatOverlapReportIsReadable(t *testing.T) {
	if got := FormatOverlapReport(nil); got == "" {
		t.Fatal("empty report must say nothing was found, not return an empty string")
	}
	a := overlapCandidate("decl-a", "ready", "jira.issue.created", "event.priority == 'High'")
	b := overlapCandidate("decl-b", "ready", "jira.issue.created", "event.priority != 'Low'")
	pairs := DetectOverlaps([]ActiveDeclaration{a, b})
	report := FormatOverlapReport(pairs)
	for _, want := range []string{"decl-a", "decl-b", "ready", "jira.issue.created", OverlapConfirmed} {
		if !strings.Contains(report, want) {
			t.Fatalf("report %q missing %q", report, want)
		}
	}
}

// Boolean-domain closed check: excluding both true and false leaves nothing
// -- the one case where two "!=" conjunctions on the same field ARE decided
// unsatisfiable rather than assumed compatible.
func TestDetectOverlapsBoolDomainExhaustedIsDecidedNonOverlap(t *testing.T) {
	a := overlapCandidate("decl-a", "ready", "pr-upkeep.pr", "event.done != true")
	b := overlapCandidate("decl-b", "ready", "pr-upkeep.pr", "event.done != false")
	pairs := DetectOverlaps([]ActiveDeclaration{a, b})
	if len(pairs) != 0 {
		t.Fatalf("excluding both bool values leaves no satisfying assignment: %+v", pairs)
	}
}

func TestDetectOverlapsStringDomainNeqNeqOverlaps(t *testing.T) {
	// String domain is treated as open: excluding two different strings
	// still leaves room, so this is a decided overlap.
	a := overlapCandidate("decl-a", "ready", "pr-upkeep.pr", "event.priority != 'Low'")
	b := overlapCandidate("decl-b", "ready", "pr-upkeep.pr", "event.priority != 'Medium'")
	pairs := DetectOverlaps([]ActiveDeclaration{a, b})
	if len(pairs) != 1 || pairs[0].Status != OverlapConfirmed {
		t.Fatalf("two open-domain != constraints on the same field should decide overlap: %+v", pairs)
	}
}
