package declactions_test

// agent.result, the reaction to a completed agent.work run (issue #328, task
// t38e, owner decision d7). Like human.decision and code.result, nothing
// external reports it: the scheduler Driver's reaction pass emits it, with
// the domain outcome the agent reported (not collapsed to `completed`), and
// with a marker the control plane mints for the agent_work run and binds to
// the run id, so a declaration reacting to it continues the same lineage.
//
// The agent's outcome is its claim, never evidence: the reaction says so
// (outcome_authority "proposed"), and a forged agent.result carrying the
// PR's public github.pr marker does not continue anything.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

const codexThor = "culture/codex-thor"

// agentDecl is an agent.work declaration on codex-thor. outcomes, when not
// empty, is its migrated contract (with.graph_config.contract.outcomes),
// each outcome with an object schema.
func agentDecl(name, trigger, start, landing string, outcomes ...string) decl.Declaration {
	with := map[string]any{"uses": "actor://" + codexThor + "@sha256:" + strings.Repeat("a", 64), "input": map[string]any{"instruction": "analyse"}}
	if len(outcomes) > 0 {
		contract := map[string]any{}
		for _, o := range outcomes {
			contract[o] = map[string]any{"schema": map[string]any{"type": "object"}}
		}
		with["graph_config"] = map[string]any{"contract": map[string]any{"outcomes": contract}}
	}
	raw, _ := json.Marshal(with)
	return decl.Declaration{Name: name, Trigger: decl.Trigger{Kind: trigger},
		Action:    decl.Action{Kind: "agent.work", With: raw},
		StartNode: decl.Node{Name: start, Deadline: "none"}, LandingNode: decl.Node{Name: landing, Deadline: "1h"}}
}

// onAgentResult reacts to agent.result on start when condition holds.
func onAgentResult(name, condition, start, landing string) decl.Declaration {
	d := jiraDecl(name, "agent.result", start, landing)
	d.Condition = condition
	return d
}

type agentReaction struct {
	ID      string
	Payload struct {
		Node             string         `json:"node"`
		Outcome          string         `json:"outcome"`
		OutcomeAuthority string         `json:"outcome_authority"`
		Result           map[string]any `json:"result"`
		Origin           struct {
			Marker       string `json:"marker"`
			ArtifactKind string `json:"artifact_kind"`
			ArtifactID   string `json:"artifact_id"`
		} `json:"origin"`
	}
}

func (r *reactionHarness) agentResults(run string) []agentReaction {
	t := r.t
	t.Helper()
	rows, err := r.db.Pool().Query(r.ctx, `SELECT id,payload FROM signal_events WHERE namespace_id=$1 AND run_id=$2 AND name='agent.result' ORDER BY created_at,id`, r.ns, run)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []agentReaction
	for rows.Next() {
		var x agentReaction
		var raw []byte
		if err := rows.Scan(&x.ID, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &x.Payload); err != nil {
			t.Fatalf("agent.result payload %s: %v", raw, err)
		}
		out = append(out, x)
	}
	return out
}

func (r *reactionHarness) signalCount(run string) int {
	r.t.Helper()
	var n int
	if err := r.db.Pool().QueryRow(r.ctx, `SELECT count(*) FROM signal_events WHERE namespace_id=$1 AND run_id=$2`, r.ns, run).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// assertAgentResult pins t38e's reaction: exactly one agent.result for the
// parent's run, carrying the agent's own outcome as a proposed claim, its
// output, and an origin whose marker the engine minted for the agent_work
// run and bound to the run id; the child hangs off the parent in one lineage.
func (r *reactionHarness) assertAgentResult(parent firingRow, child postgres.DeclarationVersion, wantOutcome string) firingRow {
	t := r.t
	t.Helper()
	rx := r.agentResults(parent.id)
	if len(rx) != 1 {
		t.Fatalf("%d agent.result reactions for run %s, want exactly 1", len(rx), parent.id)
	}
	p := rx[0].Payload
	if p.Outcome != wantOutcome || p.OutcomeAuthority != "proposed" || p.Node == "" || p.Result["bridge"] != codexThor {
		t.Fatalf("agent.result payload = %+v, want outcome %q as a proposed claim with the run's output", p, wantOutcome)
	}
	if p.Origin.ArtifactKind != "agent.work" || p.Origin.ArtifactID != parent.id || !strings.HasPrefix(p.Origin.Marker, "cn1:"+parent.id+":agent.work:") {
		t.Fatalf("agent.result origin = %+v, want an agent.work marker of firing %s bound to its run id", p.Origin, parent.id)
	}
	c := r.onlyFiring(child)
	if c.parent != parent.id || c.lineage != parent.lineage || c.eventID != rx[0].ID {
		t.Fatalf("child firing = %+v, want parent %s in lineage %s via reaction %s", c, parent.id, parent.lineage, rx[0].ID)
	}
	return c
}

// The agent's reported outcome is carried unchanged, and it is what the
// reacting declarations' conditions branch on: `packaged` fires one branch,
// `no_fix` the other, each in the agent.work firing's lineage. Re-running
// the pass emits nothing new.
func TestAgentWorkOutcomeEmitsAgentResult(t *testing.T) {
	for _, tc := range []struct{ outcome, fires, skips string }{
		{"packaged", "ar-packaged", "ar-no-fix"},
		{"no_fix", "ar-no-fix", "ar-packaged"},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			r := newReactionHarness(t)
			r.bridges[codexThor].reply(tc.outcome, map[string]any{"verdicts": []string{"f1"}})
			a := r.publish(agentDecl("ar-analyse", "timer", "ready", "analysed", "packaged", "no_fix"))
			branches := map[string]postgres.DeclarationVersion{
				"ar-packaged": r.publish(onAgentResult("ar-packaged", `event.outcome == "packaged"`, "analysed", "staged")),
				"ar-no-fix":   r.publish(onAgentResult("ar-no-fix", `event.outcome == "no_fix"`, "analysed", "finished")),
			}
			for _, b := range branches {
				r.must(b, a)
			}
			r.start("ready")
			fa := r.onlyFiring(a)
			if state := r.settle(fa.id, false); state != "completed" {
				t.Fatalf("agent.work run reporting %q ended %s, want completed", tc.outcome, state)
			}
			r.drive()
			r.drive()
			fb := r.assertAgentResult(fa, branches[tc.fires], tc.outcome)
			if n := r.lineageLen(fb.id); n != 2 {
				t.Fatalf("%s's lineage has %d entries, want 2", tc.fires, n)
			}
			if got := r.lastOutcome(fb.eventID, branches[tc.fires]); got != declengine.OutcomeFired {
				t.Fatalf("%s outcome = %q, want %q", tc.fires, got, declengine.OutcomeFired)
			}
			if fs := r.firings(branches[tc.skips]); len(fs) != 0 {
				t.Fatalf("%s fired on outcome %q: %+v", tc.skips, tc.outcome, fs)
			}
			if got := r.lastOutcome(fb.eventID, branches[tc.skips]); got != declengine.OutcomeConditionFalse {
				t.Fatalf("%s outcome = %q, want %q", tc.skips, got, declengine.OutcomeConditionFalse)
			}
			before := r.signalCount(fa.id)
			r.drive()
			if after := r.signalCount(fa.id); after != before || len(r.agentResults(fa.id)) != 1 {
				t.Fatalf("a third pass emitted more: %d -> %d signal events for run %s", before, after, fa.id)
			}
		})
	}
}

// A failed agent.work run keeps emitting only its action.* result: an
// outcome the contract does not declare is the engine's contract_rejected,
// a technical failure, and no agent.result is emitted for it.
func TestFailedAgentWorkRunEmitsNoAgentResult(t *testing.T) {
	r := newReactionHarness(t)
	r.bridges[codexThor].reply("exploded", nil)
	a := r.publish(agentDecl("ar-fail", "timer", "ready", "analysed", "packaged", "no_fix"))
	b := r.publish(onAgentResult("ar-fail-after", "true", "analysed", "staged"))
	r.must(b, a)
	r.start("ready")
	fa := r.onlyFiring(a)
	if state := r.settle(fa.id, false); state != "failed" {
		t.Fatalf("agent.work run reporting an undeclared outcome ended %s, want failed", state)
	}
	r.drive()
	r.drive()
	if rx := r.agentResults(fa.id); len(rx) != 0 {
		t.Fatalf("a failed run emitted %d agent.result reactions: %+v", len(rx), rx)
	}
	var actionResults int
	if err := r.db.Pool().QueryRow(r.ctx, `SELECT count(*) FROM signal_events WHERE namespace_id=$1 AND run_id=$2 AND name LIKE 'action.%'`, r.ns, fa.id).Scan(&actionResults); err != nil || actionResults != 1 {
		t.Fatalf("failed run emitted %d action.* results (err=%v), want 1", actionResults, err)
	}
	if fs := r.firings(b); len(fs) != 0 {
		t.Fatalf("an agent.result reaction fired for a failed run: %+v", fs)
	}
}

// 'before' emits nothing; 'shadow' emits and evaluates the reaction as a
// would-fire record and dispatches nothing -- exactly t38c's switch rule.
func TestAgentResultObeysTheSwitch(t *testing.T) {
	r := newReactionHarness(t)
	r.bridges[codexThor].reply("packaged", nil)
	a := r.publish(agentDecl("ar-sw", "timer", "ready", "analysed", "packaged", "no_fix"))
	b := r.publish(onAgentResult("ar-sw-after", `event.outcome == "packaged"`, "analysed", "staged"))
	r.must(b, a)
	r.start("ready")
	fa := r.onlyFiring(a)
	if state := r.settle(fa.id, false); state != "completed" {
		t.Fatalf("agent.work run ended %s, want completed", state)
	}
	r.flip(declengine.ModeBefore)
	r.drive()
	if rx := r.agentResults(fa.id); len(rx) != 0 {
		t.Fatalf("'before' emitted %d agent.result reactions, want 0", len(rx))
	}
	r.flip(declengine.ModeShadow)
	r.drive()
	r.drive()
	fb := r.assertAgentResult(fa, b, "packaged")
	if got := r.lastOutcome(fb.eventID, b); got != declengine.OutcomeShadow {
		t.Fatalf("reaction outcome in 'shadow' = %q, want %q", got, declengine.OutcomeShadow)
	}
	if n := len(r.bridges["culture/jira"].received()); n != 0 {
		t.Fatalf("'shadow' invoked the jira bridge %d times", n)
	}
}

// The PR's github.pr marker is public -- the bridge stamps it into the pull
// request -- and it verifies to the agent.work firing once the PR is bound.
// An agent.result carrying it instead of the agent_work marker is forged:
// it starts a fresh lineage, so the MUST link is unmet and nothing fires.
// The real reaction still continues the lineage afterwards.
func TestAgentResultWithThePRMarkerIsRejected(t *testing.T) {
	r := newReactionHarness(t)
	r.bridges[codexThor].reply("packaged", nil)
	a := r.publish(agentDecl("ar-forge", "timer", "ready", "analysed", "packaged", "no_fix"))
	b := r.publish(onAgentResult("ar-forge-after", "true", "analysed", "staged"))
	r.must(b, a)
	r.start("ready")
	fa := r.onlyFiring(a)
	if state := r.settle(fa.id, false); state != "completed" {
		t.Fatalf("agent.work run ended %s, want completed", state)
	}
	prMarker, _ := r.bridges[codexThor].received()[0][declengine.MarkerInputKey].(string)
	if !strings.HasPrefix(prMarker, "cn1:"+fa.id+":github.pr:") {
		t.Fatalf("bridge was handed %q, want the firing's github.pr marker", prMarker)
	}
	pr := "artifact-" + strings.ReplaceAll(codexThor, "/", "-")
	payload, _ := json.Marshal(map[string]any{"node": "analysed", "outcome": "no_fix",
		"origin": map[string]string{"marker": prMarker, "artifact_kind": "github.pr", "artifact_id": pr}})
	d, err := r.db.DeliverSignalEvent(r.ctx, postgres.DeliverSignalEventInput{NamespaceID: r.ns, Name: "agent.result", Payload: payload,
		Emitter: "forger", Declarations: declengine.Router{Engine: r.engine, Switch: r.sw}})
	if err != nil || d.DeclarationErr != nil {
		t.Fatalf("deliver forged: err=%v declarationErr=%v", err, d.DeclarationErr)
	}
	if got := r.lastOutcome(d.Event.ID, b); got != declengine.OutcomeLineageMissing {
		t.Fatalf("forged agent.result: outcome %q, want %q (fresh lineage, MUST link unmet)", got, declengine.OutcomeLineageMissing)
	}
	if fs := r.firings(b); len(fs) != 0 {
		t.Fatalf("forged agent.result fired the reacting declaration: %+v", fs)
	}
	r.drive()
	r.assertAgentResult(fa, b, "packaged")
}

// The two markers of one agent.work firing are bound independently: after
// the reaction pass bound the agent_work marker to the run id, a real
// github.pr reaction still binds the PR the run output reports to the
// dispatch marker and continues the lineage -- reconcileRun never tries to
// rebind the agent_work marker to the PR. (The agent.result reactor's
// condition is false here, so the landing node stays open for the PR's.)
func TestPRReactionAfterAgentResultStillContinues(t *testing.T) {
	r := newReactionHarness(t)
	a := r.publish(agentDecl("ar-pr", "timer", "ready", "analysed"))
	b := r.publish(onAgentResult("ar-pr-result", `event.outcome == "never"`, "analysed", "staged"))
	c := r.publish(jiraDecl("ar-pr-created", "github.pr.created", "analysed", "reviewed"))
	r.must(b, a)
	r.must(c, a)
	r.start("ready")
	fa := r.onlyFiring(a)
	if state := r.settle(fa.id, false); state != "completed" {
		t.Fatalf("agent.work run ended %s, want completed", state)
	}
	r.drive()
	rx := r.agentResults(fa.id)
	if len(rx) != 1 || rx[0].Payload.Origin.ArtifactID != fa.id || rx[0].Payload.Outcome != "completed" {
		t.Fatalf("agent.result reactions = %+v, want one bound to run %s with outcome completed", rx, fa.id)
	}

	prMarker, _ := r.bridges[codexThor].received()[0][declengine.MarkerInputKey].(string)
	pr := "artifact-" + strings.ReplaceAll(codexThor, "/", "-")
	payload, _ := json.Marshal(map[string]any{"node": "analysed",
		"origin": map[string]string{"marker": prMarker, "artifact_kind": "github.pr", "artifact_id": pr}})
	d, err := r.db.DeliverSignalEvent(r.ctx, postgres.DeliverSignalEventInput{NamespaceID: r.ns, Name: "github.pr.created", Payload: payload,
		Emitter: "culture/github", Declarations: declengine.Router{Engine: r.engine, Switch: r.sw}})
	if err != nil || d.DeclarationErr != nil {
		t.Fatalf("deliver github.pr.created: err=%v declarationErr=%v", err, d.DeclarationErr)
	}
	fc := r.onlyFiring(c)
	if fc.parent != fa.id || fc.lineage != fa.lineage {
		t.Fatalf("github.pr.created firing = %+v, want parent %s in lineage %s", fc, fa.id, fa.lineage)
	}
	bound := map[string]string{}
	rows, err := r.db.Pool().Query(r.ctx, `SELECT artifact_kind,COALESCE(artifact_id,'') FROM declaration_minted_markers WHERE namespace_id=$1 AND firing_id=$2`, r.ns, fa.id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			t.Fatal(err)
		}
		bound[kind] = id
	}
	if len(bound) != 2 || bound["github.pr"] != pr || bound["agent.work"] != fa.id {
		t.Fatalf("firing %s's markers bind %v, want github.pr -> %s and agent.work -> the run id", fa.id, bound, pr)
	}
}

// The pin runs both ways (gate fix): the agent.work marker is minted by the
// control plane for its own agent.result reaction and is readable in that
// event's payload. Copied onto any other event name -- here a
// github.pr.created claiming the run as its artifact -- it must not verify,
// or a forger could continue the agent firing's lineage on a trigger the
// control plane never emitted.
func TestAgentWorkMarkerOnAnotherEventIsRejected(t *testing.T) {
	r := newReactionHarness(t)
	a := r.publish(agentDecl("ar-copy", "timer", "ready", "analysed"))
	b := r.publish(onAgentResult("ar-copy-result", `event.outcome == "never"`, "analysed", "staged"))
	c := r.publish(jiraDecl("ar-copy-created", "github.pr.created", "analysed", "reviewed"))
	r.must(b, a)
	r.must(c, a)
	r.start("ready")
	fa := r.onlyFiring(a)
	if state := r.settle(fa.id, false); state != "completed" {
		t.Fatalf("agent.work run ended %s, want completed", state)
	}
	r.drive()
	rx := r.agentResults(fa.id)
	if len(rx) != 1 {
		t.Fatalf("agent.result reactions = %+v, want one", rx)
	}
	origin := rx[0].Payload.Origin
	payload, _ := json.Marshal(map[string]any{"node": "analysed",
		"origin": map[string]string{"marker": origin.Marker, "artifact_kind": origin.ArtifactKind, "artifact_id": origin.ArtifactID}})
	d, err := r.db.DeliverSignalEvent(r.ctx, postgres.DeliverSignalEventInput{NamespaceID: r.ns, Name: "github.pr.created", Payload: payload,
		Emitter: "forger", Declarations: declengine.Router{Engine: r.engine, Switch: r.sw}})
	if err != nil || d.DeclarationErr != nil {
		t.Fatalf("deliver copied marker: err=%v declarationErr=%v", err, d.DeclarationErr)
	}
	if got := r.lastOutcome(d.Event.ID, c); got != declengine.OutcomeLineageMissing {
		t.Fatalf("github.pr.created with the agent.work marker: outcome %q, want %q", got, declengine.OutcomeLineageMissing)
	}
	if fs := r.firings(c); len(fs) != 0 {
		t.Fatalf("a copied agent.work marker fired %+v", fs)
	}
}
