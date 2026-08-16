package repair_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/repair"
)

// A lane that can do everything a repair needs: it writes, and it can run the
// tool the failing suite is spelled with. Tests narrow it down field by field
// to reach each refusal.
func healthyLane() repair.Lane {
	return repair.Lane{
		ActorID:           "act_codex_thor",
		ActorKey:          "company/codex-thor",
		SurfaceAdvertised: true,
		Posture:           "workspace-write",
		Grants:            []string{"workspace-write", "tmp-write"},
		Toolchains: []repair.Toolchain{{
			Name:       "go",
			UsableIn:   []string{"workspace-write"},
			UnusableIn: map[string]string{"read-only": "nothing is writable in this mode"},
		}},
	}
}

func rejectingGate() repair.Gate {
	return repair.Gate{
		Suite:           "go test ./...",
		Command:         []string{"go", "test", "./..."},
		ExitCode:        1,
		CommitSHA:       strings.Repeat("a", 40),
		VerdictRecordID: "rec_verdict_1",
	}
}

func baseNow() time.Time { return time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC) }

func baseInput(now time.Time) repair.Input {
	return repair.Input{
		RunID:         "run_1",
		NodeRunID:     "nr_1",
		AttemptID:     "att_1",
		Gate:          rejectingGate(),
		Lane:          healthyLane(),
		RouterActorID: "act_gate_router",
		History:       repair.History{FirstRejectionAt: now},
		Now:           now,
	}
}

func TestGatePassRoutesNowhere(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	in := baseInput(now)
	in.Gate.ExitCode = 0

	routing := repair.Decide(in)

	if routing.Destination != repair.DestinationNone {
		t.Fatalf("destination = %q, want %q", routing.Destination, repair.DestinationNone)
	}
	if routing.Reason != repair.ReasonGatePassed {
		t.Fatalf("reason = %q, want %q", routing.Reason, repair.ReasonGatePassed)
	}
}

func TestFirstRejectionRoutesToABoundedRepair(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	routing := repair.Decide(baseInput(now))

	if routing.Destination != repair.DestinationRepair {
		t.Fatalf("destination = %q, want %q (%s)", routing.Destination, repair.DestinationRepair, routing.Narrative)
	}
	if routing.Reason != repair.ReasonWithinBound {
		t.Fatalf("reason = %q, want %q", routing.Reason, repair.ReasonWithinBound)
	}
	if routing.AttemptNumber != 1 {
		t.Fatalf("attempt number = %d, want 1", routing.AttemptNumber)
	}
	if routing.MaxAttempts != repair.MaxAttempts {
		t.Fatalf("max attempts = %d, want %d", routing.MaxAttempts, repair.MaxAttempts)
	}
	if routing.AttemptsRemaining != repair.MaxAttempts-1 {
		t.Fatalf("attempts remaining = %d, want %d", routing.AttemptsRemaining, repair.MaxAttempts-1)
	}
	if routing.LaneActorID != "act_codex_thor" {
		t.Fatalf("lane actor = %q, want act_codex_thor", routing.LaneActorID)
	}
}

// The ceiling, exercised one attempt at a time. The last row is the one that
// matters: a bound whose ceiling is never tested is a comment.
func TestTheCeilingIsEnforcedAndTheLastAttemptIsTheLast(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	for prior := 0; prior < repair.MaxAttempts; prior++ {
		in := baseInput(now)
		in.History.Attempts = prior
		routing := repair.Decide(in)
		if routing.Destination != repair.DestinationRepair {
			t.Fatalf("with %d prior attempts: destination = %q, want %q (%s)",
				prior, routing.Destination, repair.DestinationRepair, routing.Narrative)
		}
		if routing.AttemptNumber != prior+1 {
			t.Fatalf("with %d prior attempts: attempt number = %d, want %d", prior, routing.AttemptNumber, prior+1)
		}
		if routing.AttemptsRemaining != repair.MaxAttempts-prior-1 {
			t.Fatalf("with %d prior attempts: remaining = %d, want %d",
				prior, routing.AttemptsRemaining, repair.MaxAttempts-prior-1)
		}
	}

	in := baseInput(now)
	in.History.Attempts = repair.MaxAttempts
	routing := repair.Decide(in)

	if routing.Destination != repair.DestinationHuman {
		t.Fatalf("at the ceiling: destination = %q, want %q", routing.Destination, repair.DestinationHuman)
	}
	if routing.Reason != repair.ReasonCeilingReached {
		t.Fatalf("at the ceiling: reason = %q, want %q", routing.Reason, repair.ReasonCeilingReached)
	}
	if routing.AttemptsRemaining != 0 {
		t.Fatalf("at the ceiling: remaining = %d, want 0", routing.AttemptsRemaining)
	}
	// The narrative has to carry the number. An operator reading "went to a
	// human" without "after 2 of 2" cannot tell a ceiling from a crash.
	if !strings.Contains(routing.Narrative, "2 of 2") {
		t.Fatalf("ceiling narrative does not state the bound: %q", routing.Narrative)
	}
}

// A run whose gate has been rejecting for longer than the window stops being
// repairable even with attempts unspent.
func TestTheWindowIsEnforcedSeparatelyFromTheCeiling(t *testing.T) {
	first := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	in := baseInput(first.Add(repair.Window + time.Second))
	in.History = repair.History{Attempts: 0, FirstRejectionAt: first}

	routing := repair.Decide(in)

	if routing.Destination != repair.DestinationHuman {
		t.Fatalf("destination = %q, want %q", routing.Destination, repair.DestinationHuman)
	}
	if routing.Reason != repair.ReasonWindowExpired {
		t.Fatalf("reason = %q, want %q", routing.Reason, repair.ReasonWindowExpired)
	}
	if routing.AttemptsRemaining != repair.MaxAttempts {
		t.Fatalf("remaining = %d, want %d — the window expired, the budget did not",
			routing.AttemptsRemaining, repair.MaxAttempts)
	}
}

func TestTheWindowBoundaryItselfStillRepairs(t *testing.T) {
	first := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	in := baseInput(first.Add(repair.Window))
	in.History = repair.History{Attempts: 0, FirstRejectionAt: first}

	if routing := repair.Decide(in); routing.Destination != repair.DestinationRepair {
		t.Fatalf("exactly at the window edge: destination = %q, want %q (%s)",
			routing.Destination, repair.DestinationRepair, routing.Narrative)
	}
}

// The workflow-scope boundary. A repair dispatch is a dispatch, so it may not
// touch CI configuration — which makes a gate failure implicating .github/ a
// case the loop must hand to a person rather than attempt.
func TestAFailureImplicatingCIConfigurationGoesToAHuman(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	for _, path := range []string{
		".github/workflows/tests.yml",
		"./.github/workflows/tests.yml",
		".github/dependabot.yml",
		".github/",
	} {
		in := baseInput(now)
		in.ImplicatedPaths = []string{"internal/api/server.go", path}

		routing := repair.Decide(in)

		if routing.Destination != repair.DestinationHuman {
			t.Fatalf("%s: destination = %q, want %q", path, routing.Destination, repair.DestinationHuman)
		}
		if routing.Reason != repair.ReasonOutOfWorkflowScope {
			t.Fatalf("%s: reason = %q, want %q", path, routing.Reason, repair.ReasonOutOfWorkflowScope)
		}
		if len(routing.GuardedPaths) != 1 || routing.GuardedPaths[0] != strings.TrimPrefix(path, "./") {
			t.Fatalf("%s: guarded paths = %v, want the one guarded path named", path, routing.GuardedPaths)
		}
	}
}

// Out-of-scope is decided before the bound: a repair that could never have
// been legal must not be reported as one the budget ran out on.
func TestOutOfScopeIsDecidedBeforeTheCeiling(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	in := baseInput(now)
	in.History.Attempts = repair.MaxAttempts
	in.ImplicatedPaths = []string{".github/workflows/tests.yml"}

	if routing := repair.Decide(in); routing.Reason != repair.ReasonOutOfWorkflowScope {
		t.Fatalf("reason = %q, want %q — scope outranks the ceiling", routing.Reason, repair.ReasonOutOfWorkflowScope)
	}
}

// The recorded risk, mechanized: a lane that cannot run the suite that failed
// cannot verify its own repair, so it is not offered one.
func TestALaneThatCannotRunTheSuiteIsNotSentARepair(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	in := baseInput(now)
	in.Lane.Posture = "read-only"
	in.Lane.Grants = nil

	routing := repair.Decide(in)

	if routing.Destination != repair.DestinationHuman {
		t.Fatalf("destination = %q, want %q", routing.Destination, repair.DestinationHuman)
	}
	if routing.Reason != repair.ReasonLaneCannotWrite {
		t.Fatalf("reason = %q, want %q", routing.Reason, repair.ReasonLaneCannotWrite)
	}
}

func TestALaneWhoseToolchainIsUnusableCannotVerify(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	in := baseInput(now)
	in.Lane.Toolchains = []repair.Toolchain{{
		Name:       "go",
		UsableIn:   nil,
		UnusableIn: map[string]string{"workspace-write": "go: creating work dir: read-only file system"},
	}}

	routing := repair.Decide(in)

	if routing.Reason != repair.ReasonLaneCannotVerify {
		t.Fatalf("reason = %q, want %q", routing.Reason, repair.ReasonLaneCannotVerify)
	}
	// The lane's own words for why, not this package's guess at them.
	if !strings.Contains(routing.Narrative, "creating work dir") {
		t.Fatalf("narrative drops the lane's advertised reason: %q", routing.Narrative)
	}
}

// #119, mechanized: a suite that needs the network cannot be verified in a
// posture that grants none, even though the tool itself runs fine there.
func TestASuiteNeedingAGrantThePostureLacksCannotBeVerified(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	in := baseInput(now)
	in.Gate.RequiresGrants = []string{"network-egress"}

	routing := repair.Decide(in)

	if routing.Reason != repair.ReasonLaneCannotVerify {
		t.Fatalf("reason = %q, want %q", routing.Reason, repair.ReasonLaneCannotVerify)
	}
	if !strings.Contains(routing.Narrative, "network-egress") {
		t.Fatalf("narrative does not name the missing grant: %q", routing.Narrative)
	}
}

// Fail closed. A lane that advertised no capability surface has not been
// shown to be able to verify anything, and "I cannot tell" resolves the same
// way "unsafe" does — the discipline internal/engine's retryRefusal states.
func TestALaneThatAdvertisedNoSurfaceIsRefusedRatherThanAssumed(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	in := baseInput(now)
	in.Lane.SurfaceAdvertised = false

	routing := repair.Decide(in)

	if routing.Destination != repair.DestinationHuman {
		t.Fatalf("destination = %q, want %q", routing.Destination, repair.DestinationHuman)
	}
	if routing.Reason != repair.ReasonLaneUnknown {
		t.Fatalf("reason = %q, want %q", routing.Reason, repair.ReasonLaneUnknown)
	}
}

func TestNoLaneAtAllGoesToAHuman(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	in := baseInput(now)
	in.Lane = repair.Lane{}

	routing := repair.Decide(in)

	if routing.Reason != repair.ReasonNoLane {
		t.Fatalf("reason = %q, want %q", routing.Reason, repair.ReasonNoLane)
	}
}

// A tool the lane says nothing about is not a tool the lane can be shown to
// run. Same fail-closed rule as an absent surface.
func TestAnUnadvertisedToolchainCannotBeVerified(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	in := baseInput(now)
	in.Gate.Command = []string{"/usr/bin/pytest", "-n", "auto"}

	routing := repair.Decide(in)

	if routing.Reason != repair.ReasonLaneCannotVerify {
		t.Fatalf("reason = %q, want %q", routing.Reason, repair.ReasonLaneCannotVerify)
	}
	if !strings.Contains(routing.Narrative, "pytest") {
		t.Fatalf("narrative does not name the tool it could not find: %q", routing.Narrative)
	}
}

// --- the record -----------------------------------------------------------

func TestRecordIsDerivedFromAValidatorAndCarriesTheBound(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	in := baseInput(now)
	routing := repair.Decide(in)

	rec, err := routing.Record(in)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if rec.RecordType != ledger.RecordDecision {
		t.Fatalf("record type = %q, want %q", rec.RecordType, ledger.RecordDecision)
	}
	if rec.Authority != ledger.AuthorityDerived {
		t.Fatalf("authority = %q, want %q", rec.Authority, ledger.AuthorityDerived)
	}
	if rec.Origin.Kind != ledger.OriginValidator {
		t.Fatalf("origin kind = %q, want %q", rec.Origin.Kind, ledger.OriginValidator)
	}
	if rec.SubjectRef.String() != "rec_verdict_1" {
		t.Fatalf("subject ref = %v, want the verdict record", rec.SubjectRef)
	}

	var data map[string]any
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	for _, key := range []string{
		"selected", "options", "rationale", "bound", "attempt_number", "repair_lane_actor_id",
	} {
		if _, ok := data[key]; !ok {
			t.Fatalf("payload is missing %q: %v", key, data)
		}
	}
	bound, _ := data["bound"].(map[string]any)
	if got, want := bound["max_attempts"], float64(repair.MaxAttempts); got != want {
		t.Fatalf("bound.max_attempts = %v, want %v", got, want)
	}
	if got, want := bound["window_seconds"], float64(repair.Window/time.Second); got != want {
		t.Fatalf("bound.window_seconds = %v, want %v", got, want)
	}
	if bound["at_ceiling"] != "route to a human node" {
		t.Fatalf("bound.at_ceiling = %v, want the ceiling behaviour stated", bound["at_ceiling"])
	}
}

// The record must say, in its own payload, that nothing was dispatched. A
// routing a reader mistakes for an execution is worse than no routing.
func TestTheRecordStatesThatNothingWasDispatched(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	in := baseInput(now)
	rec, err := repair.Decide(in).Record(in)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if data["dispatched"] != false {
		t.Fatalf("dispatched = %v, want false", data["dispatched"])
	}
	if _, ok := data["dispatch_note"].(string); !ok {
		t.Fatalf("payload states no reason for not dispatching: %v", data)
	}
}

func TestRecordRefusesWithoutAnIdentifiedRouter(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	in := baseInput(now)
	in.RouterActorID = ""

	if _, err := repair.Decide(in).Record(in); err == nil {
		t.Fatal("an anonymous router composed a derived record; want a refusal")
	}
}

func TestRecordRefusesWithoutARun(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	in := baseInput(now)
	in.RunID = ""

	if _, err := repair.Decide(in).Record(in); err == nil {
		t.Fatal("a routing named no run; want a refusal")
	}
}

// --- reading the history back ---------------------------------------------

func routingRecord(t *testing.T, id string, destination repair.Destination) ledger.Record {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"question": "q",
		"selected": string(destination),
		"router":   repair.RouterCollectionMethod,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return ledger.Record{
		ID:         id,
		RecordType: ledger.RecordDecision,
		Authority:  ledger.AuthorityDerived,
		Origin:     ledger.Origin{Kind: ledger.OriginValidator, ActorID: "act_gate_router"},
		Data:       payload,
	}
}

func TestPriorAttemptsCountsOnlyRepairRoutings(t *testing.T) {
	records := []ledger.Record{
		routingRecord(t, "rec_1", repair.DestinationRepair),
		routingRecord(t, "rec_2", repair.DestinationHuman),
		routingRecord(t, "rec_3", repair.DestinationRepair),
		routingRecord(t, "rec_4", repair.DestinationNone),
		// A decision record from somewhere else entirely (a human task) must
		// not be counted as a repair round.
		{ID: "rec_5", RecordType: ledger.RecordDecision, Authority: ledger.AuthorityProposed,
			Origin: ledger.Origin{Kind: ledger.OriginHuman, ActorID: "act_human"},
			Data:   json.RawMessage(`{"selected":"repair"}`)},
	}

	if got := repair.PriorAttempts(records); got != 2 {
		t.Fatalf("PriorAttempts = %d, want 2", got)
	}
}

func TestPriorAttemptsIgnoresSupersededRoutings(t *testing.T) {
	superseding := routingRecord(t, "rec_2", repair.DestinationRepair)
	superseding.Supersedes = ledger.NullableID("rec_1")

	records := []ledger.Record{
		routingRecord(t, "rec_1", repair.DestinationRepair),
		superseding,
	}

	if got := repair.PriorAttempts(records); got != 1 {
		t.Fatalf("PriorAttempts = %d, want 1 — a superseded routing is not a spent attempt", got)
	}
}

func TestFirstRejectionAtAnchorsOnTheEarliestRoutingThatWentSomewhere(t *testing.T) {
	early := time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)
	late := time.Date(2026, 8, 16, 15, 0, 0, 0, time.UTC)

	repairing := routingRecord(t, "rec_2", repair.DestinationRepair)
	repairing.CreatedAt = late
	human := routingRecord(t, "rec_1", repair.DestinationHuman)
	human.CreatedAt = early
	// A routing that went nowhere is not a rejection and must not anchor the
	// window earlier than the first real one.
	none := routingRecord(t, "rec_0", repair.DestinationNone)
	none.CreatedAt = early.Add(-2 * time.Hour)

	got := repair.FirstRejectionAt([]ledger.Record{none, repairing, human})
	if !got.Equal(early) {
		t.Fatalf("FirstRejectionAt = %s, want %s", got, early)
	}
}

func TestFirstRejectionAtIsZeroWhenNothingHasBeenRouted(t *testing.T) {
	if got := repair.FirstRejectionAt(nil); !got.IsZero() {
		t.Fatalf("FirstRejectionAt = %s, want the zero time", got)
	}
}
