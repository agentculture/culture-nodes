package api

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// The ticket page's decidable half (task t18, spec c6/c10, plan decision q1).
//
// Until this file existed, GET /v1alpha1/tickets/{id} returned the ticket's
// human tasks as raw `human_tasks` rows: a caller that wanted to OFFER the
// decision had to re-parse each task's `request` blob to find the outcomes
// the engine would accept, and had no way at all to link back to the board.
// The Decisions view did that parse in TypeScript; the ticket page did not
// exist as a decision surface at all, which is why a Jira comment could name
// options that the page it linked to would not show.
//
// Two derivations live here, both pure and both read-time:
//
//   - ticketPendingTask shapes ONE pending task into what a decider needs to
//     act — the outcomes the engine will accept, the schema its response is
//     validated against, its deadline — read back out of the task's own
//     recorded request. Never re-derived from the workflow: the request is
//     the record of what the human was actually shown (§9.9).
//   - ticketBackLink composes the ticket's Jira URL from the work-item fact
//     the ticket's runs carry, so the page can always get back to the board.

// ticketPendingTaskRequest is the subset of the recorded human-task request
// this projection reads. It deliberately mirrors internal/engine's
// (unexported) humanTaskRequest field-for-field rather than importing it:
// these three fields are a WIRE contract the ticket page reads, and the
// engine's struct is free to grow fields that are none of this page's
// business.
type ticketPendingTaskRequest struct {
	DecisionSchemaRef string     `json:"decision_schema_ref"`
	Deadline          *time.Time `json:"deadline"`
	AllowedOutcomes   []string   `json:"allowed_outcomes"`
}

// TicketPendingTaskOut is one pending human task on a ticket, shaped for the
// surface that decides it.
//
// RunID and LedgerVersion are not decoration. A decision is committed
// through the same stale-guarded review transaction a claim review is
// (PRD §10.8): POST /human-tasks/{id}/decision refuses unless
// expected_ledger_version equals the run's current version. The version a
// decider submits must therefore be the one the page they read was rendered
// from — so it is served WITH the task rather than fetched by the client in
// a second, later request that could straddle an append.
type TicketPendingTaskOut struct {
	ID    string `json:"id"`
	RunID string `json:"run_id"`
	Kind  string `json:"kind"`
	// AllowedOutcomes is exactly the set DecideHumanTask will accept, and
	// is always present — an empty array means this task declares none (an
	// alert rather than a choice, e.g. `schedule_failing`), which a caller
	// must be able to tell apart from "the field was omitted".
	AllowedOutcomes   []string   `json:"allowed_outcomes"`
	DecisionSchemaRef string     `json:"decision_schema_ref,omitempty"`
	Deadline          *time.Time `json:"deadline,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	LedgerVersion     int64      `json:"ledger_version"`
}

// ticketPendingTask shapes one pending task. A request that will not parse
// yields a task with no outcomes rather than an error: the task IS pending
// and a human should still see that it exists, on a page that then honestly
// offers nothing to click.
func ticketPendingTask(task HumanTaskOut, ledgerVersion int64) TicketPendingTaskOut {
	out := TicketPendingTaskOut{
		ID:              task.ID,
		RunID:           task.RunID,
		Kind:            task.Kind,
		AllowedOutcomes: []string{},
		CreatedAt:       task.CreatedAt,
		LedgerVersion:   ledgerVersion,
	}
	var request ticketPendingTaskRequest
	if len(task.Request) == 0 || json.Unmarshal(task.Request, &request) != nil {
		return out
	}
	if request.AllowedOutcomes != nil {
		out.AllowedOutcomes = request.AllowedOutcomes
	}
	out.DecisionSchemaRef = request.DecisionSchemaRef
	out.Deadline = request.Deadline
	return out
}

// ticketBackLink is the ticket's URL on the board, composed from the Jira
// work-item fact its own runs carry (`details_url`, or `jira_site` plus the
// key — sweep.py's jira_work_items writes the first, and the second is the
// configuration the first is built from).
//
// Runs arrive newest-first, and the first run that carries a usable fact
// wins: a re-read of the ticket is the most recent statement of where it
// lives.
//
// Every candidate is validated before it is returned, because this value is
// rendered as an href. A fact reaches this control plane from a sweep
// reading a configured Jira site — not a hostile source, but not one this
// process controls either, and "the ticket page will render whatever string
// a fact puts in details_url" is a `javascript:` link away from being a
// defect. http and https only, and a host is required.
func ticketBackLink(ticketID string, runs []RunOut) string {
	for _, run := range runs {
		var fact struct {
			DetailsURL string `json:"details_url"`
			JiraSite   string `json:"jira_site"`
		}
		if len(run.Input) == 0 || json.Unmarshal(run.Input, &fact) != nil {
			continue
		}
		if link := safeTicketURL(fact.DetailsURL); link != "" {
			return link
		}
		if link := jiraBrowseURL(fact.JiraSite, ticketID); link != "" {
			return link
		}
	}
	return ""
}

// safeTicketURL returns u if it is an absolute http(s) URL with a host, and
// "" otherwise.
func safeTicketURL(u string) string {
	trimmed := strings.TrimSpace(u)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return trimmed
}

// jiraBrowseURL builds https://<site>/browse/<key> from a Jira site HOST —
// the same shape, and the same "a host is not a URL" rule, that sweep.py
// enforces on `jira_site` before it ever emits a fact.
func jiraBrowseURL(site, ticketID string) string {
	site = strings.TrimSpace(site)
	if site == "" || ticketID == "" || strings.ContainsAny(site, "/:? #") {
		return ""
	}
	return "https://" + site + "/browse/" + url.PathEscape(ticketID)
}

// ticketFrameBackLink is the fallback: a ticket whose runs carry no Jira
// fact, but whose posted frame states where it lives. It is second, not
// first, because a frame is authored content and the fact is measured.
func ticketFrameBackLink(frame *TicketFrameOut) string {
	if frame == nil || len(frame.Frame) == 0 {
		return ""
	}
	var doc struct {
		TicketURL string `json:"ticket_url"`
		JiraURL   string `json:"jira_url"`
	}
	if json.Unmarshal(frame.Frame, &doc) != nil {
		return ""
	}
	if link := safeTicketURL(doc.TicketURL); link != "" {
		return link
	}
	return safeTicketURL(doc.JiraURL)
}

// runLedgerVersions memoizes a ledger-version reader for the life of ONE
// response.
//
// A ticket's decidable surface has two halves — the human tasks waiting on
// a decision, and the undecided ledger claims — and both submit against the
// same run's version. Reading it twice would let one response quote two
// versions for the same run, and the decider would have no way to know
// which of the two the guard is going to measure them against. Reading it
// once means the page states one version per run, or refuses.
func runLedgerVersions(read func(runID string) (int64, error)) func(string) (int64, error) {
	seen := map[string]int64{}
	return func(runID string) (int64, error) {
		if v, ok := seen[runID]; ok {
			return v, nil
		}
		v, err := read(runID)
		if err != nil {
			return 0, err
		}
		seen[runID] = v
		return v, nil
	}
}

// pendingTicketTasks shapes every pending task in tasks, reading each run's
// current ledger version once. version is injected so this stays testable
// without a store; handleGetTicket passes the real reader.
func pendingTicketTasks(tasks []HumanTaskOut, version func(runID string) (int64, error)) ([]TicketPendingTaskOut, error) {
	out := make([]TicketPendingTaskOut, 0, len(tasks))
	versions := map[string]int64{}
	for _, task := range tasks {
		if task.Status != engine.HumanTaskStatusPending {
			continue
		}
		v, seen := versions[task.RunID]
		if !seen {
			read, err := version(task.RunID)
			if err != nil {
				return nil, err
			}
			versions[task.RunID], v = read, read
		}
		out = append(out, ticketPendingTask(task, v))
	}
	return out, nil
}
