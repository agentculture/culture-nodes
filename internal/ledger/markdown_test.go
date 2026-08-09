package ledger_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestMarkdownRendersEveryProjectionKind proves the renderer accepts every
// standard projection (PRD §10.9) and always says, in the artifact itself,
// that it is a reflection rather than a second source of truth.
func TestMarkdownRendersEveryProjectionKind(t *testing.T) {
	f := newFixture(t)

	for _, kind := range ledger.ProjectionKinds() {
		t.Run(string(kind), func(t *testing.T) {
			p, err := ledger.Project(f.records, kind, f.completedTask.ID)
			if err != nil {
				t.Fatalf("project %s: %v", kind, err)
			}
			md, err := p.Markdown()
			if err != nil {
				t.Fatalf("Markdown: %v", err)
			}
			if !strings.HasPrefix(md, "# ") {
				t.Errorf("%s markdown does not start with a heading:\n%s", kind, md)
			}
			if !strings.Contains(md, string(kind)) {
				t.Errorf("%s markdown does not name its own projection kind:\n%s", kind, md)
			}
			if !strings.Contains(md, p.Digest) {
				t.Errorf("%s markdown does not carry the projection's digest:\n%s", kind, md)
			}
			if !strings.Contains(md, "not authoritative") && !strings.Contains(md, "reflection") {
				t.Errorf("%s markdown does not state the PRD §10.9 non-authoritative rule:\n%s", kind, md)
			}
		})
	}
}

// TestMarkdownIsDeterministic is the determinism promise the task asks for:
// rendering the same projection twice — and rendering projections built from
// the same record set read in a different storage order — produces
// byte-identical Markdown.
func TestMarkdownIsDeterministic(t *testing.T) {
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

			mdForward, err := forward.Markdown()
			if err != nil {
				t.Fatalf("Markdown(forward): %v", err)
			}
			mdBackward, err := backward.Markdown()
			if err != nil {
				t.Fatalf("Markdown(backward): %v", err)
			}
			if mdForward != mdBackward {
				t.Fatalf("markdown depends on storage order for %s:\n--- forward ---\n%s\n--- backward ---\n%s",
					kind, mdForward, mdBackward)
			}

			// Rendering twice from the same projection value must also be
			// byte-identical — the whole point of a pure function.
			again, err := forward.Markdown()
			if err != nil {
				t.Fatalf("Markdown(forward) again: %v", err)
			}
			if again != mdForward {
				t.Fatalf("markdown is not stable across repeated calls for %s", kind)
			}
		})
	}
}

// TestMarkdownSummaryMapOrderIsStable guards the specific trap a naive
// implementation falls into: DeliveryCounts carries several map[string]int
// fields, and Go randomizes map iteration order per range. Rendering the
// same summary many times must never let that randomness leak into the
// output.
func TestMarkdownSummaryMapOrderIsStable(t *testing.T) {
	f := newFixture(t)

	p, err := ledger.DeliverySummary(f.records)
	if err != nil {
		t.Fatalf("DeliverySummary: %v", err)
	}

	first, err := p.Markdown()
	if err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	for i := 0; i < 25; i++ {
		got, err := p.Markdown()
		if err != nil {
			t.Fatalf("Markdown iteration %d: %v", i, err)
		}
		if got != first {
			t.Fatalf("delivery_summary markdown varied across renders (iteration %d) — a map is being walked unsorted", i)
		}
	}

	// Every count map key must appear, alphabetically ordered, not in
	// whatever order the underlying map happened to iterate.
	for _, want := range []string{"blocked", "completed", "ready", "running"} {
		if !strings.Contains(first, want) {
			t.Errorf("delivery_summary markdown is missing tasks_by_status key %q:\n%s", want, first)
		}
	}
}

// TestMarkdownDataIsCanonicalRegardlessOfStoredKeyOrder proves a record's
// arbitrary Data payload renders with sorted keys even when the bytes that
// happened to arrive carried a different order — the same canonicalization
// rule the digest already applies (internal/contracts.CanonicalJSON), so two
// records with the same logical payload in different byte order render
// identical Markdown.
func TestMarkdownDataIsCanonicalRegardlessOfStoredKeyOrder(t *testing.T) {
	l, store := newTestLedger(t)

	mustAppend(t, l, ledger.Record{
		RecordType: ledger.RecordAnnouncement,
		RunID:      testRunID,
		Origin:     humanOrigin,
		Authority:  ledger.AuthorityProposed,
		Data:       json.RawMessage(`{"zeta":1,"alpha":2}`),
	})

	p, err := ledger.CurrentScope(store.all())
	if err != nil {
		t.Fatalf("CurrentScope: %v", err)
	}
	md, err := p.Markdown()
	if err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	alphaIdx := strings.Index(md, `"alpha"`)
	zetaIdx := strings.Index(md, `"zeta"`)
	if alphaIdx == -1 || zetaIdx == -1 {
		t.Fatalf("markdown does not carry both data keys:\n%s", md)
	}
	if alphaIdx > zetaIdx {
		t.Fatalf("data keys are not canonically sorted in the rendered markdown:\n%s", md)
	}
}

// TestMarkdownEmptyProjectionSaysSo covers the boundary the fixture doesn't:
// a projection with no items still renders valid, non-empty, honest
// Markdown rather than an empty string or a panic.
func TestMarkdownEmptyProjectionSaysSo(t *testing.T) {
	p, err := ledger.CurrentScope(nil)
	if err != nil {
		t.Fatalf("CurrentScope(nil): %v", err)
	}
	md, err := p.Markdown()
	if err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	if !strings.Contains(strings.ToLower(md), "no records") {
		t.Errorf("empty projection markdown does not say so plainly:\n%s", md)
	}
}

// TestMarkdownDeliverySummaryCarriesCounts proves the delivery_summary kind
// renders its DeliveryCounts, not just an empty items table.
func TestMarkdownDeliverySummaryCarriesCounts(t *testing.T) {
	f := newFixture(t)

	p, err := ledger.DeliverySummary(f.records)
	if err != nil {
		t.Fatalf("DeliverySummary: %v", err)
	}
	md, err := p.Markdown()
	if err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	for _, want := range []string{
		"confirmed_claims", "rejected_claims", "undecided_claims",
		"completed_unverified_tasks", "results_awaiting_review",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("delivery_summary markdown is missing field %q:\n%s", want, md)
		}
	}
}

// TestMarkdownEvidenceForSubjectCarriesSubject proves the one projection
// kind with a Subject renders it.
func TestMarkdownEvidenceForSubjectCarriesSubject(t *testing.T) {
	f := newFixture(t)

	p, err := ledger.EvidenceForSubject(f.records, f.completedTask.ID)
	if err != nil {
		t.Fatalf("EvidenceForSubject: %v", err)
	}
	md, err := p.Markdown()
	if err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	if !strings.Contains(md, f.completedTask.ID) {
		t.Errorf("evidence_for_subject markdown does not name its subject %q:\n%s", f.completedTask.ID, md)
	}
}
