// Control-plane stamping for human.ask and code.run (task t38c, #328;
// cortex review findings C1/C2; deviation d3).
//
// A bridge-backed action continues its lineage because the bridge stamps
// what it creates with the firing's minted marker, the worker's completed
// run reports the artifact id, reconcileRun (dispatch.go) binds that id to
// the marker, and the reaction event the outside world later delivers
// carries marker + artifact id, which MarkerService.Resolve verifies.
//
// Two action kinds never reach a bridge. human.ask is served by the control
// plane's own human-task surface; code.run by a runner:// service that
// returns its result in the operation status. requireStamping skips both on
// purpose -- there is no bridge to refuse -- which, before this file, meant
// no one stamped them: nothing emitted a human.decision event at all (a
// declaration triggered on it never fired), there was no code.result
// trigger kind, and any chain through a human decision or a code step
// started a fresh lineage, breaking every MUST link across it.
//
// The control plane is therefore the stamper for these two kinds. The
// scheduler Driver's reaction pass (EmitActionReactions, run in 'shadow' and
// 'after' under the namespace's shared switch lock, after EmitActionResults)
// finds each completed declaration run whose firing's minted marker is for a
// human.decision or code.result artifact and has no reaction yet, and, under
// a per-run advisory lock:
//
//  1. resolves the produced artifact: for human.ask the resolved human task
//     (human_tasks.id of the run's "action" approval node run); for code.run
//     the runner operation id (runner_invocations.operation_id of the run's
//     "action" code node run, newest attempt), falling back to the run id
//     when the run records none. A marker already bound (reconcileRun saw an
//     artifact_id in the run output) keeps its binding;
//  2. binds that id to the marker through MarkerService.RecordArtifact --
//     the same path reconcileRun uses for bridge runs;
//  3. inserts the reaction signal event: name = the artifact kind
//     (human.decision or code.result), run_id = the firing, and a payload in
//     the t38 delivered-event convention (EventFromSignal): `node` = the
//     firing's landing node, `origin` = {marker, artifact_kind,
//     artifact_id}, `outcome` = the action node run's domain outcome
//     (approved / rejected / expired for a decision; passed / failed for a
//     code step), plus the decision response or the code result;
//
// and, after the commit, offers the event to Handle, where the marker
// verifies to this firing and the reaction continues its lineage.
//
// Task t38e (#328, owner decision d7) adds the third: agent.result, the
// reaction to a completed agent.work run. An agent.work firing's dispatch
// marker stays minted for Produces[0] (github.pr) -- that is the one its
// bridge stamps into the pull request, and reconcileRun binds the PR to it.
// The run itself is the second product, agent_work, and nothing external
// ever reports it, so the control plane stamps it the way it stamps a
// decision or a code result, with its own marker:
//
//  1. the candidate is any completed run of a marked firing (a Produces[0]
//     marker exists) whose pinned version's action kind is agent.work;
//  2. under the run's advisory lock, the firing's agent_work marker is
//     found or, the first time, minted by the engine's MarkerService (the
//     same key, the same table) -- it is never handed to any actor;
//  3. the run id is bound to it through RecordArtifact, and reconcileRun
//     never binds a bridge-reported artifact id to an agent_work marker;
//  4. the event is agent.result with origin {marker, artifact_kind:
//     agent.work, artifact_id: run id}, `outcome` = the domain outcome the
//     agent reported on its action node run (not collapsed to completed:
//     workerEnvelope declares the declaration's contract outcomes),
//     `result` = the run output, and outcome_authority "proposed".
//
// So the reaction's marker check is MarkerService.verify, unchanged in
// every step: the MAC under the engine's key, the minted row for exactly
// (firing, agent.work, nonce), and the bound artifact equal to the run id.
// verify only gained a rejection (marker.go controlPlaneReactionMarkers):
// an agent.result must carry an agent_work marker -- the PR's github.pr
// marker is public, stamped into the PR body, and must not be enough to
// claim an agent outcome -- and likewise human.decision and code.result
// must carry their own kinds.
//
// A failed agent.work run emits only its action.* result (EmitActionResults
// scans status 'failed'; this pass scans 'completed').
//
// Idempotent: the NOT EXISTS guard plus the advisory lock mean two passes
// (or two schedulers racing) emit one reaction per run. In 'shadow' the
// reaction is still emitted and evaluated -- ShadowGate records would-fire
// firings and dispatches nothing -- and in 'before' the Driver never runs the
// pass, so a run's reaction waits for the next shadow/after tick.
package declengine

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/agentculture/culture-nodes/internal/decl/kinds"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The two reaction triggers the control plane emits. Each is named after
// the artifact kind it reports (internal/decl/kinds).
const (
	ReactionHumanDecision = string(kinds.ArtifactHumanDecision)
	ReactionCodeResult    = string(kinds.ArtifactCodeResult)
	// ReactionAgentResult (task t38e) is the one reaction NOT named after
	// its artifact: the artifact is the agent_work run (kinds.ArtifactAgentWork).
	ReactionAgentResult = "agent.result"
)

// ReactionCandidate is one completed declaration run owed a reaction.
// Reaction is the event name; ArtifactKind the marker's kind. For an
// agent.result candidate Nonce and MAC are empty until EmitReaction finds
// or mints the firing's agent_work marker.
type ReactionCandidate struct {
	FiringID, Reaction, ArtifactKind, Nonce, MAC, NodeName, Subject string
}

// ReactionStamp is what the engine lends EmitReaction: the marker service's
// bind and mint, so the backend never holds the marker key.
type ReactionStamp struct {
	// Bind binds artifactID to marker (MarkerService.RecordArtifact).
	Bind func(ctx context.Context, marker, artifactID string) error
	// Mint mints a marker for the firing and artifact kind (MarkerService.Mint).
	Mint func(ctx context.Context, firingID, artifactKind string) (string, error)
}

func (c ReactionCandidate) marker() string {
	return "cn1:" + c.FiringID + ":" + c.ArtifactKind + ":" + c.Nonce + ":" + c.MAC
}

// ReactionEvent is one reaction EmitActionReactions emitted.
type ReactionEvent struct {
	NamespaceID, FiringID, EventID, Trigger, Outcome, ArtifactID string
}

// ReactionBackend is the optional Backend capability behind the pass.
type ReactionBackend interface {
	// ReactionCandidates lists completed runs owed a reaction, at most limit.
	ReactionCandidates(ctx context.Context, namespaceID string, limit int) ([]ReactionCandidate, error)
	// EmitReaction binds the candidate's artifact (through stamp) and
	// appends its reaction event, once per run. emitted is false when the
	// reaction already exists or the run's artifact cannot be resolved (yet).
	EmitReaction(ctx context.Context, namespaceID string, c ReactionCandidate, stamp ReactionStamp) (ev postgres.SignalEvent, outcome, artifactID string, emitted bool, err error)
}

// EmitActionReactions is the control plane stamping pass for human.ask,
// code.run and agent.work; see this file's comment.
func (e *Engine) EmitActionReactions(ctx context.Context, namespaceID string, limit int) ([]ReactionEvent, error) {
	rb, ok := e.backend.(ReactionBackend)
	if !ok {
		return nil, nil
	}
	due, err := rb.ReactionCandidates(ctx, namespaceID, limit)
	if err != nil {
		return nil, err
	}
	var out []ReactionEvent
	var failures []error
	stamp := ReactionStamp{
		Bind: func(ctx context.Context, marker, artifactID string) error {
			return e.markers.RecordArtifact(ctx, namespaceID, marker, artifactID)
		},
		Mint: func(ctx context.Context, firingID, artifactKind string) (string, error) {
			return e.markers.Mint(ctx, namespaceID, firingID, artifactKind)
		},
	}
	for _, c := range due {
		ev, outcome, artifactID, emitted, err := rb.EmitReaction(ctx, namespaceID, c, stamp)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !emitted {
			continue
		}
		out = append(out, ReactionEvent{NamespaceID: namespaceID, FiringID: c.FiringID, EventID: ev.ID, Trigger: ev.Name, Outcome: outcome, ArtifactID: artifactID})
		event := EventFromSignal(ev)
		// The reaction belongs to the subject its action was taken for, so
		// the per-subject concurrency cap keeps counting the same chain.
		event.Subject = c.Subject
		if err := e.Handle(ctx, event); err != nil {
			failures = append(failures, err)
		}
	}
	return out, errors.Join(failures...)
}

// reactionCandidatesSQL finds completed declaration runs whose landing node
// exists (Finish ran, so Handle has a node to derive) and that have no
// reaction yet: firings that minted a human.decision or code.result marker
// ($3, $4), and (task t38e) marked agent.work firings -- the pinned version's
// action kind is agent.work ($5) and the firing minted its dispatch marker
// for Produces[0] ($6) -- owed agent.result ($7) for their agent_work run
// ($8). An agent.result candidate carries no marker yet: EmitReaction finds
// or mints it under the run's lock.
const reactionCandidatesSQL = `SELECT * FROM (
	SELECT r.id,m.artifact_kind AS reaction,m.artifact_kind,m.nonce,m.mac,dn.node_name,COALESCE(f.subject,'') FROM runs r
	JOIN declaration_firings f ON f.namespace_id=r.namespace_id AND f.id=r.id
	JOIN declaration_minted_markers m ON m.namespace_id=r.namespace_id AND m.firing_id=r.id
	JOIN declaration_nodes dn ON dn.namespace_id=r.namespace_id AND dn.opening_firing_id=r.id
	WHERE r.namespace_id=$1 AND r.status='completed' AND m.artifact_kind IN ($3,$4)
	AND NOT EXISTS (SELECT 1 FROM signal_events se WHERE se.namespace_id=r.namespace_id AND se.run_id=r.id AND se.name=m.artifact_kind)
	UNION ALL
	SELECT r.id,$7,$8,'','',dn.node_name,COALESCE(f.subject,'') FROM runs r
	JOIN declaration_firings f ON f.namespace_id=r.namespace_id AND f.id=r.id
	JOIN declaration_versions v ON v.namespace_id=f.namespace_id AND v.id=f.declaration_version
	JOIN declaration_nodes dn ON dn.namespace_id=r.namespace_id AND dn.opening_firing_id=r.id
	WHERE r.namespace_id=$1 AND r.status='completed' AND v.body->'action'->>'kind'=$5
	AND EXISTS (SELECT 1 FROM declaration_minted_markers m WHERE m.namespace_id=r.namespace_id AND m.firing_id=r.id AND m.artifact_kind=$6)
	AND NOT EXISTS (SELECT 1 FROM signal_events se WHERE se.namespace_id=r.namespace_id AND se.run_id=r.id AND se.name=$7)
) due ORDER BY 1,2 LIMIT $2`

// ReactionCandidates implements ReactionBackend.
func (p PostgresBackend) ReactionCandidates(ctx context.Context, namespaceID string, limit int) ([]ReactionCandidate, error) {
	if limit <= 0 {
		limit = DefaultDriverBatch
	}
	rows, err := p.Store.Pool().Query(ctx, reactionCandidatesSQL, namespaceID, limit, ReactionHumanDecision, ReactionCodeResult,
		agentWorkAction, agentWorkDispatchMarker(), ReactionAgentResult, string(kinds.ArtifactAgentWork))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReactionCandidate
	for rows.Next() {
		var c ReactionCandidate
		if err := rows.Scan(&c.FiringID, &c.Reaction, &c.ArtifactKind, &c.Nonce, &c.MAC, &c.NodeName, &c.Subject); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// agentWorkAction is the action kind whose completed runs are owed an
// agent.result reaction.
const agentWorkAction = "agent.work"

// agentWorkDispatchMarker is the kind the engine mints an agent.work
// firing's dispatch marker for: Produces[0] (engine.go), github.pr today.
func agentWorkDispatchMarker() string {
	k, ok := kinds.Action(agentWorkAction)
	if !ok || len(k.Produces) == 0 {
		return ""
	}
	return string(k.Produces[0])
}

// EmitReaction implements ReactionBackend under a per-run advisory lock.
func (p PostgresBackend) EmitReaction(ctx context.Context, namespaceID string, c ReactionCandidate, stamp ReactionStamp) (postgres.SignalEvent, string, string, bool, error) {
	none := postgres.SignalEvent{}
	if c.Reaction == "" {
		c.Reaction = c.ArtifactKind
	}
	tx, err := p.Store.Pool().Begin(ctx)
	if err != nil {
		return none, "", "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "decl-reaction:"+namespaceID+":"+c.FiringID); err != nil {
		return none, "", "", false, err
	}
	var already bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM signal_events WHERE namespace_id=$1 AND run_id=$2 AND name=$3)`,
		namespaceID, c.FiringID, c.Reaction).Scan(&already); err != nil || already {
		return none, "", "", false, err
	}
	payload, outcome, artifactID, ok, err := reactionFacts(ctx, tx, namespaceID, c)
	if err != nil || !ok {
		return none, "", "", false, err
	}
	if c.Nonce == "" {
		// agent.result: the control plane is the stamper of the agent_work
		// run, so it mints that marker itself -- once, under this lock; a
		// pass that minted and then failed left the marker for this one.
		if c, err = agentWorkMarker(ctx, tx, namespaceID, c, stamp); err != nil {
			return none, "", "", false, err
		}
	}
	// A marker reconcileRun already bound (the run output named an
	// artifact_id) keeps that binding; RecordArtifact refuses to rebind.
	// reconcileRun never binds an agent_work marker (dispatch.go).
	var bound string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(artifact_id,'') FROM declaration_minted_markers
		WHERE namespace_id=$1 AND firing_id=$2 AND artifact_kind=$3 AND nonce=$4`,
		namespaceID, c.FiringID, c.ArtifactKind, c.Nonce).Scan(&bound); err != nil {
		return none, "", "", false, err
	}
	if bound != "" {
		artifactID = bound
	} else if err := stamp.Bind(ctx, c.marker(), artifactID); err != nil {
		return none, "", "", false, err
	}
	payload["node"] = c.NodeName
	payload["firing_id"] = c.FiringID
	payload["outcome"] = outcome
	payload["origin"] = map[string]any{"marker": c.marker(), "artifact_kind": c.ArtifactKind, "artifact_id": artifactID}
	raw, err := json.Marshal(payload)
	if err != nil {
		return none, "", "", false, err
	}
	ev := postgres.SignalEvent{ID: store.NewULID(), NamespaceID: namespaceID, RunID: c.FiringID, Name: c.Reaction, Payload: raw, Emitter: DeclarationEngineActorID}
	if _, err := tx.Exec(ctx, `INSERT INTO signal_events(id,namespace_id,run_id,name,payload,emitter) VALUES($1,$2,$3,$4,$5,$6)`,
		ev.ID, ev.NamespaceID, ev.RunID, ev.Name, ev.Payload, ev.Emitter); err != nil {
		return none, "", "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return none, "", "", false, err
	}
	return ev, outcome, artifactID, true, nil
}

// agentWorkMarker returns c with the firing's agent_work marker: the oldest
// one already minted, or a fresh one from stamp.Mint. The caller holds the
// run's advisory lock, so two passes never both mint.
func agentWorkMarker(ctx context.Context, tx pgx.Tx, namespaceID string, c ReactionCandidate, stamp ReactionStamp) (ReactionCandidate, error) {
	err := tx.QueryRow(ctx, `SELECT nonce,mac FROM declaration_minted_markers WHERE namespace_id=$1 AND firing_id=$2 AND artifact_kind=$3
		ORDER BY created_at,id LIMIT 1`, namespaceID, c.FiringID, c.ArtifactKind).Scan(&c.Nonce, &c.MAC)
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return c, err
	}
	marker, err := stamp.Mint(ctx, c.FiringID, c.ArtifactKind)
	if err != nil {
		return c, err
	}
	parsed, ok := parseMarker(marker)
	if !ok || parsed.firingID != c.FiringID || parsed.kind != c.ArtifactKind {
		return c, errors.New("declengine: minted agent_work marker does not name its firing")
	}
	c.Nonce, c.MAC = parsed.nonce, parsed.mac
	return c, nil
}

// reactionFacts reads what the run's single "action" node (workerEnvelope
// always compiles the firing's action into a node of that name) produced:
// its domain outcome, the artifact id, and the reaction's extra payload.
// ok is false when the artifact is not resolvable yet.
func reactionFacts(ctx context.Context, tx pgx.Tx, namespaceID string, c ReactionCandidate) (payload map[string]any, outcome, artifactID string, ok bool, err error) {
	var nodeRunID string
	err = tx.QueryRow(ctx, `SELECT id,COALESCE(outcome,'') FROM node_runs WHERE namespace_id=$1 AND run_id=$2 AND node_key='action'
		ORDER BY created_at DESC,id DESC LIMIT 1`, namespaceID, c.FiringID).Scan(&nodeRunID, &outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", "", false, nil
	}
	if err != nil {
		return nil, "", "", false, err
	}
	payload = map[string]any{}
	switch c.ArtifactKind {
	case ReactionHumanDecision:
		var status string
		var response []byte
		err = tx.QueryRow(ctx, `SELECT id,status,COALESCE(response,'{}'::jsonb) FROM human_tasks
			WHERE namespace_id=$1 AND run_id=$2 AND node_run_id=$3 AND status<>'pending'
			ORDER BY resolved_at DESC NULLS LAST,created_at DESC,id DESC LIMIT 1`, namespaceID, c.FiringID, nodeRunID).Scan(&artifactID, &status, &response)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", "", false, nil
		}
		if err != nil {
			return nil, "", "", false, err
		}
		payload["human_task_id"] = artifactID
		payload["decision"] = json.RawMessage(response)
	case ReactionCodeResult:
		err = tx.QueryRow(ctx, `SELECT operation_id FROM runner_invocations WHERE namespace_id=$1 AND run_id=$2 AND node_run_id=$3
			ORDER BY attempt DESC LIMIT 1`, namespaceID, c.FiringID, nodeRunID).Scan(&artifactID)
		if errors.Is(err, pgx.ErrNoRows) {
			artifactID, err = c.FiringID, nil
		}
		if err != nil {
			return nil, "", "", false, err
		}
		var result []byte
		if err := tx.QueryRow(ctx, `SELECT COALESCE(output,'{}'::jsonb) FROM runs WHERE namespace_id=$1 AND id=$2`, namespaceID, c.FiringID).Scan(&result); err != nil {
			return nil, "", "", false, err
		}
		payload["result"] = json.RawMessage(result)
	case string(kinds.ArtifactAgentWork):
		// The artifact is the agent_work run itself, so its id is the run
		// id. The outcome is the one the agent reported (workerEnvelope
		// declares the contract's outcomes, so it is not collapsed): a
		// proposed claim the reaction relays, never evidence (PRD §10.4) --
		// nothing here writes a ledger record, and outcome_authority says so
		// to every declaration that conditions on it.
		artifactID = c.FiringID
		var result []byte
		if err := tx.QueryRow(ctx, `SELECT COALESCE(output,'{}'::jsonb) FROM runs WHERE namespace_id=$1 AND id=$2`, namespaceID, c.FiringID).Scan(&result); err != nil {
			return nil, "", "", false, err
		}
		payload["result"] = json.RawMessage(result)
		payload["outcome_authority"] = "proposed"
	default:
		return nil, "", "", false, nil
	}
	return payload, outcome, artifactID, true, nil
}

var _ ReactionBackend = PostgresBackend{}
