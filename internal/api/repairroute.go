package api

import (
	"context"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/repair"
)

// DefaultRepairRouterActorID is the producer identity a gate-failure routing
// is derived under.
//
// It names the ROUTER, not the process that happened to run it — the same
// argument preflight.DispatchGateActorID makes: a derived record's producer
// is the deterministic computation that produced it (PRD §10.4), and two API
// processes computing the same routing from the same records are the same
// producer in every sense a reader cares about.
//
// Like that one, it is a REGISTRATION OBLIGATION: ledger_records
// .origin_actor_id has a real foreign key to actors(id), so a deployment that
// has not registered this identity records no routings. That failure is
// reported on the response rather than swallowed — see routeGateFailure.
const DefaultRepairRouterActorID = "gate_repair_router"

// suiteVerdictResult is the 201 body of POST /v1alpha1/runs/{id}/
// suite-verdicts (components.schemas.SuiteVerdictResult).
//
// It carries two records rather than one because a rejecting gate produces
// two facts, and separating them across two round trips would put the
// operator back in the loop the routing exists to take them out of. Routing
// is null for a passing gate — there is nothing to route — and RoutingError
// is set, with Routing still null, when a routing was computed but could not
// be recorded.
//
// Both are stated explicitly, never by omission. Issue #120 is the cost of
// the alternative: a stale bridge and an honest refusal produced identical
// evidence, and it took an ssh to tell them apart.
type suiteVerdictResult struct {
	Verdict ledger.Record  `json:"verdict"`
	Routing *ledger.Record `json:"routing,omitempty"`
	// RoutingError is why the gate failure was NOT recorded as routed. It is
	// operator-facing text, not an API error: the verdict itself landed, and
	// failing the whole request over the derivation would lose the primary
	// record to protect a secondary one.
	RoutingError string `json:"routing_error,omitempty"`
}

// routeGateFailure decides where a rejecting gate goes next and records the
// decision (task t32, issue #102).
//
// It runs only for a REJECTING verdict. A passing gate returns (nil, "") and
// writes nothing: a ledger row per green gate saying "nothing to do" is noise
// that makes the rows that mean something harder to find.
//
// Every input is already-recorded fact — the verdict just appended, the run's
// own ledger, and the lane's registered capability surface — which is what
// makes the output `derived` rather than anybody's proposal.
func (s *Server) routeGateFailure(
	ctx context.Context,
	runID string,
	verdict ledger.Record,
	req createSuiteVerdictRequest,
	records []ledger.Record,
	measured handover.MeasuredHandover,
) (*ledger.Record, string) {
	if req.ExitCode == nil || *req.ExitCode == 0 {
		return nil, ""
	}

	lane := s.resolveRepairLane(ctx, req.RepairActorID, records)

	in := repair.Input{
		RunID:     runID,
		NodeRunID: req.NodeRunRef,
		AttemptID: req.AttemptRef,
		Gate: repair.Gate{
			Suite:           req.Suite,
			Command:         req.Command,
			ExitCode:        *req.ExitCode,
			CommitSHA:       req.CommitSHA,
			VerdictRecordID: verdict.ID,
			RequiresGrants:  req.RequiresGrants,
		},
		Lane: lane,
		History: repair.History{
			Attempts:         repair.PriorAttempts(records),
			FirstRejectionAt: repair.FirstRejectionAt(records),
		},
		// The paths the control plane MEASURED for this run's handover, plus
		// anything the gate named. The measured half is the one that matters:
		// it is how a failure on a commit that touched CI configuration is
		// recognised without anyone having to remember to declare it.
		ImplicatedPaths: append(append([]string(nil), measured.ChangedPaths...), req.ImplicatedPaths...),
		RouterActorID:   s.repairRouterActorID(),
		Now:             time.Now().UTC(),
	}

	record, err := repair.Decide(in).Record(in)
	if err != nil {
		return nil, err.Error()
	}
	appended, err := s.Ledger.Append(ctx, record)
	if err != nil {
		// Overwhelmingly this is the producer identity nobody registered,
		// which is a CONFIGURATION fault and reads as one. It is reported
		// rather than returned: the verdict is the primary record and losing
		// it to protect its derivation would be the worse trade.
		return nil, fmt.Sprintf(
			"the gate failure was not recorded as routed: %v. The router's producer identity %q must be a "+
				"registered actor (ledger_records.origin_actor_id references actors(id)); register it with "+
				"kind `validator` and no endpoint, or set the server's RepairRouterActorID to one that is",
			err, s.repairRouterActorID())
	}
	return &appended, ""
}

// resolveRepairLane answers "whose workspace is this failure in".
//
// The explicit override wins, because a deployment that repairs somewhere
// other than where the work was done must be able to say so. Otherwise it is
// the actor that authored the run's proposed claims — the party that has the
// checkout, the session history, and the defect. Reading it off the ledger
// costs no extra query: the handler has already fetched the records.
//
// A lane that resolves to nothing, or to an actor the store cannot return,
// yields the zero Lane, which Decide routes to a human. Nothing here guesses.
func (s *Server) resolveRepairLane(ctx context.Context, override string, records []ledger.Record) repair.Lane {
	actorID := override
	if actorID == "" {
		actorID = lastAgentActorID(records)
	}
	if actorID == "" {
		return repair.Lane{}
	}
	actor, err := s.engineStore.GetActor(ctx, actorID)
	if err != nil {
		// The actor id is on a record whose foreign key already guaranteed
		// it exists, so this is a transient store fault rather than a
		// missing row. Either way the lane is unknown, and an unknown lane
		// is refused rather than assumed.
		return repair.Lane{ActorID: actorID}
	}
	return repair.LaneFromCapabilities(actor.ID, actor.ActorKey, actor.Capabilities)
}

// lastAgentActorID is the actor behind the run's most recent proposed,
// agent-origin record — the session that did the work this gate rejected.
//
// The LAST one wins for the same reason handover.Measured's last match does:
// records are immutable and append-only, so the newest is the current one. A
// run whose work was re-dispatched to a different actor routes its repair at
// the actor that produced what is actually on the branch.
//
// A review record is skipped even when it is agent-origin and proposed: since
// login-from-anywhere task t11 the merge gate posts its verdicts under its
// own agent credential, and the gate's verdict is not the session whose work
// it judged. Routing a repair at the gate would send the failure back to the
// instrument that found it.
func lastAgentActorID(records []ledger.Record) string {
	actorID := ""
	for _, rec := range ledger.Live(records) {
		if rec.Origin.Kind != ledger.OriginAgent || rec.Authority != ledger.AuthorityProposed {
			continue
		}
		if rec.RecordType == ledger.RecordReview {
			continue
		}
		if rec.Origin.ActorID != "" {
			actorID = rec.Origin.ActorID
		}
	}
	return actorID
}

func (s *Server) repairRouterActorID() string {
	if s.repairRouterActor != "" {
		return s.repairRouterActor
	}
	return DefaultRepairRouterActorID
}
