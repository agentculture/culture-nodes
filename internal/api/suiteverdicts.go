package api

import (
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

	// The three fields task t32 adds, all of them inputs to the repair
	// routing and none of them to the verdict record itself. A verdict says
	// what a suite did; these say what a repair of it would need.
	//
	// RequiresGrants are the dispatch grants the SUITE needs beyond running
	// its own binary — the gate is the only party that knows its suite talks
	// to a database, and a lane whose posture grants no network egress
	// cannot verify one (#119).
	RequiresGrants []string `json:"requires_grants"`
	// ImplicatedPaths are paths the gate knows this failure involves, added
	// to the ones the control plane measured for the run's handover.
	ImplicatedPaths []string `json:"implicated_paths"`
	// RepairActorID overrides which lane a repair would go to. Empty means
	// the actor that authored the run's claims.
	RepairActorID string `json:"repair_actor_id"`
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
	// origin: resolved from principal
	var warning string
	req.ValidatorActorID, warning = principalActor(r, "validator_actor_id", req.ValidatorActorID)
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

	records, err := s.Ledger.Records(ctx, id)
	if err != nil {
		return internalError(err)
	}
	measured, _ := handover.Measured(records)
	evidenceID, measuredCommit := measured.RecordID, measured.CommitSHA
	if measuredCommit != "" && measuredCommit != req.CommitSHA {
		return badRequest(
			"re-run the suite against the commit the run handed over, or collect the handover again "+
				"(scripts/collect-handover.py <run-id>)",
			"this run's handover was measured at commit %s, but the verdict names %s: the suite ran against "+
				"something other than what this run handed over",
			measuredCommit, req.CommitSHA)
	}
	attemptID, err := s.recordedAttemptID(ctx, req.NodeRunRef, req.AttemptRef)
	if err != nil {
		return internalError(err)
	}

	record, err := handover.SuiteVerdict{
		RunID:             id,
		NodeRunID:         req.NodeRunRef,
		AttemptID:         attemptID,
		Suite:             req.Suite,
		Command:           req.Command,
		ExitCode:          *req.ExitCode,
		CommitSHA:         req.CommitSHA,
		Ref:               req.Ref,
		ValidatorActorID:  req.ValidatorActorID,
		ValidatorRevision: strconv.FormatInt(int64(validator.Revision), 10),
		EvidenceRecordID:  evidenceID,
		EvaluatedAt:       time.Now().UTC(),
	}.Record()
	if err != nil {
		return classify(err)
	}
	record, err = withAttemptRef(record, req.AttemptRef)
	if err != nil {
		return internalError(err)
	}

	appended, err := s.Ledger.Append(ctx, record)
	if err != nil {
		return classify(err)
	}

	// Task t32: a rejecting gate does not stop here. It is routed — to a
	// bounded repair attempt on a lane that can actually verify one, or to a
	// human — and the routing is recorded beside the verdict rather than
	// left to whoever happens to read the red tick. `records` is the ledger
	// as it stood BEFORE this verdict, which is exactly the history the
	// bound is counted over.
	routing, routingErr := s.routeGateFailure(ctx, id, appended, req, records, measured)

	writeJSONWithWarning(w, http.StatusCreated, suiteVerdictResult{
		Verdict:      appended,
		Routing:      routing,
		RoutingError: routingErr,
	}, warning)
	return nil
}
