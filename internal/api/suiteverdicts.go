package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// createSuiteVerdictRequest is components.schemas.CreateSuiteVerdictRequest
// (task t11, issue #101).
//
// ExitCode is a pointer, and that is the most load-bearing decision in this
// file. An omitted exit code decoding to 0 would mean every truncated,
// mis-serialised or half-written request recorded a PASS — the exact false
// green this endpoint exists to make impossible. Absent and zero must be
// distinguishable, so absent is a refusal.
type createSuiteVerdictRequest struct {
	Suite            string   `json:"suite"`
	Command          []string `json:"command"`
	ExitCode         *int     `json:"exit_code"`
	CommitSHA        string   `json:"commit_sha"`
	Ref              string   `json:"ref"`
	ValidatorActorID string   `json:"validator_actor_id"`
	NodeRunRef       string   `json:"node_run_ref"`
	AttemptRef       string   `json:"attempt_ref"`
}

// handleCreateSuiteVerdict is POST /v1alpha1/runs/{id}/suite-verdicts: the
// merge gate's mechanical half, recording what a suite did to a handed-over
// commit as a `derived` validator record (PRD §10.4).
//
// The endpoint exists because the alternative was an operator looking at a
// green tick. That is not evidence of anything — it is a person's report of
// a rendering, and it leaves nothing in the ledger that names the suite, the
// exit code, or the commit. A test suite IS a deterministic validator, and
// §10.4 admits derived records from exactly those, so the finding can be
// recorded by the thing that produced it.
//
// Everything about the record's SHAPE is internal/handover's
// (SuiteVerdict.Record — origin, authority, the computed verdict label, the
// commit-sha refusal). This handler decides only the three things that need
// the control plane:
//
//  1. Standing. The route is gated by the SAME decision secret the review
//     surface uses. Whoever can post a verdict here can decide a merge, which
//     is the same authority as deciding a claim; a third secret would only
//     mean a third thing to leak, and an open route would mean anyone on the
//     network could write "suite passed".
//  2. Producer identity. validator_actor_id must be a registered actor
//     (ledger_records.origin_actor_id is a foreign key, so this is enforced
//     twice over), and must not be a HUMAN. That refusal is
//     internal/ledger's checkReviewerIsHuman read backwards: t30 refuses a
//     non-human deciding a claim, and this refuses a human being written
//     down as a deterministic validator. A person who ran a suite and typed
//     the result is making a claim about it.
//  3. Subject. If the control plane has ALREADY measured this run's handover
//     (an observed evidence record from internal/handover's fetch), the
//     verdict must name that same commit, and it becomes the verdict's
//     subject and provenance. A verdict naming a different commit is refused
//     outright rather than recorded with a caveat: it is a suite that ran
//     against something else, and a ledger row saying "tested, but not what
//     this run handed over" is worse than the absence of one.
//
// When the control plane measured NO handover — which after issue #120 is
// most runs, because the deployed bridges predated the code that mints the
// ref — the verdict still lands, with no subject. It says what it tested and
// claims no corroboration it does not have.
func (s *Server) handleCreateSuiteVerdict(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDecisionAuth(r); err != nil {
		return err
	}

	id := r.PathValue("id")
	ctx := r.Context()

	if _, err := s.engineStore.Run(ctx, id); err != nil {
		return classify(err)
	}

	var req createSuiteVerdictRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest(
			"send a JSON body matching CreateSuiteVerdictRequest: {suite, exit_code, commit_sha, validator_actor_id}",
			"decode request body: %v", err)
	}
	if req.ExitCode == nil {
		return badRequest(
			"send the suite's exit_code explicitly; an omitted one is not a passing one",
			"exit_code is required: a verdict with no exit code records no finding, and defaulting it to 0 "+
				"would turn every malformed request into a pass")
	}

	validator, err := s.engineStore.GetActor(ctx, req.ValidatorActorID)
	if err != nil {
		return classify(err)
	}
	if validator.Kind == ledger.ActorKindHuman {
		return badRequest(
			"name a non-human validator identity (a CI job, a gate runner) as validator_actor_id",
			"actor %s is registered as kind %q: a human is not a deterministic validator, so a suite result "+
				"attributed to one is a claim about a suite rather than the suite's own output (PRD §10.4)",
			req.ValidatorActorID, validator.Kind)
	}

	evidenceID, measuredCommit, err := s.measuredHandover(ctx, id)
	if err != nil {
		return internalError(err)
	}
	if measuredCommit != "" && measuredCommit != req.CommitSHA {
		return badRequest(
			"re-run the suite against the commit the run handed over, or collect the handover again "+
				"(scripts/collect-handover.py <run-id>)",
			"this run's handover was measured at commit %s, but the verdict names %s: the suite ran against "+
				"something other than what this run handed over",
			measuredCommit, req.CommitSHA)
	}

	record, err := handover.SuiteVerdict{
		RunID:            id,
		NodeRunID:        req.NodeRunRef,
		AttemptID:        req.AttemptRef,
		Suite:            req.Suite,
		Command:          req.Command,
		ExitCode:         *req.ExitCode,
		CommitSHA:        req.CommitSHA,
		Ref:              req.Ref,
		ValidatorActorID:  req.ValidatorActorID,
		ValidatorRevision: strconv.FormatInt(int64(validator.Revision), 10),
		EvidenceRecordID:  evidenceID,
		EvaluatedAt:       time.Now().UTC(),
	}.Record()
	if err != nil {
		return classify(err)
	}

	appended, err := s.Ledger.Append(ctx, record)
	if err != nil {
		return classify(err)
	}

	writeJSON(w, http.StatusCreated, appended)
	return nil
}

// measuredHandover returns the id of this run's live handover-evidence
// record and the commit that record measured, or ("", "") when the control
// plane has fetched no handover for the run.
//
// The recognition rule itself is handover.MeasuredCommit's, not a copy of it
// here: it reads the payload internal/handover's own buildRecord writes, and
// a second opinion about those field names in this package would drift the
// first time either side changed. This handler's share is the read.
func (s *Server) measuredHandover(ctx context.Context, runID string) (string, string, error) {
	records, err := s.Ledger.Records(ctx, runID)
	if err != nil {
		return "", "", err
	}
	id, commit := handover.MeasuredCommit(records)
	return id, commit, nil
}
