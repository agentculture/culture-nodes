package api

import (
	"encoding/json"
	"testing"
)

// The ticket back-link is rendered as an href on a page an operator clicks
// (task t18, spec c10). The fact it is composed from arrives from a sweep
// reading a configured Jira site — trusted enough to display, not trusted
// enough to hand the browser a scheme. These cases pin the boundary; they
// need no database, so a regression here fails on a laptop, not only under
// the Postgres suite next door.
func TestTicketBackLinkComposition(t *testing.T) {
	run := func(input string) []RunOut {
		return []RunOut{{Input: json.RawMessage(input)}}
	}
	cases := []struct {
		name string
		runs []RunOut
		want string
	}{
		{"details_url as the sweep writes it",
			run(`{"source":"jira","id":"SCRUM-6","details_url":"https://jira.example.test/browse/SCRUM-6"}`),
			"https://jira.example.test/browse/SCRUM-6"},
		{"jira_site composes the browse URL",
			run(`{"jira_site":"jira.example.test"}`),
			"https://jira.example.test/browse/SCRUM-6"},
		{"details_url wins over jira_site",
			run(`{"details_url":"https://one.example.test/browse/SCRUM-6","jira_site":"two.example.test"}`),
			"https://one.example.test/browse/SCRUM-6"},
		{"a javascript: details_url is refused, not rendered",
			run(`{"details_url":"javascript:alert(1)"}`), ""},
		{"a scheme-less details_url is refused",
			run(`{"details_url":"/browse/SCRUM-6"}`), ""},
		{"a jira_site that is a URL rather than a host is refused",
			run(`{"jira_site":"https://jira.example.test/"}`), ""},
		{"runs arrive newest-first, so the newest run's fact wins",
			[]RunOut{
				{Input: json.RawMessage(`{"details_url":"https://new.example.test/browse/SCRUM-6"}`)},
				{Input: json.RawMessage(`{"details_url":"https://old.example.test/browse/SCRUM-6"}`)},
			},
			"https://new.example.test/browse/SCRUM-6"},
		{"a run with no fact is skipped, not treated as an answer",
			[]RunOut{
				{Input: json.RawMessage(`{"source":"jira","id":"SCRUM-6"}`)},
				{Input: json.RawMessage(`{"details_url":"https://old.example.test/browse/SCRUM-6"}`)},
			},
			"https://old.example.test/browse/SCRUM-6"},
		{"no run says where the ticket lives", run(`{"subject":"nothing jira-ish"}`), ""},
		{"unparsable input is skipped, never fatal", run(`not json`), ""},
		{"no runs at all", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ticketBackLink("SCRUM-6", tc.runs); got != tc.want {
				t.Fatalf("ticketBackLink = %q, want %q", got, tc.want)
			}
		})
	}
}

// A posted frame is the fallback source, and it is held to the same scheme
// rule: authored content is not a reason to relax it.
func TestTicketFrameBackLink(t *testing.T) {
	cases := []struct {
		name  string
		frame *TicketFrameOut
		want  string
	}{
		{"no frame", nil, ""},
		{"ticket_url", &TicketFrameOut{Frame: json.RawMessage(`{"ticket_url":"https://j.example.test/browse/X-1"}`)},
			"https://j.example.test/browse/X-1"},
		{"jira_url is the older spelling", &TicketFrameOut{Frame: json.RawMessage(`{"jira_url":"https://j.example.test/browse/X-1"}`)},
			"https://j.example.test/browse/X-1"},
		{"javascript: refused", &TicketFrameOut{Frame: json.RawMessage(`{"ticket_url":"javascript:alert(1)"}`)}, ""},
		{"frame with no link", &TicketFrameOut{Frame: json.RawMessage(`{"claims":[]}`)}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ticketFrameBackLink(tc.frame); got != tc.want {
				t.Fatalf("ticketFrameBackLink = %q, want %q", got, tc.want)
			}
		})
	}
}

// A task whose request will not parse, or declares no outcomes, is still
// SHOWN — with an empty (never absent) outcome list, so a page can say "this
// is waiting on you and offers nothing to click" rather than silently
// dropping it.
func TestTicketPendingTaskShapesAnUndecidableTask(t *testing.T) {
	got := ticketPendingTask(HumanTaskOut{ID: "ht-1", RunID: "run-1", Kind: "schedule_failing",
		Request: json.RawMessage(`{"schedule_id":"sch-1"}`)}, 7)
	if got.AllowedOutcomes == nil || len(got.AllowedOutcomes) != 0 {
		t.Fatalf("allowed_outcomes = %#v, want an empty non-nil slice", got.AllowedOutcomes)
	}
	if got.LedgerVersion != 7 || got.ID != "ht-1" || got.Kind != "schedule_failing" {
		t.Fatalf("shaped task = %+v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"allowed_outcomes":[]`; !containsSubstring(string(encoded), want) {
		t.Fatalf("encoded task %s, want it to carry %s", encoded, want)
	}
}

// pendingTicketTasks reads each RUN's version once, not each task's: two
// tasks on one run must be decided against the same frame.
func TestPendingTicketTasksReadsOneVersionPerRun(t *testing.T) {
	reads := 0
	tasks := []HumanTaskOut{
		{ID: "a", RunID: "run-1", Status: "pending", Request: json.RawMessage(`{"allowed_outcomes":["approved"]}`)},
		{ID: "b", RunID: "run-1", Status: "pending", Request: json.RawMessage(`{"allowed_outcomes":["approved"]}`)},
		{ID: "c", RunID: "run-1", Status: "decided", Request: json.RawMessage(`{"allowed_outcomes":["approved"]}`)},
	}
	out, err := pendingTicketTasks(tasks, func(string) (int64, error) { reads++; return 4, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("pending tasks = %+v, want the two pending ones", out)
	}
	if reads != 1 {
		t.Fatalf("ledger version reads = %d, want 1 per run", reads)
	}
	for _, task := range out {
		if task.LedgerVersion != 4 {
			t.Fatalf("task %s ledger_version = %d, want 4", task.ID, task.LedgerVersion)
		}
	}
}

func containsSubstring(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
