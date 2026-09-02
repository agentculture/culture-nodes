package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// The ticket page's claim-deciding half (task t14, spec c11/c40).
//
// GET /v1alpha1/tickets/{id} serves a ticket's undecided claims grouped by
// run, each group quoted at the ledger version that response read. This is
// the route that answers back — and its shape is dictated by a property of
// the ledger rather than by convenience: a review is opened against ONE run
// at ONE stated version (PRD §10.8), so a ticket whose claims live on three
// runs is three reviews, not one.
//
// That makes partial success the normal case, not an edge case. If the
// batch were all-or-nothing, one run whose ledger moved between the page
// render and the click would discard the decider's verdicts on every other
// run and make them read the whole page again. So each run is committed in
// its own transaction, in the order the caller sent, and each reports its
// own outcome: committed, conflict, or error (decision c40).
//
// What is NOT per-run is anything the whole request would be wrong without.
// A missing rationale, a reviewer who is not a registered human, a verdict
// this surface does not understand, a run this ticket does not own: every
// one of those is refused before the first review is created, so a bad
// request cannot leave half a ticket decided. The reviewer check in
// particular is the ledger's own (ledger.RequireHumanReviewer) rather than a
// copy of its rule — discovering at run three that the reviewer was never a
// human would leave runs one and two decided by an actor that may not
// decide anything.
//
// The commit itself is ledger.CreateReviewRequest + ledger.CommitReview,
// the same pair POST /v1alpha1/runs/{id}/reviews and POST
// /v1alpha1/reviews/{id}/commit drive. Nothing about authority, staleness,
// coverage or supersession is re-decided here.

// Verdict words this surface accepts. They are the decider's vocabulary —
// the state a record ends up in — while ledger.Verdict is the action taken
// on it, and the two are mapped in one place rather than being assumed to
// be spelled the same.
const (
	ticketVerdictConfirmed = "confirmed"
	ticketVerdictRejected  = "rejected"
)

// Per-run outcome words. `conflict` is specifically "the ledger moved under
// you, no decision was written" — it is not an error the decider caused, and
// it is the one outcome a page should answer by re-reading and offering the
// decision again.
//
// "No decision" is narrower than "nothing", and the difference is reported
// rather than hidden. The pair below is two ledger transactions: the request
// commits first, and PRD §10.8's promise is that a stale COMMIT applies
// nothing and leaves that request at `requested`
// (ledger.TestCommitReviewRejectsAStaleLedgerAndAppliesNothing pins it). So
// a conflict can leave an open review request behind, and this route names
// it in `review_id` so the decider can retry that review instead of
// discovering an orphan later (#274 review, Qodo finding 2). Collapsing the
// two into one transaction was the alternative; it was not taken, because
// the two-step shape is the ledger's own contract, shared with POST
// /v1alpha1/runs/{id}/reviews, and re-deciding it under one route is how the
// two surfaces would come to mean different things by "conflict".
const (
	ticketReviewCommitted = "committed"
	ticketReviewConflict  = "conflict"
	ticketReviewError     = "error"
)

// ticketReviewRunRequest is one run's verdict inside a batch.
type ticketReviewRunRequest struct {
	RunID string `json:"run_id"`
	// ExpectedLedgerVersion is the version the page that rendered these
	// records was read at — GET /v1alpha1/tickets/{id}'s
	// `pending_records[].ledger_version`, not a version fetched separately.
	ExpectedLedgerVersion int64    `json:"expected_ledger_version"`
	Records               []string `json:"records"`
	Verdict               string   `json:"verdict"`
}

// ticketReviewsRequest is components.schemas.TicketReviewsRequest.
type ticketReviewsRequest struct {
	Runs []ticketReviewRunRequest `json:"runs"`
	// Rationale is why the reviewer decided this way, recorded on every
	// review record every run in the batch appends. Required, for the
	// reason POST /v1alpha1/reviews/{id}/commit requires it: a confirmation
	// with no stated reason cannot be told apart from an unread one.
	Rationale string `json:"rationale"`
	// ReviewerActorID names the deciding human when no principal is
	// resolved. With a principal it is ignored in favour of the
	// principal's own actor, exactly as every other write route on this
	// ticket treats its free-text identity field (task t10).
	ReviewerActorID string `json:"reviewer_actor_id"`
}

// TicketReviewRunResultOut is one run's outcome.
type TicketReviewRunResultOut struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
	// ReviewID is the review this run's outcome belongs to. On `committed`
	// it is the committed review. It is ALSO present when the commit half
	// failed after the request half succeeded: the two are separate ledger
	// transactions (PRD §10.8 pins that a stale commit applies nothing and
	// leaves the request at `requested`), so a conflict here does not undo
	// the open review request it was going to commit. Naming it is what
	// keeps that request findable — GET /v1alpha1/reviews/{id} reads it and
	// POST /v1alpha1/reviews/{id}/commit retries it at a fresh version
	// (#274 review, Qodo finding 2). Empty means no review was created at
	// all, which is every failure of the request half.
	ReviewID string `json:"review_id,omitempty"`
	// LedgerVersion is the run's version AFTER the commit, present only on
	// `committed` — what a caller re-submitting against this run next
	// would use without re-reading the whole ticket.
	LedgerVersion int64 `json:"ledger_version,omitempty"`
	// Message states what happened in the decider's terms on anything that
	// is not a clean commit. A conflict with no message tells them nothing.
	Message string `json:"message,omitempty"`
}

// TicketReviewsResultOut is components.schemas.TicketReviewsResult.
type TicketReviewsResultOut struct {
	TicketID string                     `json:"ticket_id"`
	Runs     []TicketReviewRunResultOut `json:"runs"`
	// CommittedRuns is how many of them committed. The response status is
	// 200 whenever the batch was well-formed enough to attempt — a partial
	// outcome is this route's normal answer, not a failure — so a caller
	// that wants one number to branch on reads this rather than the status
	// line, and a caller that wants to know WHICH reads `runs`.
	CommittedRuns int `json:"committed_runs"`
}

// handleTicketReviews is POST /v1alpha1/tickets/{id}/reviews.
func (s *Server) handleTicketReviews(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDecisionAuth(r); err != nil {
		return err
	}

	var req ticketReviewsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest(
			"send {runs: [{run_id, expected_ledger_version, records, verdict}], rationale}",
			"decode ticket reviews request: %v", err)
	}
	// origin: resolved from principal
	var warning string
	req.ReviewerActorID, warning = principalActor(r, "reviewer_actor_id", req.ReviewerActorID)

	ticketID := r.PathValue("id")
	if err := s.checkTicketReviewsRequest(r, ticketID, req); err != nil {
		return err
	}

	out := TicketReviewsResultOut{TicketID: ticketID, Runs: make([]TicketReviewRunResultOut, 0, len(req.Runs))}
	for _, run := range req.Runs {
		result := s.commitTicketRunReview(r, req, run)
		if result.Status == ticketReviewCommitted {
			out.CommittedRuns++
		}
		out.Runs = append(out.Runs, result)
	}
	writeJSONWithWarning(w, http.StatusOK, out, warning)
	return nil
}

// checkTicketReviewsRequest refuses everything that would make the WHOLE
// request wrong, before a single review exists. See this file's header for
// why these are not per-run outcomes.
func (s *Server) checkTicketReviewsRequest(r *http.Request, ticketID string, req ticketReviewsRequest) error {
	if req.Rationale == "" {
		return badRequest(
			"state why in rationale — what you read, ran, or checked to reach these verdicts",
			"rationale must not be empty: a confirmation with no stated reason cannot be told apart from an unread one")
	}
	if req.ReviewerActorID == "" {
		return badRequest(
			"sign in, or name the human actor deciding these records in reviewer_actor_id; GET /v1alpha1/actors lists the registered ones",
			"reviewer_actor_id must not be empty: a confirmation nobody is accountable for is not a confirmation")
	}
	if len(req.Runs) == 0 {
		return badRequest("name at least one run to decide in runs[]", "a review batch with no runs decides nothing")
	}

	owned, err := s.ticketRuns(r.Context(), ticketID)
	if err != nil {
		return internalError(err)
	}
	ownedIDs := make(map[string]bool, len(owned))
	for _, run := range owned {
		ownedIDs[run.ID] = true
	}
	for _, run := range req.Runs {
		switch {
		case run.RunID == "":
			return badRequest("every entry in runs[] needs a run_id", "a run entry with no run id")
		case !ownedIDs[run.RunID]:
			// The ticket's own route decides the ticket's own runs. A run
			// this ticket does not own is decided through
			// POST /v1alpha1/runs/{id}/reviews, where the caller has to
			// name it deliberately.
			return badRequest(
				"decide a run this ticket owns; the ticket projection's pending_records lists them",
				"run %s is not one of ticket %s's runs", run.RunID, ticketID)
		case len(run.Records) == 0:
			return badRequest(
				"name the records this verdict decides in records[]",
				"run %s names no records: a verdict on nothing is not a decision", run.RunID)
		case run.Verdict != ticketVerdictConfirmed && run.Verdict != ticketVerdictRejected:
			return badRequest(
				"verdict must be \"confirmed\" or \"rejected\"",
				"run %s has verdict %q", run.RunID, run.Verdict)
		}
	}

	// The reviewer is the ledger's rule, asked once for the whole batch.
	if err := s.Ledger.RequireHumanReviewer(r.Context(), req.ReviewerActorID); err != nil {
		if errors.Is(err, ledger.ErrActorNotFound) {
			return badRequest(
				"name a registered actor; GET /v1alpha1/actors lists them",
				"reviewer %s is not a registered actor", req.ReviewerActorID)
		}
		return classify(err)
	}
	return nil
}

// commitTicketRunReview opens and commits ONE run's review, reporting what
// happened rather than aborting the batch.
func (s *Server) commitTicketRunReview(r *http.Request, req ticketReviewsRequest, run ticketReviewRunRequest) TicketReviewRunResultOut {
	out := TicketReviewRunResultOut{RunID: run.RunID}

	created, err := s.Ledger.CreateReviewRequest(r.Context(), run.RunID, run.Records, run.ExpectedLedgerVersion,
		ledger.WithReviewer(req.ReviewerActorID))
	if err != nil {
		out.Status, out.Message = ticketRunReviewFailure(err)
		return out
	}

	verdict := ledger.VerdictConfirm
	if run.Verdict == ticketVerdictRejected {
		verdict = ledger.VerdictReject
	}
	// The decision set has to cover exactly the records the request named,
	// which is what CreateReviewRequest just recorded — built from the
	// review's own targets rather than from the caller's list, so a
	// duplicate id in records[] cannot make the coverage check fail on a
	// request the ledger already normalised.
	decisions := make(map[string]ledger.Verdict, len(created.RecordIDs))
	for _, id := range created.RecordIDs {
		decisions[id] = verdict
	}

	result, err := s.Ledger.CommitReview(r.Context(), created.ID, decisions, run.ExpectedLedgerVersion,
		ledger.WithRationale(req.Rationale))
	if err != nil {
		out.Status, out.Message = ticketRunReviewFailure(err)
		// The request is already durable and still open. Report it rather
		// than let the caller read "conflict" as "nothing happened".
		out.ReviewID = created.ID
		return out
	}
	out.Status, out.ReviewID, out.LedgerVersion = ticketReviewCommitted, result.ReviewID, result.LedgerVersion
	return out
}

// ticketRunReviewFailure classifies one run's failure through the same
// classify() every other route reports a ledger error through, so a
// conflict here and a conflict on POST /v1alpha1/reviews/{id}/commit mean
// the same thing.
func ticketRunReviewFailure(err error) (string, string) {
	classified := classify(err)
	if classified.Status == http.StatusConflict {
		return ticketReviewConflict, classified.Body.Message
	}
	return ticketReviewError, classified.Body.Message
}
