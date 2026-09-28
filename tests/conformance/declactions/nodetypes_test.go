package declactions_test

// Engine-derived node types and start_from (issue #328, task t38d, owner
// decision d6). A landing node's `host` and `actor_kind` are recorded by the
// engine from facts it recorded itself -- the action kind it dispatched
// (human.ask -> human, code.run -> code) and the registration of the actor
// the firing run's attempt ran on (metadata.harness, and the bridge-measured
// capabilities.preflight.host.hostname) -- never from what the action
// returned, what a reaction's payload says, or what a declaration asks for.
// A declaration with `start_from` fires from open nodes by those types.
//
// Everything runs through the real surfaces (reactionHarness): the
// production engine, the real worker and actors-table registry, the real
// human-task and runner-service paths, and the scheduler's Driver.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// addTypedBridge registers a stamping bridge the way register-actor.sh /
// cutover.sh register a harness lane: metadata.harness names the engine and
// the capability surface carries the host the bridge measured itself on.
// Empty harness or hostname registers the row without that fact.
func (r *reactionHarness) addTypedBridge(key, harness, hostname string) {
	r.t.Helper()
	b := newFakeBridge(r.t, key)
	r.bridges[key] = b
	metadata := map[string]any{"auth_token_env": bridgeTokenEnv}
	if harness != "" {
		metadata["harness"] = harness
	}
	caps := map[string]any{"stamping": map[string]any{"marker": "cn1", "version": 1}}
	if hostname != "" {
		caps["preflight"] = map[string]any{"host": map[string]any{"hostname": hostname}}
	}
	md, _ := json.Marshal(metadata)
	cp, _ := json.Marshal(caps)
	if _, err := r.db.Pool().Exec(r.ctx, `INSERT INTO actors(id,namespace_id,actor_key,revision,kind,protocol,endpoint_ref,metadata,capabilities) VALUES($1,$2,$3,1,'agent','nodes.actor/v1alpha1',$4,$5,$6)`,
		store.NewULID(), r.ns, key, b.server.URL, md, cp); err != nil {
		r.t.Fatal(err)
	}
}

// agentOn is agentDecl aimed at the bridge registered under key.
func agentOn(name, key, start, landing string) decl.Declaration {
	d := agentDecl(name, "timer", start, landing)
	d.Action.With = json.RawMessage(`{"uses":"actor://` + key + `@sha256:` + strings.Repeat("a", 64) + `","input":{"instruction":"analyse"}}`)
	return d
}

// nodeTypes reads the recorded types of the node firing opened, "" = unset.
func (r *reactionHarness) nodeTypes(firing string) (host, actorKind string) {
	r.t.Helper()
	if err := r.db.Pool().QueryRow(r.ctx, `SELECT COALESCE(host,''),COALESCE(actor_kind,'') FROM declaration_nodes WHERE namespace_id=$1 AND opening_firing_id=$2`,
		r.ns, firing).Scan(&host, &actorKind); err != nil {
		r.t.Fatalf("node of firing %s: %v", firing, err)
	}
	return host, actorKind
}

// evaluationCount counts v's evaluation rows for one event.
func (r *reactionHarness) evaluationCount(eventID string, v postgres.DeclarationVersion) int {
	r.t.Helper()
	var n int
	if err := r.db.Pool().QueryRow(r.ctx, `SELECT count(*) FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3`,
		r.ns, eventID, v.DeclarationID).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// runChain starts a's chain from start and settles its run into an
// agent.result reaction, returning a's firing and that reaction's event id.
func (r *reactionHarness) runAgentChain(a postgres.DeclarationVersion, start string) (firingRow, string) {
	r.t.Helper()
	r.start(start)
	fa := r.onlyFiring(a)
	if state := r.settle(fa.id, false); state != "completed" {
		r.t.Fatalf("agent.work run ended %s, want completed", state)
	}
	r.drive()
	rx := r.agentResults(fa.id)
	if len(rx) != 1 {
		r.t.Fatalf("%d agent.result reactions for run %s, want 1", len(rx), fa.id)
	}
	return fa, rx[0].ID
}

// A bridge actor run, a human.ask and a code.run each record the node types
// the engine can derive from its own records -- and nothing the action
// itself reported: the bridge's output claims another host and kind.
func TestNodeTypesRecordedFromEngineFacts(t *testing.T) {
	r := newReactionHarness(t)
	r.addTypedBridge("culture/claude-spark", "claude", "spark")
	r.bridges["culture/claude-spark"].reply("completed", map[string]any{"host": "orin", "actor_kind": "codex", "node": "elsewhere"})
	agent := r.publish(agentOn("nt-agent", "culture/claude-spark", "ready-agent", "analysed"))
	ask := r.publish(askDecl("nt-ask", "ready-ask", "asked"))
	code := r.publish(codeDecl("nt-code", "timer", "ready-code", "ran"))

	r.start("ready-agent")
	fAgent := r.onlyFiring(agent)
	if host, kind := r.nodeTypes(fAgent.id); host != "" || kind != "" {
		t.Fatalf("agent node types before any attempt = host %q kind %q, want both unset (no fact recorded yet)", host, kind)
	}
	if state := r.settle(fAgent.id, false); state != "completed" {
		t.Fatalf("agent run ended %s", state)
	}
	r.drive()
	if host, kind := r.nodeTypes(fAgent.id); host != "spark" || kind != "claude" {
		t.Fatalf("bridge run node types = host %q kind %q, want spark/claude from the attempt's actor registration", host, kind)
	}

	r.start("ready-ask")
	fAsk := r.onlyFiring(ask)
	if host, kind := r.nodeTypes(fAsk.id); host != "" || kind != "human" {
		t.Fatalf("human.ask node types at open = host %q kind %q, want unset/human", host, kind)
	}
	r.decide(fAsk.id, "approved")
	r.drive()
	if host, kind := r.nodeTypes(fAsk.id); host != "" || kind != "human" {
		t.Fatalf("human.ask node types after the decision = host %q kind %q, want unset/human (a person has no recorded host)", host, kind)
	}

	r.start("ready-code")
	fCode := r.onlyFiring(code)
	if _, kind := r.nodeTypes(fCode.id); kind != "code" {
		t.Fatalf("code.run node actor_kind at open = %q, want code", kind)
	}
	if state := r.settle(fCode.id, false); state != "completed" {
		t.Fatalf("code run ended %s", state)
	}
	r.drive()
	// The harness's runner actor is registered without a host fact, so the
	// host stays unset: the engine never guesses one from an endpoint URL.
	if host, kind := r.nodeTypes(fCode.id); host != "" || kind != "code" {
		t.Fatalf("code.run node types = host %q kind %q, want unset/code", host, kind)
	}
}

// start_from any fires from a non-root node whose name it does not declare:
// its start_node names a node nothing ever opens.
func TestStartFromAnyFiresFromANonRootNode(t *testing.T) {
	r := newReactionHarness(t)
	r.addTypedBridge("culture/claude-spark", "claude", "spark")
	a := r.publish(agentOn("sf-any-a", "culture/claude-spark", "ready", "analysed"))
	bd := onAgentResult("sf-any-b", "true", "never-opened", "staged")
	bd.StartFrom = &decl.StartFrom{Any: true}
	b := r.publish(bd)
	plain := r.publish(onAgentResult("sf-any-plain", "true", "never-opened", "other"))
	r.must(b, a)
	fa, reaction := r.runAgentChain(a, "ready")
	fb := r.onlyFiring(b)
	if fb.parent != fa.id || fb.lineage != fa.lineage || fb.eventID != reaction {
		t.Fatalf("start_from any firing = %+v, want parent %s in lineage %s via %s", fb, fa.id, fa.lineage, reaction)
	}
	if got := r.lastOutcome(reaction, b); got != declengine.OutcomeFired {
		t.Fatalf("start_from any outcome = %q, want fired", got)
	}
	// Without start_from, the declared start node still decides: unchanged.
	if fs := r.firings(plain); len(fs) != 0 || r.evaluationCount(reaction, plain) != 0 {
		t.Fatalf("a declaration without start_from fired from a node it does not start on: %+v", fs)
	}
}

// start_from {actor_kind: codex} fires after a codex action and not after a
// claude one, and explain names the type it needed. MUST links still hold.
func TestStartFromActorKindFiresOnlyAfterThatKind(t *testing.T) {
	r := newReactionHarness(t)
	r.addTypedBridge("culture/codex-orin", "codex", "orin")
	r.addTypedBridge("culture/claude-spark", "claude", "spark")
	aCodex := r.publish(agentOn("sf-kind-codex", "culture/codex-orin", "ready-codex", "analysed-codex"))
	aClaude := r.publish(agentOn("sf-kind-claude", "culture/claude-spark", "ready-claude", "analysed-claude"))
	bd := onAgentResult("sf-kind-b", "true", "unused", "reviewed")
	bd.StartFrom = &decl.StartFrom{ActorKind: "codex"}
	b := r.publish(bd)

	_, claudeReaction := r.runAgentChain(aClaude, "ready-claude")
	if fs := r.firings(b); len(fs) != 0 {
		t.Fatalf("start_from {actor_kind: codex} fired after a claude action: %+v", fs)
	}
	res, found, err := (declengine.PostgresBackend{Store: r.db}).Explain(r.ctx, r.ns, claudeReaction, "sf-kind-b")
	if err != nil || !found {
		t.Fatalf("explain after the claude action: found=%v err=%v", found, err)
	}
	if res.Outcome != declengine.OutcomeStartUnmatched || !strings.Contains(res.Reason, "actor_kind=codex") || !strings.Contains(res.Reason, "actor_kind=claude") {
		t.Fatalf("explain = %q / %q, want %q naming the needed actor_kind=codex and the node's actor_kind=claude", res.Outcome, res.Reason, declengine.OutcomeStartUnmatched)
	}

	fCodex, codexReaction := r.runAgentChain(aCodex, "ready-codex")
	fb := r.onlyFiring(b)
	if fb.parent != fCodex.id || fb.eventID != codexReaction {
		t.Fatalf("start_from {actor_kind: codex} firing = %+v, want parent %s via %s", fb, fCodex.id, codexReaction)
	}
}

// {host, actor_kind} is AND: only the node recorded with both fires.
func TestStartFromHostAndActorKindIsAnd(t *testing.T) {
	r := newReactionHarness(t)
	r.addTypedBridge("culture/codex-thor-2", "codex", "thor")
	r.addTypedBridge("culture/codex-orin", "codex", "orin")
	r.addTypedBridge("culture/claude-thor", "claude", "thor")
	r.addTypedBridge("culture/codex-nohost", "codex", "")
	chains := map[string]postgres.DeclarationVersion{
		"codex@thor":   r.publish(agentOn("sf-and-ct", "culture/codex-thor-2", "ready-ct", "done-ct")),
		"codex@orin":   r.publish(agentOn("sf-and-co", "culture/codex-orin", "ready-co", "done-co")),
		"claude@thor":  r.publish(agentOn("sf-and-lt", "culture/claude-thor", "ready-lt", "done-lt")),
		"codex@nohost": r.publish(agentOn("sf-and-cn", "culture/codex-nohost", "ready-cn", "done-cn")),
	}
	bd := onAgentResult("sf-and-b", "true", "unused", "reviewed")
	bd.StartFrom = &decl.StartFrom{Host: "thor", ActorKind: "codex"}
	b := r.publish(bd)
	starts := map[string]string{"codex@thor": "ready-ct", "codex@orin": "ready-co", "claude@thor": "ready-lt", "codex@nohost": "ready-cn"}
	for _, which := range []string{"codex@orin", "claude@thor", "codex@nohost"} {
		_, reaction := r.runAgentChain(chains[which], starts[which])
		if fs := r.firings(b); len(fs) != 0 {
			t.Fatalf("{host: thor, actor_kind: codex} fired after %s: %+v", which, fs)
		}
		if got := r.lastOutcome(reaction, b); got != declengine.OutcomeStartUnmatched {
			t.Fatalf("after %s: outcome %q, want %q", which, got, declengine.OutcomeStartUnmatched)
		}
	}
	fa, reaction := r.runAgentChain(chains["codex@thor"], starts["codex@thor"])
	fb := r.onlyFiring(b)
	if fb.parent != fa.id || fb.eventID != reaction {
		t.Fatalf("{host, actor_kind} firing = %+v, want parent %s via %s", fb, fa.id, reaction)
	}
}

// A declaration with start_from never fires from root: an outside event,
// or any event without a verified marker, is not an engine-opened node.
func TestStartFromNeverMatchesRoot(t *testing.T) {
	r := newReactionHarness(t)
	bd := jiraDecl("sf-root", "timer", declengine.RootNode, "done")
	bd.StartFrom = &decl.StartFrom{Any: true}
	b := r.publish(bd)
	d, err := r.db.DeliverSignalEvent(r.ctx, postgres.DeliverSignalEventInput{NamespaceID: r.ns, Name: "timer", Payload: json.RawMessage(`{"node":"root"}`),
		Emitter: "conformance", Declarations: declengine.Router{Engine: r.engine, Switch: r.sw}})
	if err != nil || d.DeclarationErr != nil {
		t.Fatalf("deliver: err=%v declarationErr=%v", err, d.DeclarationErr)
	}
	if fs := r.firings(b); len(fs) != 0 {
		t.Fatalf("start_from any fired from root: %+v", fs)
	}
	if got := r.lastOutcome(d.Event.ID, b); got != declengine.OutcomeStartUnmatched {
		t.Fatalf("outcome at root = %q, want %q", got, declengine.OutcomeStartUnmatched)
	}
}

// markerRejection returns the reason the origin-marker check recorded for
// eventID ("" when none was recorded).
func (r *reactionHarness) markerRejection(eventID string) string {
	r.t.Helper()
	var reason string
	err := r.db.Pool().QueryRow(r.ctx, `SELECT reason FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2 AND declaration_id='origin-marker'
		ORDER BY created_at DESC,id DESC LIMIT 1`, r.ns, eventID).Scan(&reason)
	if err != nil {
		return ""
	}
	return reason
}
