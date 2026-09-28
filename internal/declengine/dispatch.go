package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/decl/kinds"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// DeclarationEngineActorID is the default producer identity of the derived
// decision record each firing writes. Like postgres.RemintSchedulerActorID it
// names the deciding component, not a process, and must be REGISTERED:
// ledger_records.origin_actor_id is a foreign key to actors(id). Register it
// the way engine_remint_scheduler was: deploy/prod/register-actor.sh --engine
// engine_declaration_engine (a deploy hand-turn once the engine is wired in).
const DeclarationEngineActorID = kinds.ControlPlaneEmitter

// DispatchRequest is one claimed firing's rendered action.
type DispatchRequest struct {
	Firing      postgres.DeclarationFiring
	Declaration decl.Declaration
	Action      decl.Action
	Marker      string
}

// DispatchResult is what a dispatch reports back to the firing loop. It
// carries no ledger records: an action's records are written by the worker
// that executed it, under the ordinary authority matrix, which caps an agent
// actor at proposed (PRD §10.4). The firing loop never writes them itself.
type DispatchResult struct {
	// Variables are returned synchronously and exposed to later
	// declarations in the lineage; an async action's come from its run.
	Variables map[string]any
	// ArtifactID is the provider id of what the action created, bound to
	// the firing's marker before any reaction can inherit the lineage.
	ArtifactID string
	// Async is true when the action is still executing.
	Async bool
	// Shadowed is true when this Dispatch never actually invoked an
	// action -- switch.go's ShadowGate sets it for the namespace's engine
	// switch (c80) being 'before' or 'shadow' -- so evaluate() (engine.go)
	// records the firing's terminal outcome as OutcomeShadow instead of
	// OutcomeFired (t13, spec c88). A Dispatcher that always really
	// dispatches (WorkerDispatcher, any test dispatchFunc) leaves this
	// false by construction.
	Shadowed bool
}

// Dispatcher hands a claimed firing's action to an executor. The firing
// loop calls it at most once per firing.
type Dispatcher interface {
	Dispatch(context.Context, DispatchRequest) (DispatchResult, error)
}

// WorkerDispatcher queues a private, single-action execution envelope. The
// existing worker owns dispatch, leases, retries, actor callbacks, runner
// manifests and human decisions. This adapter never invokes an actor itself.
// One firing is one run ID; ancestry and version selection belong to the TCA
// engine, not to this envelope. Call Reconcile after worker completion to bind
// any produced artifact before accepting its reaction event.
type WorkerDispatcher struct {
	Store *postgres.Store
	// ProducerActorID is the registered identity the firing's derived
	// decision record is written under. Empty selects DeclarationEngineActorID.
	ProducerActorID string
}

// Dispatch creates the firing's run (idempotently: the run id is the
// firing id) and appends the firing's ledger record naming the declaration,
// its version and the three component digests.
func (w WorkerDispatcher) Dispatch(ctx context.Context, r DispatchRequest) (DispatchResult, error) {
	if w.Store == nil {
		return DispatchResult{}, errors.New("declengine: worker dispatcher needs a store")
	}
	producer := w.ProducerActorID
	if producer == "" {
		producer = DeclarationEngineActorID
	}
	// Both refusals run before the run exists, so a refused firing never
	// leaves an action queued for the worker to execute anyway.
	if err := w.requireStamping(ctx, r); err != nil {
		return DispatchResult{}, err
	}
	if err := w.requireRegisteredProducer(ctx, producer); err != nil {
		return DispatchResult{}, err
	}
	cw, input, err := workerEnvelope(r)
	if err != nil {
		return DispatchResult{}, err
	}
	es, err := postgres.NewEngineStore(w.Store, r.Firing.NamespaceID)
	if err != nil {
		return DispatchResult{}, err
	}
	// This engine creates the declaration action's one-node envelope by
	// explicit dispatch. It does not route graph workflow triggers, so the
	// graph NewRunGate must not prevent declaration actions in `after`.
	eng, err := engine.New(es)
	if err != nil {
		return DispatchResult{}, err
	}
	var exists bool
	if err := w.Store.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE namespace_id=$1 AND id=$2)`, r.Firing.NamespaceID, r.Firing.ID).Scan(&exists); err != nil {
		return DispatchResult{}, err
	}
	if !exists {
		_, err = eng.CreateRun(ctx, cw, input, func(run *engine.Run) { run.ID = r.Firing.ID; run.TriggerEventID = r.Firing.EventID })
		if err != nil {
			return DispatchResult{}, err
		}
	}
	ls, err := postgres.NewLedgerStore(w.Store, r.Firing.NamespaceID)
	if err != nil {
		return DispatchResult{}, err
	}
	records, err := ls.RunRecords(ctx, r.Firing.ID)
	if err != nil {
		return DispatchResult{}, err
	}
	recordID := ledger.IDPrefix + "decl_" + r.Firing.ID
	for _, rec := range records {
		if rec.ID == recordID {
			return DispatchResult{Async: true}, nil
		}
	}
	// The engine decided to fire from the pinned version; that decision is a
	// deterministic producer's derived record, never an agent's claim.
	data, err := json.Marshal(map[string]any{
		"kind": "declaration_firing", "firing_id": r.Firing.ID, "event_id": r.Firing.EventID,
		"declaration_id": r.Firing.DeclarationID, "declaration_version": r.Firing.DeclarationVersion,
		"trigger_digest": r.Firing.TriggerDigest, "condition_digest": r.Firing.ConditionDigest, "action_digest": r.Firing.ActionDigest,
	})
	if err != nil {
		return DispatchResult{}, err
	}
	l, err := postgres.NewLedger(w.Store, r.Firing.NamespaceID)
	if err != nil {
		return DispatchResult{}, err
	}
	_, err = l.Append(ctx, ledger.Record{ID: recordID, RunID: r.Firing.ID, RecordType: ledger.RecordDecision, Origin: ledger.Origin{Kind: ledger.OriginEngine, ActorID: producer}, Authority: ledger.AuthorityDerived, Data: data})
	return DispatchResult{Async: true}, err
}

// ErrProducerNotRegistered is the loud failure of a firing whose producer
// identity (DeclarationEngineActorID unless overridden) is missing from the
// actors table (task t38). Without the check the run would be created, the
// derived decision record's ledger append would then fail on
// ledger_records.origin_actor_id's foreign key, and the worker would still
// execute an action whose firing the engine recorded as failed.
var ErrProducerNotRegistered = errors.New("declengine: producer identity is not registered")

func (w WorkerDispatcher) requireRegisteredProducer(ctx context.Context, producer string) error {
	var registered bool
	if err := w.Store.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM actors WHERE id=$1)`, producer).Scan(&registered); err != nil {
		return err
	}
	if !registered {
		return fmt.Errorf("%w: %q has no actors row (ledger_records.origin_actor_id references actors(id)); register it with deploy/prod/register-actor.sh --engine %s",
			ErrProducerNotRegistered, producer, producer)
	}
	return nil
}

// StampingCapabilityMarker is the marker scheme a bridge advertises it
// stamps: {"stamping": {"marker": "cn1", ...}} on /v1/capabilities, recorded
// on its actors row's capabilities (or its preflight block's).
const StampingCapabilityMarker = "cn1"

// StampingRefusal is task t27's engine-side remainder, wired by t38: a
// dispatch whose action creates an artifact and carries a minted marker, to
// an actor whose current registration does not advertise stamping, is
// refused before anything is queued -- the bridge would create the artifact
// without the marker, and every reaction to it would then start a fresh
// lineage instead of continuing this one. evaluate() records it as
// OutcomeStampingRefused with this error's text, naming actor and revision.
type StampingRefusal struct {
	ActionKind, ActorKey string
	// Revision is the actor's newest registered revision; zero with
	// Registered false when no row exists for the key at all.
	Revision   int
	Registered bool
}

func (r *StampingRefusal) Error() string {
	if !r.Registered {
		return fmt.Sprintf("declengine: stamping refused: action %s creates an artifact and carries a marker, but actor %q has no registration to advertise the %s stamping capability",
			r.ActionKind, r.ActorKey, StampingCapabilityMarker)
	}
	return fmt.Sprintf("declengine: stamping refused: action %s creates an artifact and carries a marker, but actor %q revision %d does not advertise {\"stamping\": {\"marker\": %q}}",
		r.ActionKind, r.ActorKey, r.Revision, StampingCapabilityMarker)
}

// requireStamping applies the refusal. Out of scope by construction: a
// firing with no marker; an action whose every produced artifact is 'none';
// an action with no `uses` (human.ask -- the approval is served by the
// control plane's own human-task surface, not a bridge); and a `runner://`
// target (code.run -- a runner returns its code.result in the operation
// result, it creates nothing external to stamp).
//
// Those last two are skipped because no bridge is involved, NOT because
// their lineage may stop (task t38c, review findings C1/C2, deviation d3):
// the control plane is the stamper for both. Once the firing's run
// completes, the Driver's reaction pass (reactions.go, EmitActionReactions)
// binds the produced artifact -- the decided human task, or the runner
// operation -- to the firing's minted marker and emits the human.decision /
// code.result reaction carrying that marker, so the reaction verifies and
// continues this firing's lineage exactly as a bridge-stamped one does.
func (w WorkerDispatcher) requireStamping(ctx context.Context, r DispatchRequest) error {
	if r.Marker == "" {
		return nil
	}
	kind, ok := kinds.Action(r.Action.Kind)
	if !ok || !createsArtifact(kind) {
		return nil
	}
	var with struct {
		Uses string `json:"uses"`
	}
	if len(r.Action.With) > 0 {
		if err := json.Unmarshal(r.Action.With, &with); err != nil {
			return err
		}
	}
	if with.Uses == "" || strings.HasPrefix(with.Uses, "runner://") {
		return nil
	}
	key := actors.ActorKeyOf(with.Uses)
	refusal := &StampingRefusal{ActionKind: r.Action.Kind, ActorKey: key}
	var caps []byte
	err := w.Store.Pool().QueryRow(ctx, `SELECT revision,capabilities FROM actors WHERE namespace_id=$1 AND actor_key=$2 ORDER BY revision DESC LIMIT 1`,
		r.Firing.NamespaceID, key).Scan(&refusal.Revision, &caps)
	if errors.Is(err, pgx.ErrNoRows) {
		return refusal
	}
	if err != nil {
		return err
	}
	refusal.Registered = true
	if advertisesStamping(caps) {
		return nil
	}
	return refusal
}

func createsArtifact(k kinds.Kind) bool {
	for _, a := range k.Produces {
		if a != kinds.ArtifactNone {
			return true
		}
	}
	return false
}

// advertisesStamping reads the capability where a bridge's advertisement
// lands: top-level capabilities.stamping, or the same block nested under
// capabilities.preflight.
func advertisesStamping(raw []byte) bool {
	var caps struct {
		Stamping  *struct{ Marker string } `json:"stamping"`
		Preflight struct {
			Stamping *struct{ Marker string } `json:"stamping"`
		} `json:"preflight"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &caps) != nil {
		return false
	}
	for _, s := range []*struct{ Marker string }{caps.Stamping, caps.Preflight.Stamping} {
		if s != nil && s.Marker == StampingCapabilityMarker {
			return true
		}
	}
	return false
}

// MarkerInputKey is the action-input key a firing's minted cn1 marker rides
// under to the actor. It is the key every bridge's byte-identical
// adapters/*/src/*/stamping.py reads (`read_marker`: `raw_input.get("marker")`,
// documented there as `input.marker`), and the key the jira, github, notify
// and human-inbox servers read directly -- so the bridges are the fixed side
// and this constant follows them. It was `origin_marker` until task t29, which
// no bridge read: every marked dispatch reached its bridge unstamped.
// tests/conformance/declactions/declactions_test.go pins both halves.
const MarkerInputKey = "marker"

// workerEnvelope translates only the action into the existing worker contract.
// Action.with contains uses/input/operation (or approver_ref for human.ask).
// The supplied input is already template-rendered by the declaration engine.
func workerEnvelope(r DispatchRequest) (*compiler.CompiledWorkflow, json.RawMessage, error) {
	var with struct {
		Uses        string          `json:"uses"`
		Input       map[string]any  `json:"input"`
		Operation   json.RawMessage `json:"operation"`
		ApproverRef string          `json:"approver_ref"`
		Timeout     string          `json:"timeout"`
		Outcomes    []string        `json:"outcomes"`
		GraphConfig struct {
			Contract struct {
				Outcomes map[string]json.RawMessage `json:"outcomes"`
			} `json:"contract"`
		} `json:"graph_config"`
	}
	if err := json.Unmarshal(r.Action.With, &with); err != nil {
		return nil, nil, err
	}
	if with.Input == nil {
		with.Input = map[string]any{}
	}
	// The engine owns this key: an author-supplied value is dropped so a
	// declaration cannot hand a bridge a marker the engine never minted, and
	// an unmarked firing sends none at all (a bridge refuses a present but
	// malformed marker, and "" is malformed).
	delete(with.Input, MarkerInputKey)
	if r.Marker != "" {
		with.Input[MarkerInputKey] = r.Marker
	}
	input, err := json.Marshal(with.Input)
	if err != nil {
		return nil, nil, err
	}
	nodeKind := "action.http"
	switch r.Action.Kind {
	case "agent.work":
		nodeKind = "agent"
	case "code.run":
		nodeKind = "code"
	case "human.ask":
		nodeKind = "approval"
	case "discord.post", "github.comment", "github.review_reply", "jira.comment", "jira.transition", "jira.create":
	default:
		return nil, nil, fmt.Errorf("unsupported action %q", r.Action.Kind)
	}
	if nodeKind != "approval" && with.Uses == "" {
		return nil, nil, errors.New("action.with.uses must name a registered actor or runner")
	}
	outcome := "completed"
	if nodeKind == "code" {
		outcome = "passed"
	}
	schema := map[string]any{"schema": map[string]any{"type": "object"}}
	outcomes := map[string]any{outcome: schema}
	// The notify bridge's domain vocabulary is fixed. Supply it at the
	// engine seam so an omitted author contract cannot reject a delivered post.
	if r.Action.Kind == "discord.post" {
		if with.GraphConfig.Contract.Outcomes == nil {
			with.GraphConfig.Contract.Outcomes = make(map[string]json.RawMessage)
		}
		for _, name := range []string{"sent", "delivery_failed"} {
			if _, ok := with.GraphConfig.Contract.Outcomes[name]; !ok {
				with.GraphConfig.Contract.Outcomes[name] = json.RawMessage(`{"schema":{"type":"object","required":["delivered","status_code"]}}`)
			}
		}
	}
	// Task t38e: an agent's domain outcome is carried through, not collapsed
	// to `completed`. The node offers the outcomes the declaration's migrated
	// contract declares (with.graph_config.contract.outcomes: the source
	// graph node's own names and output schemas), so an agent reporting
	// `packaged` completes the run with outcome `packaged` and the
	// agent.result reaction (reactions.go) carries it. What the agent reports
	// is still its claim: the attempt's ledger records stay capped at
	// proposed by the worker's authority matrix, and an outcome the contract
	// does not declare is the engine's technical contract_rejected, never a
	// domain answer. With no declared contract the node keeps `completed`.
	primary, extraAgent, err := agentOutcomes(nodeKind, with.GraphConfig.Contract.Outcomes, outcomes)
	if err != nil {
		return nil, nil, err
	}
	if primary != "" {
		outcome = primary
	}
	if nodeKind == "agent" {
		// This is part of every agent contract, including declarations with no
		// authored outcomes. Keep the prompt at the engine seam so every
		// backend receives the same choice through input.instruction.
		outcomes["blocked"] = map[string]any{"schema": map[string]any{
			"type": "object", "required": []string{"reason"},
			"properties": map[string]any{"reason": map[string]any{"type": "string", "minLength": 1}},
		}}
		extraAgent = append(extraAgent, "blocked")
		instruction, _ := with.Input["instruction"].(string)
		with.Input["instruction"] = instruction + "\n\nIf you cannot complete this task for a reason outside your control (missing credentials, missing access, or contradictory instructions), answer with outcome blocked. In that case, override any earlier final-answer format instruction: your final answer must be exactly a JSON object {\"outcome\":\"blocked\",\"output\":{\"reason\":\"...\"}}. Report exactly what blocked you in output.reason; include your summary and evidence as additional output fields when useful.\n"
		input, err = json.Marshal(with.Input)
		if err != nil {
			return nil, nil, err
		}
	}
	// Task t38c: a nonzero exit is a code step's DOMAIN answer (`failed`,
	// ConventionalCodeOutcomes' failure port), not a technical failure: the
	// run completes and the code.result reaction carries it. Transport,
	// timeout and runner trouble still fail the run (action.* results).
	extra := extraAgent
	if nodeKind == "code" {
		outcomes["failed"] = schema
		extra = append(extra, "failed")
	}
	node := map[string]any{"kind": nodeKind, "ownerRef": "team/declarations", "input": map[string]any{"from": "/run/input"}, "contract": map[string]any{"outcomes": outcomes}}
	if nodeKind == "approval" {
		if with.ApproverRef == "" {
			return nil, nil, errors.New("human.ask requires approver_ref")
		}
		node["approverRef"] = with.ApproverRef
		delete(outcomes, outcome)
		choices := with.Outcomes
		if len(choices) == 0 {
			choices = []string{"approved", "rejected"}
		}
		outcome = choices[0]
		outcomes[outcome] = schema
		for _, choice := range choices[1:] {
			outcomes[choice] = schema
			extra = append(extra, choice)
		}
		// `expired` is implied for every approval node by the compiler;
		// an edge for it lets an expired task complete the run, so the
		// human.decision reaction reports it (t38c) instead of the run
		// failing on "no edge matched".
		extra = append(extra, "expired")
	} else {
		node["uses"] = with.Uses
	}
	if nodeKind == "code" {
		node["operation"] = with.Operation
		node["ledger"] = map[string]any{"observe": []string{"evidence"}}
	} else {
		node["ledger"] = map[string]any{"propose": []string{"claim", "result", "question", "task", "assumption", "success_signal"}}
	}
	if with.Timeout != "" {
		node["policy"] = map[string]any{"timeout": with.Timeout}
	}
	edges := []any{map[string]any{"from": "action." + outcome, "to": "finish"}}
	for _, o := range extra {
		edges = append(edges, map[string]any{"from": "action." + o, "to": "finish"})
	}
	// An approval node has no attempt and so no output; binding the run's
	// output to it failed every decided human.ask run (found by t38c). Its
	// decision reaches the lineage through the human.decision reaction.
	output := "/nodes/action/output"
	if nodeKind == "approval" {
		output = "/run/input"
	}
	spec := map[string]any{
		"entry": "action", "contract": map[string]any{"input": schema, "output": schema},
		"nodes": map[string]any{"action": node, "finish": map[string]any{"kind": "end", "ownerRef": "team/declarations", "output": map[string]any{"from": output}}}, "edges": edges}
	if nodeKind == "approval" {
		limit, err := approvalRunLimit(r.Declaration.LandingNode)
		if err != nil {
			return nil, nil, err
		}
		spec["limits"] = map[string]any{"maxDuration": limit}
	}
	document := map[string]any{"apiVersion": "nodes.culture.dev/v1alpha1", "kind": "Workflow", "metadata": map[string]any{"name": "decl-" + strings.ToLower(r.Firing.DeclarationID), "version": "1.0.0", "ownerRef": "team/declarations"}, "spec": spec}
	source, err := json.Marshal(document)
	if err != nil {
		return nil, nil, err
	}
	cw, diagnostics, err := compiler.Compile(source, compiler.FormatJSON)
	if err != nil {
		return nil, nil, err
	}
	if cw == nil {
		return nil, nil, fmt.Errorf("declaration worker envelope: %v", diagnostics)
	}
	return cw, input, nil
}

// approvalRunLimit is a human.ask run's wall-clock bound. A person answers
// on their own schedule, so the compiler's one-hour DefaultMaxDuration would
// fail every decision made later than that (run.bounded max_duration), and
// the decision would never reach a declaration as human.decision. Production
// lost four decisions this way on 2026-09-28 (#328). The run must outlive
// the landing node it opened -- the node's own deadline (node.expired) is
// what bounds the wait -- so the bound is that deadline plus an hour, and a
// year for a node declared "none".
func approvalRunLimit(landing decl.Node) (string, error) {
	if landing.Deadline == "" {
		landing.Deadline = "none"
	}
	d, err := landingDeadline(landing)
	if err != nil {
		return "", err
	}
	if d == 0 {
		return (365 * 24 * time.Hour).String(), nil
	}
	return (d + time.Hour).String(), nil
}

// agentOutcomeName is the shape of a domain outcome an agent node may
// declare: lower snake case, as every migrated contract names them.
var agentOutcomeName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// agentOutcomes replaces an agent node's default `completed` with the
// declared contract's outcomes, in sorted order: primary takes the default
// outcome's edge to finish, extra each get their own. Every other node kind,
// and an agent node with no declared contract, keeps its primary default;
// workerEnvelope then adds the conventional blocked outcome to every agent.
func agentOutcomes(nodeKind string, declared map[string]json.RawMessage, outcomes map[string]any) (primary string, extra []string, err error) {
	// A bridge action (action.http) reports a domain outcome too -- the jira
	// bridge answers create_issue with issue_created (#328 t32, found live).
	if (nodeKind != "agent" && nodeKind != "action.http") || len(declared) == 0 {
		return "", []string{}, nil
	}
	if nodeKind == "agent" {
		if _, redefined := declared["blocked"]; redefined {
			return "", nil, fmt.Errorf("graph_config.contract.outcomes: blocked is a reserved conventional outcome")
		}
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	delete(outcomes, "completed")
	for _, name := range names {
		if !agentOutcomeName.MatchString(name) {
			return "", nil, fmt.Errorf("graph_config.contract.outcomes: %q is not an outcome name", name)
		}
		var spec map[string]any
		if err := json.Unmarshal(declared[name], &spec); err != nil || spec == nil {
			return "", nil, fmt.Errorf("graph_config.contract.outcomes.%s must be an object with a schema", name)
		}
		if _, ok := spec["schema"]; !ok {
			spec["schema"] = map[string]any{"type": "object"}
		}
		outcomes[name] = spec
	}
	return names[0], names[1:], nil
}

// Reconcile binds artifacts reported by the existing worker's terminal run.
// The receiver must reconcile before replaying a reaction that arrived before
// completion. An incomplete run never supplies artifact identity or variables.
func (e *Engine) Reconcile(ctx context.Context, db *postgres.Store, namespaceID, firingID string) error {
	return reconcileRun(ctx, db, e.markers, namespaceID, firingID)
}

// PrepareOrigin collects a completed worker result before marker verification.
// Unknown/copied markers still go through the normal rejection path.
func (w WorkerDispatcher) PrepareOrigin(ctx context.Context, origin OriginEvent, markers *MarkerService) error {
	p, ok := parseMarker(origin.Marker)
	if !ok {
		return nil
	}
	r, err := markers.store.LookupMarker(ctx, origin.NamespaceID, p.firingID, p.kind, p.nonce)
	if err != nil {
		return err
	}
	if r.NamespaceID != origin.NamespaceID || r.FiringID != p.firingID || r.MAC != p.mac || r.ArtifactID != "" {
		return nil
	}
	return reconcileRun(ctx, w.Store, markers, origin.NamespaceID, p.firingID)
}

func reconcileRun(ctx context.Context, db *postgres.Store, markersService *MarkerService, namespaceID, firingID string) error {
	var output []byte
	var state string
	if err := db.Pool().QueryRow(ctx, `SELECT status,output FROM runs WHERE namespace_id=$1 AND id=$2`, namespaceID, firingID).Scan(&state, &output); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	if state != "completed" {
		return nil
	}
	var result struct {
		ArtifactID string `json:"artifact_id"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return err
	}
	if result.ArtifactID == "" {
		return nil
	}
	// A marker the control plane stamps itself (task t38e: agent.work's
	// agent_work marker, minted and bound to the run id by the reaction
	// pass) never takes a bridge-reported artifact id: the run output's
	// artifact_id is what the bridge created (a PR), not the run.
	rows, err := db.Pool().Query(ctx, `SELECT artifact_kind,nonce,mac FROM declaration_minted_markers WHERE namespace_id=$1 AND firing_id=$2 AND artifact_kind<>$3`,
		namespaceID, firingID, string(kinds.ArtifactAgentWork))
	if err != nil {
		return err
	}
	var markers []string
	for rows.Next() {
		var kind, nonce, mac string
		if err := rows.Scan(&kind, &nonce, &mac); err != nil {
			rows.Close()
			return err
		}
		markers = append(markers, "cn1:"+firingID+":"+kind+":"+nonce+":"+mac)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, marker := range markers {
		if err := markersService.RecordArtifact(ctx, namespaceID, marker, result.ArtifactID); err != nil {
			return err
		}
	}
	return nil
}
