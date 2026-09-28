package declactions_test

// The migrated pr-upkeep declarations chain end to end (issue #328, task
// t31b). This test loads the REAL files under examples/pr-upkeep/declarations
// for the readiness -> human-merges-pr -> finish segment -- their triggers,
// conditions, start and landing nodes, approver and inputs as authored -- and
// the manifest's links between them. The only thing the harness supplies is
// what makes the two code.run actions runnable here: `uses` and `operation`
// point at the conformance runner service instead of the production
// runner://headspace/pr-upkeep-readiness and runner://headspace/docker
// registrations. It replaces t38c's TestPRUpkeepChainIsOneLineage, which had
// to supply the chained node names and reaction triggers itself because t31's
// files did not chain.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

const prUpkeepDeclarations = "../../../examples/pr-upkeep/declarations"

// realDeclaration parses one migrated declaration file exactly as publish
// would, so trigger defaults are the parser's, not the harness's.
func realDeclaration(t *testing.T, file string) decl.Declaration {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(prUpkeepDeclarations, file))
	if err != nil {
		t.Fatal(err)
	}
	d, err := decl.Parse(raw, decl.FormatJSON)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return *d
}

// runnableHere repoints a code.run action's runner at the conformance runner
// service, keeping every other field of its `with` block (input, timeout,
// provenance) as authored.
func runnableHere(t *testing.T, d decl.Declaration) decl.Declaration {
	t.Helper()
	var with map[string]any
	if err := json.Unmarshal(d.Action.With, &with); err != nil {
		t.Fatal(err)
	}
	with["uses"] = codeRunnerRef
	with["operation"] = map[string]any{"image": "python:3.12-slim@" + codeImageDigest, "argv": []string{"python3", "-V"},
		"network": "none", "allowedOutputPaths": []string{}}
	raw, err := json.Marshal(with)
	if err != nil {
		t.Fatal(err)
	}
	d.Action.With = raw
	return d
}

func TestPRUpkeepSweepTimerChain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		exit    int
		target  string
		outcome string
	}{
		{"passed", 0, "swept.json", "passed"},
		{"failed", 1, "sweep-failed.json", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sweep := realDeclaration(t, "sweep.json")
			passed := realDeclaration(t, "swept.json")
			failed := realDeclaration(t, "sweep-failed.json")
			if sweep.Trigger.Kind != "timer" || passed.StartNode.Name != sweep.LandingNode.Name || failed.StartNode.Name != sweep.LandingNode.Name {
				t.Fatal("real sweep declarations do not form the timer chain")
			}
			r := newReactionHarness(t)
			r.runner.exitCode = tc.exit
			a := r.publishReal(runnableHere(t, sweep))
			b := r.publishReal(runnableHere(t, passed))
			c := r.publishReal(runnableHere(t, failed))
			manifestBytes, err := os.ReadFile(filepath.Join(prUpkeepDeclarations, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				Links []struct{ From, To, Kind string } `json:"links"`
			}
			if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
				t.Fatal(err)
			}
			versions := map[string]postgres.DeclarationVersion{sweep.Name: a, passed.Name: b, failed.Name: c}
			linked := 0
			for _, link := range manifest.Links {
				from, okFrom := versions[link.From]
				to, okTo := versions[link.To]
				if !okFrom || !okTo {
					continue
				}
				if err := r.db.LinkDeclarations(r.ctx, r.ns, from.DeclarationID, to.DeclarationID, link.Kind); err != nil {
					t.Fatal(err)
				}
				linked++
			}
			if linked < 2 {
				t.Fatalf("manifest supplied %d sweep links, want both branches", linked)
			}
			d := r.scheduleTick(json.RawMessage(`{"schedule":"pr-upkeep-sweep-5m"}`), declengine.Router{Engine: r.engine, Switch: r.sw})
			if d.DeclarationErr != nil {
				t.Fatal(d.DeclarationErr)
			}
			fa := r.onlyFiring(a)
			if state := r.settle(fa.id, false); state != "completed" {
				t.Fatalf("sweep run state %s", state)
			}
			r.drive()
			var target postgres.DeclarationVersion
			if tc.target == "swept.json" {
				target = b
			} else {
				target = c
			}
			child := r.assertContinues(fa, target, "code.result", tc.outcome, r.runner.operations()[0].OperationID)
			if child.lineage != fa.lineage {
				t.Fatal("sweep reaction lost lineage")
			}
			other := c
			if target.ID == c.ID {
				other = b
			}
			if firings := r.firings(other); len(firings) != 0 {
				t.Fatalf("wrong branch fired: %+v", firings)
			}
		})
	}
}

func TestPRUpkeepSweepIgnoresTimerWithoutSchedule(t *testing.T) {
	r := newReactionHarness(t)
	sweep := r.publishReal(runnableHere(t, realDeclaration(t, "sweep.json")))
	d := r.scheduleTick(json.RawMessage(`{}`), declengine.Router{Engine: r.engine, Switch: r.sw})
	if d.DeclarationErr != nil {
		t.Fatal(d.DeclarationErr)
	}
	if got := r.lastOutcome(d.Event.ID, sweep); got != declengine.OutcomeConditionFalse {
		t.Fatalf("outcome %s", got)
	}
	if fs := r.firings(sweep); len(fs) != 0 {
		t.Fatalf("empty timer fired: %+v", fs)
	}
}

// publishReal publishes and activates d as authored (no default rewriting,
// and its own exposes list) and approves every listed entry, as the owner
// would.
func (r *reactionHarness) publishReal(d decl.Declaration) postgres.DeclarationVersion {
	t := r.t
	t.Helper()
	body, err := d.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.db.PublishDeclaration(r.ctx, postgres.PublishDeclarationInput{NamespaceID: r.ns, Name: d.Name, Body: body, Author: "human"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.db.RecordDeclarationActivation(r.ctx, r.ns, v.ID, "activate", "human", ""); err != nil {
		t.Fatal(err)
	}
	r.approveWidenings(v, d)
	return v
}

func TestPRUpkeepDeclarationsChainIsOneLineage(t *testing.T) {
	readinessSrc := realDeclaration(t, "readiness.json")
	askSrc := realDeclaration(t, "human-merges-pr.json")
	finishSrc := realDeclaration(t, "finish.json")
	// The chain the files must form, checked before anything runs: each
	// declaration starts where its predecessor lands and triggers on the
	// reaction to its predecessor's action.
	if askSrc.StartNode.Name != readinessSrc.LandingNode.Name || askSrc.Trigger.Kind != "code.result" || readinessSrc.Action.Kind != "code.run" {
		t.Fatalf("human-merges-pr (%s on %s) does not react to readiness (%s landing on %s)",
			askSrc.Trigger.Kind, askSrc.StartNode.Name, readinessSrc.Action.Kind, readinessSrc.LandingNode.Name)
	}
	if finishSrc.StartNode.Name != askSrc.LandingNode.Name || finishSrc.Trigger.Kind != "human.decision" || askSrc.Action.Kind != "human.ask" {
		t.Fatalf("finish (%s on %s) does not react to human-merges-pr (%s landing on %s)",
			finishSrc.Trigger.Kind, finishSrc.StartNode.Name, askSrc.Action.Kind, askSrc.LandingNode.Name)
	}

	r := newReactionHarness(t)
	versions := map[string]postgres.DeclarationVersion{
		readinessSrc.Name: r.publishReal(runnableHere(t, readinessSrc)),
		askSrc.Name:       r.publishReal(askSrc),
		finishSrc.Name:    r.publishReal(runnableHere(t, finishSrc)),
	}
	readiness, merges, finish := versions[readinessSrc.Name], versions[askSrc.Name], versions[finishSrc.Name]

	// The manifest's links inside the segment. Links to declarations outside
	// it (route, stage-pr-open) are left out: the segment starts mid-chain,
	// so they could never be in this lineage.
	raw, err := os.ReadFile(filepath.Join(prUpkeepDeclarations, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Links []struct{ From, To, Kind string } `json:"links"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	finishMustFollowsAsk := false
	for _, l := range manifest.Links {
		from, okFrom := versions[l.From]
		to, okTo := versions[l.To]
		if !okFrom || !okTo {
			continue
		}
		if err := r.db.LinkDeclarations(r.ctx, r.ns, from.DeclarationID, to.DeclarationID, l.Kind); err != nil {
			t.Fatal(err)
		}
		if l.From == finishSrc.Name && l.To == askSrc.Name && l.Kind == "must" {
			finishMustFollowsAsk = true
		}
	}
	if !finishMustFollowsAsk {
		t.Fatalf("manifest has no must link %s -> %s", finishSrc.Name, askSrc.Name)
	}

	// readiness starts on stage-pr-open's landing node, on the jira.comment
	// reaction to the comment stage-pr-open posted.
	ev, err := r.db.DeliverSignalEvent(r.ctx, postgres.DeliverSignalEventInput{NamespaceID: r.ns, Name: readinessSrc.Trigger.Kind, Emitter: "conformance"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Handle(r.ctx, declengine.Event{NamespaceID: r.ns, ID: ev.Event.ID, Kind: readinessSrc.Trigger.Kind,
		Node: readinessSrc.StartNode.Name, Variables: map[string]any{"comment_id": "10001"}}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	fr := r.onlyFiring(readiness)
	if state := r.settle(fr.id, false); state != "completed" {
		t.Fatalf("readiness ended %s, want completed", state)
	}
	r.drive()
	fm := r.assertContinues(fr, merges, "code.result", "passed", r.runner.operations()[0].OperationID)
	task := r.decide(fm.id, "approved")
	r.drive()
	ff := r.assertContinues(fm, finish, "human.decision", "approved", task)
	if n := r.lineageLen(ff.id); n != 3 {
		t.Fatalf("finish's lineage has %d entries, want 3 (finish, human-merges-pr, readiness)", n)
	}
	if ff.lineage != fr.lineage {
		t.Fatalf("finish lineage %s != readiness lineage %s", ff.lineage, fr.lineage)
	}
	if got := r.lastOutcome(ff.eventID, finish); got != declengine.OutcomeFired {
		t.Fatalf("finish outcome = %q, want %q", got, declengine.OutcomeFired)
	}
}

// The analyse -> stage-dispatch hop over the real files (task t38e): route
// decides, its code.result starts analyse (agent.work on the developer
// lane), the agent reports `packaged` against analyse's migrated contract,
// and the agent.result reaction starts stage-dispatch -- whose condition
// reads that outcome -- in the same lineage. finish-no-fix, the other
// branch off analyse's landing node, is evaluated and does not fire.
func TestPRUpkeepAnalyseToStageDispatchIsOneLineage(t *testing.T) {
	routeSrc := realDeclaration(t, "route.json")
	analyseSrc := realDeclaration(t, "analyse.json")
	stageSrc := realDeclaration(t, "stage-dispatch.json")
	noFixSrc := realDeclaration(t, "finish-no-fix.json")
	if analyseSrc.Trigger.Kind != "code.result" || analyseSrc.StartNode.Name != routeSrc.LandingNode.Name || analyseSrc.Action.Kind != "agent.work" {
		t.Fatalf("analyse (%s on %s, %s) does not react to route (landing on %s)", analyseSrc.Trigger.Kind, analyseSrc.StartNode.Name, analyseSrc.Action.Kind, routeSrc.LandingNode.Name)
	}
	for _, d := range []decl.Declaration{stageSrc, noFixSrc} {
		if d.Trigger.Kind != "agent.result" || d.StartNode.Name != analyseSrc.LandingNode.Name {
			t.Fatalf("%s (%s on %s) does not react to analyse (landing on %s)", d.Name, d.Trigger.Kind, d.StartNode.Name, analyseSrc.LandingNode.Name)
		}
	}

	r := newReactionHarness(t)
	// The files name production actors; the harness registers stand-ins
	// under exactly those keys, so `uses` stays as authored.
	developer := r.addBridge(actors.ActorKeyOf(usesOf(t, analyseSrc)))
	jira := r.addJiraBridge(actors.ActorKeyOf(usesOf(t, stageSrc)))
	developer.reply("packaged", map[string]any{
		"verdicts": []any{map[string]any{"id": "f1", "verdict": "FIX", "reason": "unanswered and real"}},
		"packages": []any{map[string]any{"rule": "go:S1192", "file": "internal/x.go", "finding_ids": []any{"f1"}}},
	})
	versions := map[string]postgres.DeclarationVersion{
		routeSrc.Name:   r.publishReal(runnableHere(t, routeSrc)),
		analyseSrc.Name: r.publishReal(analyseSrc),
		stageSrc.Name:   r.publishReal(stageSrc),
		noFixSrc.Name:   r.publishReal(runnableHere(t, noFixSrc)),
	}
	route, analyse, stage, noFix := versions[routeSrc.Name], versions[analyseSrc.Name], versions[stageSrc.Name], versions[noFixSrc.Name]

	raw, err := os.ReadFile(filepath.Join(prUpkeepDeclarations, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Links []struct{ From, To, Kind string } `json:"links"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	stageMustFollowAnalyse := false
	for _, l := range manifest.Links {
		from, okFrom := versions[l.From]
		to, okTo := versions[l.To]
		if !okFrom || !okTo {
			continue
		}
		if err := r.db.LinkDeclarations(r.ctx, r.ns, from.DeclarationID, to.DeclarationID, l.Kind); err != nil {
			t.Fatal(err)
		}
		if l.From == stageSrc.Name && l.To == analyseSrc.Name && l.Kind == "must" {
			stageMustFollowAnalyse = true
		}
	}
	if !stageMustFollowAnalyse {
		t.Fatalf("manifest has no must link %s -> %s", stageSrc.Name, analyseSrc.Name)
	}

	// The sweep's work-item fact for a keyed (non-gh:) item starts route.
	item := map[string]any{"source": "github_pr", "repository": "agentculture/culture-nodes", "number": 328, "head_sha": "abc123",
		"work_item": "SCRUM-7", "findings": []any{map[string]any{"id": "f1", "rule": "go:S1192", "file": "internal/x.go"}}}
	payload, _ := json.Marshal(item)
	ev, err := r.db.DeliverSignalEvent(r.ctx, postgres.DeliverSignalEventInput{NamespaceID: r.ns, Name: routeSrc.Trigger.Kind, Payload: payload, Emitter: "conformance"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Handle(r.ctx, declengine.Event{NamespaceID: r.ns, ID: ev.Event.ID, Kind: routeSrc.Trigger.Kind, Node: declengine.RootNode, Variables: item}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	fr := r.onlyFiring(route)
	if state := r.settle(fr.id, false); state != "completed" {
		t.Fatalf("route ended %s, want completed", state)
	}
	r.drive()
	fa := r.assertContinues(fr, analyse, "code.result", "passed", r.runner.operations()[0].OperationID)
	if state := r.settle(fa.id, false); state != "completed" {
		t.Fatalf("analyse reporting packaged ended %s, want completed", state)
	}
	if n := len(developer.received()); n != 1 {
		t.Fatalf("developer lane invoked %d times, want 1", n)
	}
	r.drive()
	rx := r.agentResults(fa.id)
	if len(rx) != 1 || rx[0].Payload.Outcome != "packaged" || rx[0].Payload.Origin.ArtifactKind != "agent.work" || rx[0].Payload.Origin.ArtifactID != fa.id {
		t.Fatalf("analyse's agent.result = %+v, want one carrying packaged with its agent_work origin", rx)
	}
	fs := r.onlyFiring(stage)
	if fs.parent != fa.id || fs.lineage != fr.lineage || fs.eventID != rx[0].ID {
		t.Fatalf("stage-dispatch firing = %+v, want parent %s in route's lineage %s via %s", fs, fa.id, fr.lineage, rx[0].ID)
	}
	if n := r.lineageLen(fs.id); n != 3 {
		t.Fatalf("stage-dispatch's lineage has %d entries, want 3 (stage-dispatch, analyse, route)", n)
	}
	if got := r.lastOutcome(fs.eventID, stage); got != declengine.OutcomeFired {
		t.Fatalf("stage-dispatch outcome = %q, want %q", got, declengine.OutcomeFired)
	}
	if got := r.lastOutcome(fs.eventID, noFix); got != declengine.OutcomeConditionFalse {
		t.Fatalf("finish-no-fix outcome on packaged = %q, want %q", got, declengine.OutcomeConditionFalse)
	}
	if fs := r.firings(noFix); len(fs) != 0 {
		t.Fatalf("finish-no-fix fired on packaged: %+v", fs)
	}
	if state := r.settle(fs.id, false); state != "completed" {
		t.Fatalf("stage-dispatch ended %s, want completed", state)
	}
	if got := jira.received(); len(got) != 1 || got[0]["issue"] != "SCRUM-7" {
		t.Fatalf("jira comment lane received %v, want one comment on SCRUM-7", got)
	}
}

func usesOf(t *testing.T, d decl.Declaration) string {
	t.Helper()
	var with struct {
		Uses string `json:"uses"`
	}
	if err := json.Unmarshal(d.Action.With, &with); err != nil || with.Uses == "" {
		t.Fatalf("%s names no actor: %v", d.Name, err)
	}
	return with.Uses
}

// Claude's real final answer can end in a JSON declaration after prose. The
// fake bridge derives its §13.2 response from that final text.
func TestFakeClaudeFinalMapsProseAndTrailingJSON(t *testing.T) {
	b := &fakeBridge{key: "culture/developer"}
	b.replyClaudeFinal("The only GitHub credential returned 401.\n\n" +
		`{"outcome":"blocked","output":{"reason":"No usable GitHub write credential."}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/invoke", strings.NewReader(`{"input":{}}`))
	req.Header.Set("Authorization", "Bearer "+bridgeToken)
	w := httptest.NewRecorder()
	b.invoke(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("fake Claude bridge status %d: %s", w.Code, w.Body.String())
	}
	var result actors.InvocationResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "blocked" || output["summary"] != "The only GitHub credential returned 401." {
		t.Fatalf("fake Claude final mapped to outcome %q, output %+v", result.Outcome, output)
	}
}

func TestPRUpkeepBlockedDeclarationIsHumanAsk(t *testing.T) {
	analyse := realDeclaration(t, "analyse.json")
	blocked := realDeclaration(t, "blocked-analyse.json")
	if blocked.StartNode.Name != analyse.LandingNode.Name || blocked.Trigger.Kind != "agent.result" || blocked.Condition != `event.outcome == "blocked"` || blocked.Action.Kind != "human.ask" {
		t.Fatalf("blocked route no longer leads from agent.result to human.ask: %+v", blocked)
	}
}

func TestPRUpkeepBlockedAgentRoutesToHuman(t *testing.T) {
	for _, choice := range []string{"retry", "abandon", "acknowledged"} {
		t.Run(choice, func(t *testing.T) {
			target := choice
			if choice == "acknowledged" {
				target = "acknowledge"
			}
			routeSrc := realDeclaration(t, "route.json")
			analyseSrc := realDeclaration(t, "analyse.json")
			blockedSrc := realDeclaration(t, "blocked-analyse.json")
			closeSrc := realDeclaration(t, target+"-analyse.json")
			if blockedSrc.StartNode.Name != analyseSrc.LandingNode.Name || blockedSrc.Trigger.Kind != "agent.result" || closeSrc.StartNode.Name != blockedSrc.LandingNode.Name {
				t.Fatal("blocked route does not follow the real agent and human landing nodes")
			}
			r := newReactionHarness(t)
			developer := r.addBridge(actors.ActorKeyOf(usesOf(t, analyseSrc)))
			developer.replyClaudeFinal("The only GitHub credential available is rejected with `401 Bad credentials`.\nEvidence:\n- The PR edit received 401.\n\n" +
				`{"outcome":"blocked","output":{"reason":"GitHub token returned 401","pr":"agentculture/culture-nodes#329"}}`)
			versions := map[string]postgres.DeclarationVersion{
				routeSrc.Name:   r.publishReal(runnableHere(t, routeSrc)),
				analyseSrc.Name: r.publishReal(analyseSrc),
				blockedSrc.Name: r.publishReal(blockedSrc),
				closeSrc.Name: r.publishReal(func() decl.Declaration {
					if choice == "retry" {
						return closeSrc
					}
					// Point the authored recorder at the fake runner while keeping
					// its actual operation and input intact.
					var with map[string]any
					if err := json.Unmarshal(closeSrc.Action.With, &with); err != nil {
						t.Fatal(err)
					}
					with["uses"] = codeRunnerRef
					body, err := json.Marshal(with)
					if err != nil {
						t.Fatal(err)
					}
					closeSrc.Action.With = body
					return closeSrc
				}()),
			}
			route, analyse, blocked, close := versions[routeSrc.Name], versions[analyseSrc.Name], versions[blockedSrc.Name], versions[closeSrc.Name]
			raw, err := os.ReadFile(filepath.Join(prUpkeepDeclarations, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				Links []struct{ From, To, Kind string } `json:"links"`
			}
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			for _, link := range manifest.Links {
				from, okFrom := versions[link.From]
				to, okTo := versions[link.To]
				if okFrom && okTo {
					if err := r.db.LinkDeclarations(r.ctx, r.ns, from.DeclarationID, to.DeclarationID, link.Kind); err != nil {
						t.Fatal(err)
					}
				}
			}
			item := map[string]any{"source": "github_pr", "repository": "agentculture/culture-nodes", "number": 328, "head_sha": "abc123", "work_item": "SCRUM-7", "findings": []any{map[string]any{"id": "f1", "rule": "go:S1192", "file": "internal/x.go"}}}
			payload, _ := json.Marshal(item)
			ev, err := r.db.DeliverSignalEvent(r.ctx, postgres.DeliverSignalEventInput{NamespaceID: r.ns, Name: routeSrc.Trigger.Kind, Payload: payload, Emitter: "conformance"})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.engine.Handle(r.ctx, declengine.Event{NamespaceID: r.ns, ID: ev.Event.ID, Kind: routeSrc.Trigger.Kind, Node: declengine.RootNode, Variables: item}); err != nil {
				t.Fatal(err)
			}
			fr := r.onlyFiring(route)
			if state := r.settle(fr.id, false); state != "completed" {
				t.Fatalf("route: %s", state)
			}
			r.drive()
			fa := r.assertContinues(fr, analyse, "code.result", "passed", r.runner.operations()[0].OperationID)
			if state := r.settle(fa.id, false); state != "completed" {
				t.Fatalf("blocked agent run: %s", state)
			}
			r.drive()
			rx := r.agentResults(fa.id)
			if len(rx) != 1 || rx[0].Payload.Outcome != "blocked" || rx[0].Payload.Origin.ArtifactKind != "agent.work" || rx[0].Payload.Origin.ArtifactID != fa.id {
				t.Fatalf("blocked agent.result = %+v", rx)
			}
			fh := r.onlyFiring(blocked)
			if fh.parent != fa.id || fh.lineage != fr.lineage || fh.eventID != rx[0].ID {
				t.Fatalf("blocked human firing = %+v, want child of %s", fh, fa.id)
			}
			var input []byte
			if err := r.db.Pool().QueryRow(r.ctx, `SELECT input FROM runs WHERE namespace_id=$1 AND id=$2`, r.ns, fh.id).Scan(&input); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(input), "GitHub token returned 401") || !strings.Contains(string(input), "401 Bad credentials") || !strings.Contains(string(input), "agentculture/culture-nodes") || !strings.Contains(string(input), "analyse") {
				t.Fatalf("human input missing context: %s", input)
			}
			var humanInput map[string]any
			if err := json.Unmarshal(input, &humanInput); err != nil {
				t.Fatal(err)
			}
			if humanInput["ticket"] != "" {
				t.Fatalf("keyed PR without an orphan ticket rendered ticket %v", humanInput["ticket"])
			}
			if state := r.settle(fh.id, true); state != "running" || r.pendingTask(fh.id) == "" {
				t.Fatalf("human ask state %s, task %q", state, r.pendingTask(fh.id))
			}
			var request []byte
			if err := r.db.Pool().QueryRow(r.ctx, `SELECT request FROM human_tasks WHERE namespace_id=$1 AND id=$2`, r.ns, r.pendingTask(fh.id)).Scan(&request); err != nil {
				t.Fatal(err)
			}
			var taskRequest struct {
				AllowedOutcomes []string `json:"allowed_outcomes"`
			}
			if err := json.Unmarshal(request, &taskRequest); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(taskRequest.AllowedOutcomes, []string{"abandon", "acknowledged", "expired", "retry"}) {
				t.Fatalf("human task choices = %v", taskRequest.AllowedOutcomes)
			}
			task := r.decide(fh.id, choice)
			r.drive()
			fc := r.assertContinues(fh, close, "human.decision", choice, task)
			if choice == "retry" {
				if fc.id == "" || closeSrc.Action.Kind != "agent.work" {
					t.Fatal("retry did not dispatch the agent")
				}
				var firstInput, retryInput []byte
				for _, row := range []struct {
					id  string
					dst *[]byte
				}{{fa.id, &firstInput}, {fc.id, &retryInput}} {
					if err := r.db.Pool().QueryRow(r.ctx, `SELECT input FROM runs WHERE namespace_id=$1 AND id=$2`, r.ns, row.id).Scan(row.dst); err != nil {
						t.Fatal(err)
					}
				}
				var first, again map[string]any
				if err := json.Unmarshal(firstInput, &first); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(retryInput, &again); err != nil {
					t.Fatal(err)
				}
				delete(first, declengine.MarkerInputKey)
				delete(again, declengine.MarkerInputKey)
				if !reflect.DeepEqual(first, again) {
					t.Fatalf("retry input differs from original: %v / %v", first, again)
				}
				return
			}
			if state := r.settle(fc.id, false); state != "completed" {
				t.Fatalf("%s blocked: %s", choice, state)
			}
			var recordInput []byte
			if err := r.db.Pool().QueryRow(r.ctx, `SELECT input FROM runs WHERE namespace_id=$1 AND id=$2`, r.ns, fc.id).Scan(&recordInput); err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(recordInput, &record); err != nil {
				t.Fatal(err)
			}
			if record["decision"] != choice || record["blocked_step"] != "analyse" || record["work_item"] != "SCRUM-7" {
				t.Fatalf("record input = %v", record)
			}
		})
	}
}
