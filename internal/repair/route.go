package repair

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// The bound. Both numbers are enforced by Decide, both are carried in the
// record it composes, and both have a test that walks them to their ceiling
// (TestTheCeilingIsEnforcedAndTheLastAttemptIsTheLast,
// TestTheWindowIsEnforcedSeparatelyFromTheCeiling).
//
// MaxAttempts is two rather than three because of what a repair round costs
// and what the evidence says it buys. Each round is a full cold session on a
// bridge — the most expensive lane rung in the ladder issue #48 records — and
// the nine hand-repaired packages in #87 were all one-shot fixes: a wrong
// indentation, a missing routing check, an unbounded body. A defect that
// survives two attempts by a lane that can run the suite is not a defect the
// same lane is going to find on the third; it is a defect somebody has to
// look at. Raising this number is a decision about spend, and it should be
// made by changing this constant and its test, not by an operator's patience.
//
// Window is measured from the run's FIRST gate rejection, not from the most
// recent one, so a loop cannot extend its own life by failing again. Twenty
// four hours matches internal/preflight.MaxWindow's reasoning at a different
// scale: a briefing composed against last week's host state must not
// authorize today's dispatch, and a repair aimed at yesterday's diagnosis
// must not be dispatched against today's tree.
const (
	MaxAttempts = 2
	Window      = 24 * time.Hour
)

// AtCeiling is what happens when either bound is reached, stated as a string
// because it goes in the record. A reader of a routing must not have to open
// this file to learn that the loop terminates.
const AtCeiling = "route to a human node"

// RouterCollectionMethod names how a routing here was produced, for the
// record's own field and for PriorAttempts' recognition rule. It is what
// keeps an unrelated `decision` record — a human task's answer, a devague
// deviation — from being counted as a repair round.
const RouterCollectionMethod = "gate_failure_routing"

// GuardedPathPrefixes are the repo-relative prefixes a repair dispatch may
// not touch, and therefore the prefixes whose involvement in a gate failure
// sends the failure to a person instead.
//
// It is DELIBERATELY WIDER than the enforcement boundary the bridges apply.
// `codex_bridge.scope_guard.GUARDED_PATH_PREFIXES` guards
// `.github/workflows/` exactly, and it is right to be narrow: it refuses work
// a session already did, so an over-inclusive prefix there destroys
// legitimate output (issue #98 is the record of that being got wrong in the
// other direction too). This list refuses to AUTOMATE, and its failure mode
// runs the other way — an over-inclusive entry sends a repairable failure to
// a human, which costs one person one look, while an under-inclusive one
// dispatches a session at a file it will be refused for touching, which costs
// a whole session and ends in a preserved branch nobody asked for. So the
// whole of `.github/` is out of scope here, not only its workflows.
var GuardedPathPrefixes = []string{".github/"}

// Destination is where a gate outcome goes next.
type Destination string

const (
	// DestinationNone is the gate passing: there is nothing to route.
	DestinationNone Destination = "none"
	// DestinationRepair is a bounded repair attempt on a named lane.
	DestinationRepair Destination = "repair"
	// DestinationHuman is a person. Every refusal in this package ends here
	// — there is no path that stalls, and none that silently stops.
	DestinationHuman Destination = "human"
)

// Reason is why the destination is what it is. Every value is a distinct
// operational situation with a distinct remedy, which is why this is an enum
// and not a sentence: #28 grades actors on these, and "it went to a human"
// with the reason folded away grades nothing.
type Reason string

const (
	ReasonGatePassed         Reason = "gate_passed"
	ReasonWithinBound        Reason = "within_bound"
	ReasonCeilingReached     Reason = "ceiling_reached"
	ReasonWindowExpired      Reason = "window_expired"
	ReasonOutOfWorkflowScope Reason = "out_of_workflow_scope"
	ReasonLaneCannotWrite    Reason = "lane_cannot_write"
	ReasonLaneCannotVerify   Reason = "lane_cannot_verify"
	ReasonLaneUnknown        Reason = "lane_capability_unknown"
	ReasonNoLane             Reason = "no_repair_lane"
)

// The two grants a repair depends on, in the vocabulary the bridges' shared
// preflight.py declares (GRANT_WORKSPACE_WRITE). A repair that cannot write
// cannot repair, so this one is checked unconditionally; everything else a
// suite needs is declared per-gate in Gate.RequiresGrants.
const grantWorkspaceWrite = "workspace-write"

// Toolchain is one tool fact read back off a lane's advertised capability
// surface — the `toolchains` entry preflight.py's measure_toolchains writes.
//
// Only the three fields a routing decision turns on are carried. This is a
// CONSUMER of the surface: internal/preflight keeps the host block opaque on
// purpose (four backends, one protocol, no engine-side idea of a host), so a
// package that needs a specific agreed key reads that key itself rather than
// pushing a typed host struct back into the protocol.
type Toolchain struct {
	Name string
	// UsableIn are the postures this tool can actually execute in.
	UsableIn []string
	// UnusableIn maps a posture to the lane's own words for why not. Those
	// words go into the narrative verbatim: "unusable" without the reason
	// sends the reader to a host they may not be able to log in to.
	UnusableIn map[string]string
}

// Lane is the candidate repair lane: the actor a repair would be dispatched
// to, and what its own capability surface says it can do.
//
// The zero value is "no lane", which routes to a human rather than being
// treated as an unconstrained one.
type Lane struct {
	ActorID  string
	ActorKey string
	// SurfaceAdvertised is whether this lane advertised a capability surface
	// at all. False is not "it can do everything" — it is "nothing here has
	// been shown", and it fails closed.
	SurfaceAdvertised bool
	// Posture is the sandbox/permission mode a repair dispatch would get
	// here (the lane's `default_sandbox_mode`).
	Posture string
	// Grants is what that posture grants, from the surface's
	// `dispatch_grants`.
	Grants     []string
	Toolchains []Toolchain
}

// Gate is the failing gate: what ran, against what, and what it needed.
type Gate struct {
	Suite   string
	Command []string
	// ExitCode is the whole of the finding. Zero routes nowhere.
	ExitCode  int
	CommitSHA string
	// VerdictRecordID is the derived suite-verdict record this routing is
	// computed from. It becomes the routing's subject and provenance.
	VerdictRecordID string
	// RequiresGrants are the dispatch grants the SUITE needs beyond running
	// its own binary — declared by the gate, because the gate is the only
	// party that knows a suite talks to a database. It is how #119 becomes a
	// routing decision instead of a wasted session: a lane whose posture
	// grants no network egress cannot verify a PostgreSQL-backed suite, no
	// matter how cleanly `go` runs there.
	RequiresGrants []string
}

// History is what this run has already spent, read out of its own ledger.
type History struct {
	// Attempts is the number of live repair routings already recorded.
	Attempts int
	// FirstRejectionAt anchors the window. Zero means this rejection is the
	// first one, and the window opens now.
	FirstRejectionAt time.Time
}

// Input is everything Decide is a function of. Nothing else is consulted:
// no clock beyond Now, no store, no network.
type Input struct {
	RunID     string
	NodeRunID string
	AttemptID string

	Gate    Gate
	Lane    Lane
	History History

	// ImplicatedPaths are the repo-relative paths this failure involves —
	// in practice the handover commit's measured changed paths, plus
	// anything the gate names explicitly. They are checked against
	// GuardedPathPrefixes.
	ImplicatedPaths []string

	RouterActorID  string
	RouterRevision string

	Now time.Time
}

// Routing is the decision. It is a value: composing it writes nothing and
// dispatches nothing.
type Routing struct {
	Destination Destination
	Reason      Reason
	// Narrative is one operator-facing sentence saying what happens next and
	// why, in the lane's own words where the lane supplied them.
	Narrative string

	// AttemptNumber is which repair round this would be, 1-based. Zero when
	// the destination is not a repair.
	AttemptNumber     int
	AttemptsUsed      int
	AttemptsRemaining int
	MaxAttempts       int
	Window            time.Duration
	// Deadline is when the window closes for this run.
	Deadline time.Time

	LaneActorID  string
	LaneActorKey string
	// GuardedPaths are the implicated paths that fell inside
	// GuardedPathPrefixes, in first-seen order. Non-empty only for
	// ReasonOutOfWorkflowScope.
	GuardedPaths []string
}

// Decide routes a gate outcome.
//
// The order of the checks is load-bearing and is pinned by
// TestOutOfScopeIsDecidedBeforeTheCeiling. Categorical refusals — this repair
// could never have been legal, this lane could never have verified it — come
// BEFORE the budget, because reporting "the retry budget ran out" for a
// repair that was never permissible tells an operator to spend more attempts
// on something no number of attempts fixes.
func Decide(in Input) Routing {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	first := in.History.FirstRejectionAt
	if first.IsZero() {
		first = now
	}

	base := Routing{
		AttemptsUsed:      in.History.Attempts,
		AttemptsRemaining: max(0, MaxAttempts-in.History.Attempts),
		MaxAttempts:       MaxAttempts,
		Window:            Window,
		Deadline:          first.Add(Window),
		LaneActorID:       in.Lane.ActorID,
		LaneActorKey:      in.Lane.ActorKey,
	}

	if in.Gate.ExitCode == 0 {
		base.Destination = DestinationNone
		base.Reason = ReasonGatePassed
		base.Narrative = fmt.Sprintf(
			"%q exited 0 against commit %s; a passing gate routes nowhere",
			in.Gate.Suite, short(in.Gate.CommitSHA))
		return base
	}

	// 1. The workflow-scope boundary. A repair attempt is a dispatch like
	//    any other, so a failure that involves CI configuration is one the
	//    loop must hand over rather than attempt.
	if guarded := Guarded(in.ImplicatedPaths); len(guarded) > 0 {
		base.Destination = DestinationHuman
		base.Reason = ReasonOutOfWorkflowScope
		base.GuardedPaths = guarded
		base.Narrative = fmt.Sprintf(
			"this failure involves %s, which a dispatch may not modify — GitHub Actions administration is "+
				"separately authorized work and is excluded from the credential a bridge dispatches under. "+
				"A repair sent here would be refused for touching it, so it goes to a person instead; no "+
				"repair attempt is spent",
			strings.Join(guarded, ", "))
		return base
	}

	// 2. The lane. Both refusals below are the recorded risk made mechanical:
	//    a lane that cannot verify its own repair must not be given one.
	if in.Lane.ActorID == "" {
		base.Destination = DestinationHuman
		base.Reason = ReasonNoLane
		base.Narrative = "no repair lane was resolved for this run, so there is nobody to route a repair to; " +
			"the gate failure goes to a person"
		return base
	}
	if !in.Lane.SurfaceAdvertised {
		base.Destination = DestinationHuman
		base.Reason = ReasonLaneUnknown
		base.Narrative = fmt.Sprintf(
			"%s advertises no capability surface, so nothing shows it can run the suite that failed. "+
				"An unknown lane is refused rather than assumed: the hazard — a repair that cannot verify "+
				"its own fix — has no downstream detection, so \"I cannot tell\" resolves the same way "+
				"\"unsafe\" does",
			laneName(in.Lane))
		return base
	}
	if !granted(in.Lane.Grants, grantWorkspaceWrite) {
		base.Destination = DestinationHuman
		base.Reason = ReasonLaneCannotWrite
		base.Narrative = fmt.Sprintf(
			"%s dispatches in posture %q, which does not grant %s: a session there cannot edit a file, so "+
				"it cannot repair anything. Routing a repair into it would spend a session to produce "+
				"nothing",
			laneName(in.Lane), in.Lane.Posture, grantWorkspaceWrite)
		return base
	}
	if why := cannotVerify(in.Lane, in.Gate); why != "" {
		base.Destination = DestinationHuman
		base.Reason = ReasonLaneCannotVerify
		base.Narrative = fmt.Sprintf(
			"%s cannot run the suite that failed, so a repair there could not be checked before it was "+
				"claimed: %s. A repair that cannot verify its own fix produces a second unverified claim, "+
				"which is the thing this gate exists to stop being enough",
			laneName(in.Lane), why)
		return base
	}

	// 3. The bound. Staleness first: a run outside the window is not a run
	//    with attempts left over, it is a run whose diagnosis expired.
	if now.After(base.Deadline) {
		base.Destination = DestinationHuman
		base.Reason = ReasonWindowExpired
		base.Narrative = fmt.Sprintf(
			"this run's gate first rejected at %s and the repair window is %s, which closed at %s. "+
				"%d of %d repair attempts are unspent and stay unspent: a repair aimed at a %s-old "+
				"diagnosis would be dispatched against a tree that has moved. It goes to a person",
			first.UTC().Format(time.RFC3339), Window, base.Deadline.UTC().Format(time.RFC3339),
			base.AttemptsRemaining, MaxAttempts, now.Sub(first).Round(time.Hour))
		return base
	}
	if in.History.Attempts >= MaxAttempts {
		base.Destination = DestinationHuman
		base.Reason = ReasonCeilingReached
		base.Narrative = fmt.Sprintf(
			"this run has spent %d of %d repair attempts and the gate still rejects. The bound is reached, "+
				"so the loop terminates here rather than buying a third session: a defect that survived "+
				"two repairs by a lane that can run the suite is one somebody has to look at",
			MaxAttempts, MaxAttempts)
		return base
	}

	base.Destination = DestinationRepair
	base.Reason = ReasonWithinBound
	base.AttemptNumber = in.History.Attempts + 1
	// On a repair, "remaining" counts what is left AFTER this round — the
	// number an operator reading "attempt 2 of 2, 0 remaining" needs. On
	// every other destination it stays the unspent budget, because nothing
	// was consumed: a window that expired with both attempts untouched must
	// not read as a budget that ran out.
	base.AttemptsRemaining = MaxAttempts - base.AttemptNumber
	base.Narrative = fmt.Sprintf(
		"%q exited %d against commit %s; this routes to repair attempt %d of %d on %s (window closes %s). "+
			"The attempt must see the gate's own output, not a summary of it, and its result is judged by "+
			"the same gate — a repair that makes things worse is caught here rather than in review",
		in.Gate.Suite, in.Gate.ExitCode, short(in.Gate.CommitSHA),
		base.AttemptNumber, MaxAttempts, laneName(in.Lane), base.Deadline.UTC().Format(time.RFC3339))
	return base
}

// Guarded reports which of paths fall inside GuardedPathPrefixes,
// de-duplicated, in first-seen order.
//
// Normalisation follows codex_bridge.scope_guard._normalize deliberately,
// including its one sharp edge: a leading "./" is stripped iteratively rather
// than with a character-class trim, which would eat the leading dot of
// ".github" itself and make the guard silently miss every path it exists for.
func Guarded(paths []string) []string {
	var hits []string
	seen := map[string]bool{}
	for _, entry := range paths {
		candidate := normalize(entry)
		if candidate == "" || seen[candidate] {
			continue
		}
		for _, prefix := range GuardedPathPrefixes {
			if candidate == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(candidate, prefix) {
				seen[candidate] = true
				hits = append(hits, candidate)
				break
			}
		}
	}
	return hits
}

func normalize(p string) string {
	out := strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	for strings.HasPrefix(out, "./") {
		out = out[2:]
	}
	return out
}

// cannotVerify returns the lane's own reason the failing suite could not be
// re-run there, or "" when nothing rules it out.
//
// Two honest limits, both stated here rather than left for a reader to
// discover:
//
//  1. The tool is identified from the gate's argv[0]. That establishes "this
//     lane can run the binary the suite is spelled with", which is weaker
//     than "this lane can run the suite" — a `go test` that needs a listener
//     is invisible here. Gate.RequiresGrants is how a gate closes that gap
//     for the cases it knows about, and the residue is one of the reasons
//     this loop is routed rather than unattended.
//  2. A tool the lane says nothing about is refused, not assumed. Same
//     fail-closed rule as an unadvertised surface.
func cannotVerify(lane Lane, gate Gate) string {
	for _, want := range gate.RequiresGrants {
		if !granted(lane.Grants, want) {
			return fmt.Sprintf(
				"the suite needs the %s grant and posture %q grants only [%s]",
				want, lane.Posture, strings.Join(lane.Grants, " "))
		}
	}

	tool := toolName(gate.Command)
	if tool == "" {
		return ""
	}
	for _, tc := range lane.Toolchains {
		if tc.Name != tool {
			continue
		}
		if contains(tc.UsableIn, lane.Posture) {
			return ""
		}
		if why := tc.UnusableIn[lane.Posture]; why != "" {
			return fmt.Sprintf("%s is unusable in posture %q there — %s", tool, lane.Posture, why)
		}
		return fmt.Sprintf(
			"%s is usable in [%s] there, and a repair dispatch would get posture %q",
			tool, strings.Join(tc.UsableIn, " "), lane.Posture)
	}
	return fmt.Sprintf(
		"%s is not among the toolchains that lane advertises, so nothing shows it can execute there",
		tool)
}

// toolName is the gate command's binary, basename-only so an absolute path
// matches the surface's bare tool name.
func toolName(command []string) string {
	if len(command) == 0 {
		return ""
	}
	return path.Base(strings.TrimSpace(command[0]))
}

func granted(grants []string, want string) bool { return contains(grants, want) }

func contains(haystack []string, needle string) bool {
	for _, entry := range haystack {
		if entry == needle {
			return true
		}
	}
	return false
}

func laneName(lane Lane) string {
	if lane.ActorKey != "" {
		return lane.ActorKey
	}
	return lane.ActorID
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// Record composes the derived decision record, or refuses.
//
// Nothing in it is anybody's opinion: the destination and the reason are
// computed by Decide from recorded facts, the bound is the package's own
// constants, and the narrative is assembled from those plus the lane's
// verbatim words. There is deliberately no field a caller can use to say "and
// I think we should try again anyway".
func (r Routing) Record(in Input) (ledger.Record, error) {
	if strings.TrimSpace(in.RunID) == "" {
		return ledger.Record{}, fmt.Errorf("repair: a routing must name the run whose gate it routed")
	}
	if strings.TrimSpace(in.RouterActorID) == "" {
		return ledger.Record{}, fmt.Errorf(
			"repair: a derived record needs an identified deterministic producer (PRD §10.4); " +
				"an anonymous router attests to nothing")
	}

	data := map[string]any{
		"question": fmt.Sprintf("the merge gate %q rejected commit %s — where does that go?",
			in.Gate.Suite, in.Gate.CommitSHA),
		"selected":  string(r.Destination),
		"options":   []string{string(DestinationRepair), string(DestinationHuman)},
		"reason":    string(r.Reason),
		"rationale": r.Narrative,
		"router":    RouterCollectionMethod,
		"bound": map[string]any{
			"max_attempts":   MaxAttempts,
			"window_seconds": int(Window / time.Second),
			"at_ceiling":     AtCeiling,
			"deadline":       r.Deadline.UTC().Format(time.RFC3339),
		},
		"attempt_number":     r.AttemptNumber,
		"attempts_used":      r.AttemptsUsed,
		"attempts_remaining": r.AttemptsRemaining,
		"suite":              in.Gate.Suite,
		"exit_code":          in.Gate.ExitCode,
		"commit_sha":         in.Gate.CommitSHA,
		// The two fields that keep a routing from being read as an
		// execution. #17 is what a swallowed "it was handled" costs.
		"dispatched": false,
		"dispatch_note": "this control plane decided and recorded the route; it did not dispatch it. " +
			"Unattended repair is deliberately not enabled — the write path through the bridges is " +
			"unproven (#18) and a lane's advertised surface cannot show that a database-backed suite " +
			"is runnable there (#119). Execute the routed dispatch deliberately",
		"repair_lane_actor_id": r.LaneActorID,
	}
	if r.LaneActorKey != "" {
		// Absent rather than empty, the discipline internal/handover states:
		// "" would read as "a key was recorded and it was blank".
		data["repair_lane_actor_key"] = r.LaneActorKey
	}
	if len(r.GuardedPaths) > 0 {
		guarded := append([]string(nil), r.GuardedPaths...)
		sort.Strings(guarded)
		data["guarded_paths"] = guarded
		data["guarded_path_prefixes"] = GuardedPathPrefixes
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return ledger.Record{}, fmt.Errorf("repair: encode routing payload: %w", err)
	}

	rec := ledger.Record{
		RecordType: ledger.RecordDecision,
		RunID:      in.RunID,
		NodeRunID:  ledger.NullableID(in.NodeRunID),
		AttemptID:  ledger.NullableID(in.AttemptID),
		Origin: ledger.Origin{
			Kind:          ledger.OriginValidator,
			ActorID:       in.RouterActorID,
			ActorRevision: in.RouterRevision,
		},
		Authority:      ledger.AuthorityDerived,
		SubjectRef:     ledger.NullableID(in.Gate.VerdictRecordID),
		ProvenanceRefs: []string{},
		Data:           payload,
	}
	if in.Gate.VerdictRecordID != "" {
		rec.ProvenanceRefs = []string{in.Gate.VerdictRecordID}
	}
	return rec, nil
}

// PriorAttempts counts the repair rounds a run has already been routed.
//
// It reads the payload Record writes — which is why it lives beside it — and
// selects on RouterCollectionMethod rather than on record type alone, so a
// human task's decision or a devague deviation is never counted as a repair
// round. Only LIVE records count: a superseded routing is a correction, not a
// spent attempt, and counting it would shrink the budget every time a mistake
// was fixed.
func PriorAttempts(records []ledger.Record) int {
	count := 0
	for _, rec := range ledger.Live(records) {
		if rec.RecordType != ledger.RecordDecision || rec.Authority != ledger.AuthorityDerived {
			continue
		}
		data, err := rec.DataMap()
		if err != nil {
			continue
		}
		if method, _ := data["router"].(string); method != RouterCollectionMethod {
			continue
		}
		if selected, _ := data["selected"].(string); selected == string(DestinationRepair) {
			count++
		}
	}
	return count
}

// FirstRejectionAt is when this run's gate first rejected, anchoring the
// window. Zero when it never has.
//
// It reads the routing records rather than the suite verdicts because the
// window bounds THIS loop: a verdict recorded by some other tool against the
// same run, before any routing existed, did not start a repair loop and must
// not shorten one.
func FirstRejectionAt(records []ledger.Record) time.Time {
	var first time.Time
	for _, rec := range ledger.Live(records) {
		if rec.RecordType != ledger.RecordDecision || rec.Authority != ledger.AuthorityDerived {
			continue
		}
		data, err := rec.DataMap()
		if err != nil {
			continue
		}
		if method, _ := data["router"].(string); method != RouterCollectionMethod {
			continue
		}
		// Positive selection: only a routing that actually went somewhere
		// opens the window. Anything else is not a rejection this loop is
		// running against.
		selected, _ := data["selected"].(string)
		if selected != string(DestinationRepair) && selected != string(DestinationHuman) {
			continue
		}
		if rec.CreatedAt.IsZero() {
			continue
		}
		if first.IsZero() || rec.CreatedAt.Before(first) {
			first = rec.CreatedAt
		}
	}
	return first
}
