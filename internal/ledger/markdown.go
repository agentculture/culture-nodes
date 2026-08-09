package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
)

// Markdown rendering of ledger projections (PRD §10.9, §24 "Markdown
// summaries are derived from JSON and are not authoritative").
//
// A projection is already a pure function of a record set (see
// projection.go's own package doc). Markdown() is a second pure function
// layered on top of that: given a Projection value, it always produces the
// same bytes. Two things make that true in the face of the actual dangers a
// naive renderer would fall into:
//
//   - a projection's Items are already sorted by id before this file ever
//     sees them (every projection constructor in this package sorts before
//     returning), so record order never depends on storage order;
//   - every Go map this file walks — a record's own Data payload, decoded
//     through DataMap, and DeliveryCounts' several map[string]int fields —
//     is walked in sorted-key order explicitly. Go randomizes map iteration
//     order per range statement; relying on it would make the "byte-stable
//     for identical input" promise false on a re-run within the very same
//     process, not just across machines.
//
// Editing the rendered text changes nothing: it is not read back by
// anything in this runtime. That is the PRD's own rule, restated in the
// artifact itself via markdownNotice.

// projectionTitles names the human-readable heading for each standard
// projection kind (PRD §10.9 order). A kind with no entry here (there should
// never be one, since ProjectionKinds() is closed) falls back to its raw
// kind string rather than panicking.
var projectionTitles = map[ProjectionKind]string{
	KindCurrentScope:    "Current Scope",
	KindConfirmedClaims: "Confirmed Claims",
	KindOpenAssumptions: "Open Assumptions and Questions",
	KindReadyTasks:      "Ready Tasks",
	KindActiveTasks:     "Active Tasks",
	KindVerificationQ:   "Verification Queue",
	KindDecisionHistory: "Decision History",
	KindEvidenceFor:     "Evidence",
	KindDeliverySummary: "Delivery Summary",
}

// markdownNotice is stamped into every rendered projection, spelling the PRD
// §10.9 rule out in the artifact itself rather than leaving it to whoever
// reads the file to already know it.
const markdownNotice = "> Generated from a ledger projection (PRD §10.9): this Markdown is a " +
	"reflection of the JSON projection below its digest. It is not authoritative — editing this " +
	"file changes nothing in ledger state."

// Markdown renders a deterministic Markdown reflection of the projection.
// The projection's own digest is included so a reader can confirm which
// exact JSON content this file reflects.
func (p Projection) Markdown() (string, error) {
	var b strings.Builder

	title := projectionTitles[p.Kind]
	if title == "" {
		title = string(p.Kind)
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	b.WriteString(markdownNotice)
	b.WriteString("\n\n")

	b.WriteString("| field | value |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| projection | `%s` |\n", mdEscape(string(p.Kind)))
	if p.Subject != "" {
		fmt.Fprintf(&b, "| subject | `%s` |\n", mdEscape(p.Subject))
	}
	fmt.Fprintf(&b, "| digest | `%s` |\n", mdEscape(p.Digest))
	fmt.Fprintf(&b, "| items | %d |\n", len(p.Items))
	b.WriteString("\n")

	if p.Summary != nil {
		summary, err := renderSummary(*p.Summary)
		if err != nil {
			return "", err
		}
		b.WriteString(summary)
	}

	if len(p.Items) == 0 {
		b.WriteString("_No records._\n")
		return b.String(), nil
	}

	b.WriteString("## Records\n\n")
	b.WriteString("| id | type | authority | origin | subject_ref | created_at |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, rec := range p.Items {
		subjectRef := rec.SubjectRef.String()
		if subjectRef == "" {
			subjectRef = "-"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s:%s | %s | %s |\n",
			mdEscape(rec.ID),
			mdEscape(string(rec.RecordType)),
			mdEscape(string(rec.Authority)),
			mdEscape(string(rec.Origin.Kind)), mdEscape(rec.Origin.ActorID),
			mdEscape(subjectRef),
			rec.CreatedAt.UTC().Format(time.RFC3339))
	}
	b.WriteString("\n")

	for _, rec := range p.Items {
		fmt.Fprintf(&b, "### %s\n\n", rec.ID)
		data, err := canonicalPrettyJSON(rec.Data)
		if err != nil {
			return "", fmt.Errorf("ledger: render markdown for record %s: %w", rec.ID, err)
		}
		fmt.Fprintf(&b, "```json\n%s\n```\n\n", data)
	}

	return b.String(), nil
}

// renderSummary renders a DeliverySummary's counted state. Every count field
// is written explicitly rather than through a generic marshal, so the
// ordering on the page is the same fixed order every time regardless of
// struct field order or any encoding library's own conventions; the three
// map[string]int fields are rendered through renderCountMap, which sorts.
func renderSummary(s DeliveryCounts) (string, error) {
	var b strings.Builder
	b.WriteString("## Delivery Summary\n\n")

	rows := []struct {
		field string
		value string
	}{
		{"run_id", orDash(s.RunID)},
		{"live_records", strconv.Itoa(s.LiveRecords)},
		{"superseded_records", strconv.Itoa(s.SupersededRecords)},
		{"confirmed_claims", strconv.Itoa(s.ConfirmedClaims)},
		{"rejected_claims", strconv.Itoa(s.RejectedClaims)},
		{"undecided_claims", strconv.Itoa(s.UndecidedClaims)},
		{"open_assumptions", strconv.Itoa(s.OpenAssumptions)},
		{"open_questions", strconv.Itoa(s.OpenQuestions)},
		{"blocking_open_questions", strconv.Itoa(s.BlockingOpenQuestions)},
		{"evidence_records", strconv.Itoa(s.EvidenceRecords)},
		{"results_awaiting_review", strconv.Itoa(s.ResultsAwaitingReview)},
		{"completed_unverified_tasks", strconv.Itoa(s.CompletedUnverifiedTasks)},
	}
	b.WriteString("| field | value |\n| --- | --- |\n")
	for _, row := range rows {
		fmt.Fprintf(&b, "| %s | %s |\n", row.field, mdEscape(row.value))
	}
	b.WriteString("\n")

	b.WriteString(renderCountMap("Tasks by Status", s.TasksByStatus))
	b.WriteString(renderCountMap("Tasks by Assurance State", s.TasksByAssurance))
	b.WriteString(renderCountMap("Evidence by Completeness", s.EvidenceByCompleteness))

	return b.String(), nil
}

// renderCountMap renders one of DeliveryCounts' map[string]int fields as a
// two-column table, keys sorted lexicographically. An empty map renders
// nothing at all, so an unpopulated axis does not leave a heading over an
// empty table.
func renderCountMap(title string, counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n\n", title)
	b.WriteString("| key | count |\n| --- | --- |\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "| %s | %d |\n", mdEscape(k), counts[k])
	}
	b.WriteString("\n")
	return b.String()
}

// canonicalPrettyJSON renders raw as indented JSON with object keys sorted —
// contracts.CanonicalJSON does the sorting (the same canonicalization the
// record's own digest is computed over), json.Indent only adds whitespace on
// top of already-canonical bytes, so it cannot reintroduce disorder.
func canonicalPrettyJSON(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	canonical, err := contracts.CanonicalJSON(raw)
	if err != nil {
		return "", err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, canonical, "", "  "); err != nil {
		return "", err
	}
	return pretty.String(), nil
}

// mdEscape neutralizes characters that would otherwise break a Markdown
// table cell or fence: a literal pipe would end the cell early, a backslash
// would escape whatever follows it, and an embedded newline would end the
// row.
func mdEscape(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// orDash renders an optional string field as "-" rather than leaving a
// table cell blank, so the cell's absence is legible rather than ambiguous
// with a rendering error.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
