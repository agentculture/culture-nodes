package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// POST /v1alpha1/runs/{id}/gate-reports — the whole merge gate's finding, in
// one transaction's worth of derived records (task t16, issue #101).
//
// # Why this is not just several suite-verdict calls
//
// suiteverdicts.go records ONE suite's exit code, and that endpoint is
// unchanged: this handler composes its records through the very same
// handover.SuiteVerdict, so a gate's per-suite rows are byte-identical to the
// ones an operator's `collect-handover.py --gate` writes today. Building on it
// was the point.
//
// What it cannot express is the two things a gate NODE has to say:
//
//  1. A gate that measured nothing. `exit_code` is required there and never
//     defaulted, precisely so a malformed request cannot become a pass — which
//     means there is no way to post "this instrument does not reach the
//     changed tree" at all. Posting exit 0 would be the false green the
//     endpoint exists to prevent, and posting exit 1 would manufacture a
//     defect the repair router would then act on.
//  2. The aggregate. A merge is decided on "did the gate pass", which is a
//     computation over the per-gate findings — and if the caller supplied it,
//     the caller could report a green gate over a report that measured
//     nothing. So the counts and the outcome are computed HERE, from the
//     statuses in the request, and there is no field for either.
//
// # What the caller may and may not say
//
// The caller states, per gate, either an exit code (it ran) or a
// not-applicable reason with the paths it did not cover (it did not). Naming
// both, or neither, is refused: those are the two shapes in which an
// unmeasured gate could pass itself off as a measured one.
//
// Everything else follows suiteverdicts.go exactly — the same decision secret
// (whoever can record a gate result can decide a merge), the same
// registered-and-not-human validator identity, and the same refusal when the
// verdict names a commit other than the one the control plane measured for
// this run's handover.
//
// # A failing gate is still routed
//
// Each rejecting per-gate verdict goes through the same routeGateFailure the
// single-suite endpoint uses (task t32, issue #102): a bounded repair on a
// lane whose advertised surface can actually run the failing suite, or a
// human. Nothing is dispatched — the control plane decides and records, and
// executing the routed dispatch stays a deliberate step while the bridge write
// path is unproven (#18).

// createGateReportRequest is components.schemas.CreateGateReportRequest.
type createGateReportRequest struct {
	// CommitSHA is the candidate the gates ran against, full 40-hex. BaseSHA
	// is what it was compared to; ChangedFiles is the set every applicability
	// decision below was made against.
	CommitSHA string `json:"commit_sha"`
	// PackageCommitSHA binds a post-merge candidate to the handover this run
	// measured. It is required when CommitSHA names the combination rather
	// than the package branch itself.
	PackageCommitSHA string   `json:"package_commit_sha"`
	BaseSHA          string   `json:"base_sha"`
	Ref              string   `json:"ref"`
	ChangedFiles     []string `json:"changed_files"`

	ValidatorActorID string `json:"validator_actor_id"`
	NodeRunRef       string `json:"node_run_ref"`
	AttemptRef       string `json:"attempt_ref"`

	Gates []gateReportEntry `json:"gates"`
}

// gateReportEntry is one declared gate's finding.
//
// ExitCode is a pointer for the reason createSuiteVerdictRequest's is, and one
// more besides: here absent does not mean "malformed", it means "this gate did
// not run", and the NotApplicable block is required to say why.
type gateReportEntry struct {
	Gate    string   `json:"gate"`
	Suite   string   `json:"suite"`
	Command []string `json:"command"`

	Instrument        string `json:"instrument"`
	InstrumentVersion string `json:"instrument_version"`

	ExitCode      *int                `json:"exit_code"`
	NotApplicable *gateNotApplicable  `json:"not_applicable"`
	Considered    []string            `json:"changed_files_considered"`
	Measurement   *gateMeasurement    `json:"measurement"`
	Repair        *gateRepairMetadata `json:"repair"`
}

// gateNotApplicable is why a gate measured nothing, and what it left uncovered.
type gateNotApplicable struct {
	Reason         string   `json:"reason"`
	UncoveredPaths []string `json:"uncovered_paths"`
}

// gateMeasurement is the number an applicable gate produced and the threshold
// it was compared against, carried through onto the per-gate record so a
// reader can check the verdict rather than take it. It is optional: a suite
// whose primary number IS its exit code has nothing to add.
type gateMeasurement struct {
	Value     *float64        `json:"value"`
	Unit      string          `json:"unit"`
	Threshold json.RawMessage `json:"threshold"`
}

// gateRepairMetadata is the per-gate half of task t32's routing inputs: what a
// repair of THIS gate would need, declared by the gate that knows.
type gateRepairMetadata struct {
	RequiresGrants  []string `json:"requires_grants"`
	ImplicatedPaths []string `json:"implicated_paths"`
	RepairActorID   string   `json:"repair_actor_id"`
}

// gateReportResult is the 201 body (components.schemas.GateReportResult).
//
// Outcome and ExitCode are echoed because they are the two values the gate
// program has to agree with the control plane about: the program exits with
// the code, the workflow routes on the outcome, and a caller that computed a
// different one should find out from the response rather than from a run that
// took the wrong edge.
type gateReportResult struct {
	Gates     []ledger.Record     `json:"gates"`
	Aggregate ledger.Record       `json:"aggregate"`
	Counts    handover.GateCounts `json:"counts"`
	Outcome   string              `json:"outcome"`
	ExitCode  int                 `json:"exit_code"`
	// Routings are the derived repair routings composed for the rejecting
	// gates, in the order those gates appear. Empty when nothing rejected.
	Routings []ledger.Record `json:"routings,omitempty"`
	// RoutingErrors are routings that were computed but could not be
	// recorded, stated rather than omitted (issue #120's lesson).
	RoutingErrors []string `json:"routing_errors,omitempty"`
}

func (s *Server) handleCreateGateReport(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDecisionAuth(r); err != nil {
		return err
	}

	id := r.PathValue("id")
	ctx := r.Context()

	if _, err := s.engineStore.Run(ctx, id); err != nil {
		return classify(err)
	}

	var req createGateReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest(
			"send a JSON body matching CreateGateReportRequest: {commit_sha, validator_actor_id, gates:[...]}",
			"decode request body: %v", err)
	}
	if len(req.Gates) == 0 {
		return badRequest(
			"list every gate the workflow declared, including the ones that measured nothing",
			"gates is empty: a report over zero gates has no counts to be counts of, and a merge decided on "+
				"one would be decided on nothing")
	}

	validator, err := s.engineStore.GetActor(ctx, req.ValidatorActorID)
	if err != nil {
		return classify(err)
	}
	if validator.Kind == ledger.ActorKindHuman {
		return badRequest(
			"name a non-human validator identity (a CI job, a gate runner) as validator_actor_id",
			"actor %s is registered as kind %q: a human is not a deterministic validator, so a gate result "+
				"attributed to one is a claim about a gate rather than the gate's own output (PRD §10.4)",
			req.ValidatorActorID, validator.Kind)
	}

	records, err := s.Ledger.Records(ctx, id)
	if err != nil {
		return internalError(err)
	}
	measured, _ := handover.Measured(records)
	measuredSubject := req.CommitSHA
	if req.PackageCommitSHA != "" {
		measuredSubject = req.PackageCommitSHA
	}
	if measured.CommitSHA != "" && measured.CommitSHA != measuredSubject {
		return badRequest(
			"re-run the gate against the commit the run handed over, or collect the handover again "+
				"(scripts/collect-handover.py <run-id>)",
			"this run's handover was measured at commit %s, but the gate report names %s: the gates ran "+
				"against something other than what this run handed over",
			measured.CommitSHA, measuredSubject)
	}

	revision := strconv.FormatInt(int64(validator.Revision), 10)
	evaluatedAt := time.Now().UTC()
	attemptID, err := s.recordedAttemptID(ctx, req.NodeRunRef, req.AttemptRef)
	if err != nil {
		return internalError(err)
	}

	// Compose EVERY record before appending any of them. A half-written gate
	// report is worse than a refused one: the records are immutable, so a
	// caller could not withdraw the rows that did land, and a reader would
	// count a report that never finished.
	composed, results, cerr := composeGateRecords(id, req, attemptID, revision, evaluatedAt, measured.RecordID)
	if cerr != nil {
		return cerr
	}
	aggregate := handover.GateAggregate{
		RunID:             id,
		NodeRunID:         req.NodeRunRef,
		AttemptID:         attemptID,
		Results:           results,
		BaseSHA:           req.BaseSHA,
		CommitSHA:         req.CommitSHA,
		Ref:               req.Ref,
		ChangedFiles:      req.ChangedFiles,
		ValidatorActorID:  req.ValidatorActorID,
		ValidatorRevision: revision,
		EvidenceRecordID:  measured.RecordID,
		EvaluatedAt:       evaluatedAt,
	}
	if err := aggregate.Validate(); err != nil {
		return classify(err)
	}

	out := gateReportResult{Gates: make([]ledger.Record, 0, len(composed))}
	for i, record := range composed {
		appended, aerr := s.Ledger.Append(ctx, record)
		if aerr != nil {
			return classify(aerr)
		}
		out.Gates = append(out.Gates, appended)
		results[i].RecordID = appended.ID
	}

	aggregate.Results = results
	aggregateRecord, err := aggregate.Record()
	if err != nil {
		return classify(err)
	}
	aggregateRecord, err = withAttemptRef(aggregateRecord, req.AttemptRef)
	if err != nil {
		return internalError(err)
	}
	appendedAggregate, err := s.Ledger.Append(ctx, aggregateRecord)
	if err != nil {
		return classify(err)
	}
	out.Aggregate = appendedAggregate
	out.Counts = results.Counts()
	out.Outcome = results.Outcome()
	out.ExitCode, _ = handover.GateExitCode(out.Outcome)

	s.routeGateReportFailures(ctx, id, req, out.Gates, results, records, measured, &out)

	writeJSON(w, http.StatusCreated, out)
	return nil
}

// composeGateRecords turns each declared gate into its ledger record and its
// status, refusing the whole report on the first thing it cannot compose.
func composeGateRecords(
	runID string, req createGateReportRequest, attemptID, revision string, evaluatedAt time.Time, evidenceID string,
) ([]ledger.Record, handover.GateResults, error) {
	composed := make([]ledger.Record, 0, len(req.Gates))
	results := make(handover.GateResults, 0, len(req.Gates))

	for i, gate := range req.Gates {
		ran := gate.ExitCode != nil
		skipped := gate.NotApplicable != nil
		switch {
		case ran && skipped:
			return nil, nil, badRequest(
				"state either the exit code the gate produced or the reason it produced none, never both",
				"gates[%d] (%q) names an exit code AND a not-applicable reason: one of the two is not true, "+
					"and a record carrying both would let a reader take whichever they preferred", i, gate.Gate)
		case !ran && !skipped:
			return nil, nil, badRequest(
				"send exit_code for a gate that ran, or not_applicable{reason, uncovered_paths} for one that "+
					"did not; an omitted exit code is not a passing one",
				"gates[%d] (%q) names neither an exit code nor a not-applicable reason, so it records no "+
					"finding at all — and a gate with no finding counted among the passes is the false green "+
					"this endpoint exists to prevent", i, gate.Gate)
		}

		var (
			record ledger.Record
			status string
			reason string
			err    error
		)
		if ran {
			record, err = handover.SuiteVerdict{
				RunID:             runID,
				NodeRunID:         req.NodeRunRef,
				AttemptID:         attemptID,
				Suite:             gateSuiteName(gate),
				Command:           gate.Command,
				ExitCode:          *gate.ExitCode,
				CommitSHA:         req.CommitSHA,
				Ref:               req.Ref,
				ValidatorActorID:  req.ValidatorActorID,
				ValidatorRevision: revision,
				EvidenceRecordID:  evidenceID,
				EvaluatedAt:       evaluatedAt,
			}.Record()
			status = handover.GateStatusPassed
			if *gate.ExitCode != 0 {
				status = handover.GateStatusFailed
			}
			if err == nil {
				record.Data, err = withGateAnnotations(record.Data, gate)
			}
		} else {
			reason = gate.NotApplicable.Reason
			status = handover.GateStatusNotApplicable
			record, err = handover.GateNotApplicable{
				RunID:                  runID,
				NodeRunID:              req.NodeRunRef,
				AttemptID:              attemptID,
				Gate:                   gate.Gate,
				Suite:                  gate.Suite,
				Command:                gate.Command,
				Instrument:             gate.Instrument,
				InstrumentVersion:      gate.InstrumentVersion,
				Reason:                 reason,
				UncoveredPaths:         gate.NotApplicable.UncoveredPaths,
				ChangedFilesConsidered: gate.Considered,
				CommitSHA:              req.CommitSHA,
				Ref:                    req.Ref,
				ValidatorActorID:       req.ValidatorActorID,
				ValidatorRevision:      revision,
				EvidenceRecordID:       evidenceID,
				EvaluatedAt:            evaluatedAt,
			}.Record()
		}
		if err != nil {
			return nil, nil, classify(err)
		}
		record, err = withAttemptRef(record, req.AttemptRef)
		if err != nil {
			return nil, nil, internalError(err)
		}

		composed = append(composed, record)
		results = append(results, handover.GateResult{Gate: gate.Gate, Status: status, Reason: reason})
	}
	return composed, results, nil
}

// gateSuiteName is what a suite verdict is filed under. The declared suite
// wins; the gate's own name is the fallback, because SuiteVerdict refuses an
// unnamed suite and "the gate matrix called it go-test" is a spelling a reader
// can act on.
func gateSuiteName(gate gateReportEntry) string {
	if gate.Suite != "" {
		return gate.Suite
	}
	return gate.Gate
}

// withGateAnnotations folds the gate's own identity and measurement onto a
// suite-verdict payload.
//
// The verdict record's shape is internal/handover's and stays that way — this
// only ADDS keys the gate matrix knows and a single suite run does not: which
// declared gate this was, which instrument produced it, the number it measured
// and the threshold it was measured against. The threshold travels with the
// result because it is pinned in the published workflow: a reader must be able
// to see that it was not chosen after the number was known.
func withGateAnnotations(payload []byte, gate gateReportEntry) ([]byte, error) {
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, fmt.Errorf("decode composed verdict payload: %w", err)
	}
	data["gate"] = gate.Gate
	data["gate_status"] = handover.GateStatusPassed
	if gate.ExitCode != nil && *gate.ExitCode != 0 {
		data["gate_status"] = handover.GateStatusFailed
	}
	for key, value := range map[string]string{
		"instrument":         gate.Instrument,
		"instrument_version": gate.InstrumentVersion,
	} {
		if value != "" {
			data[key] = value
		}
	}
	if len(gate.Considered) > 0 {
		data["changed_files_considered"] = gate.Considered
	}
	if gate.Measurement != nil {
		if gate.Measurement.Value != nil {
			data["value"] = *gate.Measurement.Value
		}
		if gate.Measurement.Unit != "" {
			data["unit"] = gate.Measurement.Unit
		}
		if len(gate.Measurement.Threshold) > 0 {
			data["threshold"] = json.RawMessage(gate.Measurement.Threshold)
		}
	}
	return json.Marshal(data)
}

// routeGateReportFailures runs task t32's routing for every rejecting gate,
// reusing the single-suite path verbatim so a red gate reaches the same
// bounded repair whether it was posted one at a time or as part of a report.
func (s *Server) routeGateReportFailures(
	ctx context.Context,
	runID string,
	req createGateReportRequest,
	appended []ledger.Record,
	results handover.GateResults,
	priorRecords []ledger.Record,
	measured handover.MeasuredHandover,
	out *gateReportResult,
) {
	for i, result := range results {
		if result.Status != handover.GateStatusFailed {
			continue
		}
		gate := req.Gates[i]
		verdictReq := createSuiteVerdictRequest{
			Suite:            gateSuiteName(gate),
			Command:          gate.Command,
			ExitCode:         gate.ExitCode,
			CommitSHA:        req.CommitSHA,
			Ref:              req.Ref,
			ValidatorActorID: req.ValidatorActorID,
			NodeRunRef:       req.NodeRunRef,
			AttemptRef:       req.AttemptRef,
		}
		if gate.Repair != nil {
			verdictReq.RequiresGrants = gate.Repair.RequiresGrants
			verdictReq.ImplicatedPaths = gate.Repair.ImplicatedPaths
			verdictReq.RepairActorID = gate.Repair.RepairActorID
		}
		routing, routingErr := s.routeGateFailure(ctx, runID, appended[i], verdictReq, priorRecords, measured)
		if routing != nil {
			out.Routings = append(out.Routings, *routing)
		}
		if routingErr != "" {
			out.RoutingErrors = append(out.RoutingErrors, routingErr)
		}
	}
}
