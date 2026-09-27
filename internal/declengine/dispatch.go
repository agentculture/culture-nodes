package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/decl"
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
const DeclarationEngineActorID = "engine_declaration_engine"

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
	cw, input, err := workerEnvelope(r)
	if err != nil {
		return DispatchResult{}, err
	}
	es, err := postgres.NewEngineStore(w.Store, r.Firing.NamespaceID)
	if err != nil {
		return DispatchResult{}, err
	}
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
	}
	if err := json.Unmarshal(r.Action.With, &with); err != nil {
		return nil, nil, err
	}
	if with.Input == nil {
		with.Input = map[string]any{}
	}
	with.Input["origin_marker"] = r.Marker
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
	node := map[string]any{"kind": nodeKind, "ownerRef": "team/declarations", "input": map[string]any{"from": "/run/input"}, "contract": map[string]any{"outcomes": outcomes}}
	if nodeKind == "approval" {
		if with.ApproverRef == "" {
			return nil, nil, errors.New("human.ask requires approver_ref")
		}
		node["approverRef"] = with.ApproverRef
		outcomes["rejected"] = schema
		delete(outcomes, outcome)
		outcome = "approved"
		outcomes[outcome] = schema
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
	if nodeKind == "approval" {
		edges = append(edges, map[string]any{"from": "action.rejected", "to": "finish"})
	}
	document := map[string]any{"apiVersion": "nodes.culture.dev/v1alpha1", "kind": "Workflow", "metadata": map[string]any{"name": "decl-" + strings.ToLower(r.Firing.DeclarationID), "version": "1.0.0", "ownerRef": "team/declarations"}, "spec": map[string]any{
		"entry": "action", "contract": map[string]any{"input": schema, "output": schema},
		"nodes": map[string]any{"action": node, "finish": map[string]any{"kind": "end", "ownerRef": "team/declarations", "output": map[string]any{"from": "/nodes/action/output"}}}, "edges": edges}}
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
	rows, err := db.Pool().Query(ctx, `SELECT artifact_kind,nonce,mac FROM declaration_minted_markers WHERE namespace_id=$1 AND firing_id=$2`, namespaceID, firingID)
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
