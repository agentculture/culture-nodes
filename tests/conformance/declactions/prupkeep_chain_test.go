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
	"os"
	"path/filepath"
	"testing"

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
