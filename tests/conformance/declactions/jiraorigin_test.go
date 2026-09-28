package declactions_test

// Jira reactions continue their lineage (issue #328, task t38f). Over the
// REAL declaration files, a jira.comment or jira.transition action's bridge
// stamps its marker into the comment it posts; the Jira webhook -- the real
// internal/api route, hydrating the issue from a fake Jira REST API -- reads
// the marker back and passes `origin` with the configured bot account, and
// the engine continues the firing's lineage only when marker.go verify
// accepts it. A marker a person copied into their own comment does not.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

const (
	jiraIntakeDeclarations = "../../../examples/jira-intake/declarations"
	jiraBot                = "712020:bot"
	jiraHuman              = "712020:person"
)

func realDeclarationIn(t *testing.T, dir, file string) decl.Declaration {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	d, err := decl.Parse(raw, decl.FormatJSON)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return *d
}

// linkManifest links every manifest link between published versions.
func (r *reactionHarness) linkManifest(dir string, versions map[string]postgres.DeclarationVersion) {
	t := r.t
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Links []struct{ From, To, Kind string } `json:"links"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, l := range manifest.Links {
		from, okFrom := versions[l.From]
		to, okTo := versions[l.To]
		if okFrom && okTo {
			if err := r.db.LinkDeclarations(r.ctx, r.ns, from.DeclarationID, to.DeclarationID, l.Kind); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func adfText(s string) map[string]any {
	return map[string]any{"type": "doc", "version": 1, "content": []any{
		map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": s}}}}}
}

func jiraComment(id, created, author, body string) map[string]any {
	return map[string]any{"id": id, "created": created, "author": map[string]any{"accountId": author}, "body": adfText(body)}
}

// stampedComment is what the jira bridge posts: its text, its actor marker,
// then stamping.stamp_text's cn1 marker on a line of its own.
func stampedComment(text, marker string) string {
	return text + "\n\n[culture-nodes:jira-actor]\n\n" + marker
}

func jiraIssue(id, key, status, creator string, comments, histories []any) map[string]any {
	return map[string]any{"id": id, "key": key, "fields": map[string]any{
		"summary": "Stamped artifacts", "description": adfText("Please pick this up."),
		"priority": map[string]any{"name": "High"}, "status": map[string]any{"name": status},
		"issuetype": map[string]any{"name": "Task"}, "created": "2026-09-28T09:00:00.000+0000",
		"creator": map[string]any{"accountId": creator},
		"comment": map[string]any{"total": len(comments), "comments": comments}},
		"changelog": map[string]any{"total": len(histories), "histories": histories}}
}

type webhookFact struct {
	Fact struct {
		Name      string          `json:"name"`
		SourceKey string          `json:"source_key"`
		Payload   json.RawMessage `json:"payload"`
	} `json:"fact"`
	Delivery struct {
		Event struct {
			ID string `json:"id"`
		} `json:"event"`
		Duplicate bool `json:"duplicate"`
	} `json:"delivery"`
}

// jiraWebhook POSTs one Jira wake-up for issue through the real webhook
// route, configured with bot as its bridge account, and returns the facts
// it delivered keyed by source key.
func (r *reactionHarness) jiraWebhook(bot string, issue map[string]any) map[string]webhookFact {
	t := r.t
	t.Helper()
	body, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer jira.Close()
	srv, err := apipkg.NewServer(r.db, r.ns,
		apipkg.WithJiraWebhook("", "wake-token", jira.URL, "team.example.com", "SCRUM", "reader@example.com", "api-token", bot),
		apipkg.WithDeclarationEngine(r.engine))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	wake := `{"webhookEvent":"comment_created","issue":{"key":"` + issue["key"].(string) + `"}}`
	srv.AccessHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1alpha1/webhooks/jira?token=wake-token", strings.NewReader(wake)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("Jira webhook: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Deliveries []webhookFact `json:"deliveries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	facts := map[string]webhookFact{}
	for _, d := range out.Deliveries {
		facts[d.Fact.SourceKey] = d
	}
	return facts
}

// fresh returns the delivered, non-duplicate fact under key.
func fresh(t *testing.T, facts map[string]webhookFact, key, name string) webhookFact {
	t.Helper()
	f, ok := facts[key]
	if !ok || f.Fact.Name != name || f.Delivery.Duplicate || f.Delivery.Event.ID == "" {
		keys := make([]string, 0, len(facts))
		for k := range facts {
			keys = append(keys, k)
		}
		t.Fatalf("no fresh %s at %s; delivered %v", name, key, keys)
	}
	return f
}

func handedMarker(t *testing.T, b *fakeBridge, i int) string {
	t.Helper()
	got := b.received()
	if len(got) <= i {
		t.Fatalf("bridge received %d invocations, want > %d", len(got), i)
	}
	marker, _ := got[i][declengine.MarkerInputKey].(string)
	if marker == "" {
		t.Fatalf("invocation %d carried no marker", i)
	}
	return marker
}

// stage-dispatch's jira.comment -> the webhook's jira.comment reaction with
// origin -> fix fires in the same lineage. A person's comment carrying the
// copied marker, delivered first, fires nothing.
func TestPRUpkeepStageDispatchCommentContinuesToFix(t *testing.T) {
	routeSrc := realDeclaration(t, "route.json")
	analyseSrc := realDeclaration(t, "analyse.json")
	stageSrc := realDeclaration(t, "stage-dispatch.json")
	fixSrc := realDeclaration(t, "fix.json")
	if fixSrc.Trigger.Kind != "jira.comment" || fixSrc.StartNode.Name != stageSrc.LandingNode.Name || stageSrc.Action.Kind != "jira.comment" {
		t.Fatalf("fix (%s on %s) does not react to stage-dispatch (%s landing on %s)", fixSrc.Trigger.Kind, fixSrc.StartNode.Name, stageSrc.Action.Kind, stageSrc.LandingNode.Name)
	}
	r := newReactionHarness(t)
	developer := r.addBridge(actors.ActorKeyOf(usesOf(t, analyseSrc)))
	jira := r.addJiraBridge(actors.ActorKeyOf(usesOf(t, stageSrc)))
	developer.reply("packaged", map[string]any{
		"verdicts": []any{map[string]any{"id": "f1", "verdict": "FIX", "reason": "unanswered and real"}},
		"packages": []any{map[string]any{"rule": "go:S1192", "file": "internal/x.go", "finding_ids": []any{"f1"}}},
	})
	jira.nextArtifacts("30001")
	versions := map[string]postgres.DeclarationVersion{
		routeSrc.Name:   r.publishReal(runnableHere(t, routeSrc)),
		analyseSrc.Name: r.publishReal(analyseSrc),
		stageSrc.Name:   r.publishReal(stageSrc),
		fixSrc.Name:     r.publishReal(fixSrc),
	}
	r.linkManifest(prUpkeepDeclarations, versions)
	route, analyse, stage, fix := versions[routeSrc.Name], versions[analyseSrc.Name], versions[stageSrc.Name], versions[fixSrc.Name]

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
	fa := r.onlyFiring(analyse)
	if state := r.settle(fa.id, false); state != "completed" {
		t.Fatalf("analyse ended %s, want completed", state)
	}
	r.drive()
	fs := r.onlyFiring(stage)
	if state := r.settle(fs.id, false); state != "completed" {
		t.Fatalf("stage-dispatch ended %s, want completed", state)
	}
	marker := handedMarker(t, jira, 0)
	if !strings.HasPrefix(marker, "cn1:"+fs.id+":jira.comment:") {
		t.Fatalf("jira bridge was handed %q, want stage-dispatch's jira.comment marker", marker)
	}
	botComment := jiraComment("30001", "2026-09-28T10:00:00.000+0000", jiraBot, stampedComment("culture-nodes:stage=dispatch\nDispatched.", marker))
	copied := jiraComment("30000", "2026-09-28T09:59:00.000+0000", jiraHuman, "the bot said "+marker)

	// A person pasting the marker into their own comment: the emitter
	// passes the origin with the person as author, the engine rejects it.
	facts := r.jiraWebhook(jiraBot, jiraIssue("10007", "SCRUM-7", "In Progress", jiraHuman, []any{copied}, nil))
	human := fresh(t, facts, "jira:team.example.com:SCRUM-7:comment:30000", "jira.comment")
	if reason := r.markerRejection(human.Delivery.Event.ID); reason != "marker not bound to artifact" {
		t.Fatalf("copied marker: rejection %q, want the artifact binding's", reason)
	}
	if fs := r.firings(fix); len(fs) != 0 {
		t.Fatalf("a copied marker fired fix: %+v", fs)
	}

	facts = r.jiraWebhook(jiraBot, jiraIssue("10007", "SCRUM-7", "In Progress", jiraHuman, []any{copied, botComment}, nil))
	if _, legacy := facts["jira:team.example.com:SCRUM-7:history:comment:30001"]; legacy {
		t.Fatal("the bot's own comment raised the legacy pr-upkeep.jira.comment")
	}
	reaction := fresh(t, facts, "jira:team.example.com:SCRUM-7:comment:30001", "jira.comment")
	var p struct {
		Origin map[string]string `json:"origin"`
	}
	if err := json.Unmarshal(reaction.Fact.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Origin["marker"] != marker || p.Origin["artifact_id"] != "30001" || p.Origin["artifact_kind"] != "jira.comment" || p.Origin["author"] != jiraBot || p.Origin["bridge_account"] != jiraBot {
		t.Fatalf("reaction origin = %v", p.Origin)
	}
	ff := r.onlyFiring(fix)
	if ff.parent != fs.id || ff.lineage != fr.lineage || ff.eventID != reaction.Delivery.Event.ID {
		t.Fatalf("fix firing = %+v, want parent %s in route's lineage %s via %s", ff, fs.id, fr.lineage, reaction.Delivery.Event.ID)
	}
	if n := r.lineageLen(ff.id); n != 4 {
		t.Fatalf("fix's lineage has %d entries, want 4 (fix, stage-dispatch, analyse, route)", n)
	}
	if got := r.lastOutcome(ff.eventID, fix); got != declengine.OutcomeFired {
		t.Fatalf("fix outcome = %q, want %q", got, declengine.OutcomeFired)
	}
}

type intakeChain struct {
	versions            map[string]postgres.DeclarationVersion
	intake, post, trans string
	fi, fp              firingRow
	jira                *fakeBridge
}

// runIntakeToPostComment publishes the jira-intake chain, delivers the
// issue's creation through the webhook and settles intake and post-comment.
func runIntakeToPostComment(r *reactionHarness, files ...string) intakeChain {
	t := r.t
	t.Helper()
	c := intakeChain{versions: map[string]postgres.DeclarationVersion{}}
	srcs := map[string]decl.Declaration{}
	for _, f := range append([]string{"intake.json", "post-comment.json", "transition.json"}, files...) {
		d := realDeclarationIn(t, jiraIntakeDeclarations, f)
		srcs[f] = d
		if d.Action.Kind == "code.run" {
			d = runnableHere(t, d)
		}
		c.versions[d.Name] = r.publishReal(d)
	}
	c.intake, c.post, c.trans = srcs["intake.json"].Name, srcs["post-comment.json"].Name, srcs["transition.json"].Name
	if srcs["transition.json"].Trigger.Kind != "jira.comment" || srcs["transition.json"].StartNode.Name != srcs["post-comment.json"].LandingNode.Name {
		t.Fatal("transition does not react to post-comment's jira.comment")
	}
	r.linkManifest(jiraIntakeDeclarations, c.versions)
	agent := r.addBridge(actors.ActorKeyOf(usesOf(t, srcs["intake.json"])))
	agent.reply("intake_drafted", map[string]any{"summary": "Drafted intake."})
	c.jira = r.addJiraBridge(actors.ActorKeyOf(usesOf(t, srcs["post-comment.json"])))
	c.jira.nextArtifacts("40001", "40002")

	facts := r.jiraWebhook(jiraBot, jiraIssue("10008", "SCRUM-8", "To Do", jiraHuman, nil, nil))
	fresh(t, facts, "jira:team.example.com:SCRUM-8:created", "jira.issue.created")
	c.fi = r.onlyFiring(c.versions[c.intake])
	if state := r.settle(c.fi.id, false); state != "completed" {
		t.Fatalf("intake ended %s, want completed", state)
	}
	r.drive()
	c.fp = r.onlyFiring(c.versions[c.post])
	if c.fp.parent != c.fi.id || c.fp.lineage != c.fi.lineage {
		t.Fatalf("post-comment firing = %+v, want parent %s", c.fp, c.fi.id)
	}
	if state := r.settle(c.fp.id, false); state != "completed" {
		t.Fatalf("post-comment ended %s, want completed", state)
	}
	return c
}

// post-comment's jira.comment -> the jira.comment reaction -> transition,
// then transition's marker comment -> jira.issue.transitioned ->
// stage-intake, all in intake's lineage. A copied marker fires nothing.
func TestJiraIntakeCommentAndTransitionContinueTheLineage(t *testing.T) {
	r := newReactionHarness(t)
	c := runIntakeToPostComment(r, "stage-intake.json", "picked-up-gh.json")
	commentMarker := handedMarker(t, c.jira, 0)
	if !strings.HasPrefix(commentMarker, "cn1:"+c.fp.id+":jira.comment:") {
		t.Fatalf("post-comment handed %q", commentMarker)
	}
	transition := c.versions[c.trans]

	copied := jiraComment("39999", "2026-09-28T09:30:00.000+0000", jiraHuman, "copying "+commentMarker)
	facts := r.jiraWebhook(jiraBot, jiraIssue("10008", "SCRUM-8", "To Do", jiraHuman, []any{copied}, nil))
	human := fresh(t, facts, "jira:team.example.com:SCRUM-8:comment:39999", "jira.comment")
	if reason := r.markerRejection(human.Delivery.Event.ID); reason != "marker not bound to artifact" {
		t.Fatalf("copied marker: rejection %q", reason)
	}
	if fs := r.firings(transition); len(fs) != 0 {
		t.Fatalf("a copied marker fired transition: %+v", fs)
	}

	posted := jiraComment("40001", "2026-09-28T09:40:00.000+0000", jiraBot, stampedComment("Drafted intake.", commentMarker))
	facts = r.jiraWebhook(jiraBot, jiraIssue("10008", "SCRUM-8", "To Do", jiraHuman, []any{copied, posted}, nil))
	reaction := fresh(t, facts, "jira:team.example.com:SCRUM-8:comment:40001", "jira.comment")
	ft := r.onlyFiring(transition)
	if ft.parent != c.fp.id || ft.lineage != c.fi.lineage || ft.eventID != reaction.Delivery.Event.ID {
		t.Fatalf("transition firing = %+v, want parent %s in lineage %s", ft, c.fp.id, c.fi.lineage)
	}
	if state := r.settle(ft.id, false); state != "completed" {
		t.Fatalf("transition ended %s, want completed", state)
	}

	// The jira bridge moves the ticket as the bot and posts the marker
	// comment; the move itself stays self-echo, the comment is the reaction.
	transitionMarker := handedMarker(t, c.jira, 1)
	if !strings.HasPrefix(transitionMarker, "cn1:"+ft.id+":jira.issue:") {
		t.Fatalf("transition handed %q, want its jira.issue marker", transitionMarker)
	}
	move := map[string]any{"id": "900", "created": "2026-09-28T09:50:00.000+0000", "author": map[string]any{"accountId": jiraBot},
		"items": []any{map[string]any{"field": "status", "fromString": "To Do", "toString": "In Progress"}}}
	markerComment := jiraComment("40002", "2026-09-28T09:50:01.000+0000", jiraBot, transitionMarker)
	facts = r.jiraWebhook(jiraBot, jiraIssue("10008", "SCRUM-8", "In Progress", jiraHuman, []any{copied, posted, markerComment}, []any{move}))
	moved := fresh(t, facts, "jira:team.example.com:SCRUM-8:transitioned:In Progress:comment:40002", "jira.issue.transitioned")
	stageSrc := realDeclarationIn(t, jiraIntakeDeclarations, "stage-intake.json")
	ghSrc := realDeclarationIn(t, jiraIntakeDeclarations, "picked-up-gh.json")
	fsi := r.onlyFiring(c.versions[stageSrc.Name])
	if fsi.parent != ft.id || fsi.lineage != c.fi.lineage || fsi.eventID != moved.Delivery.Event.ID {
		t.Fatalf("stage-intake firing = %+v, want parent %s in lineage %s", fsi, ft.id, c.fi.lineage)
	}
	if n := r.lineageLen(fsi.id); n != 4 {
		t.Fatalf("stage-intake's lineage has %d entries, want 4", n)
	}
	if got := r.lastOutcome(moved.Delivery.Event.ID, c.versions[ghSrc.Name]); got != declengine.OutcomeConditionFalse {
		t.Fatalf("picked-up-gh on a keyed issue = %q, want %q", got, declengine.OutcomeConditionFalse)
	}
}

// The artifact id matches, but the comment at it was not written by the
// configured bridge account: the emitter names its real author and verify's
// author check refuses to continue the lineage.
func TestJiraCommentNotByTheBridgeAccountIsRejected(t *testing.T) {
	r := newReactionHarness(t)
	c := runIntakeToPostComment(r)
	marker := handedMarker(t, c.jira, 0)
	forged := jiraComment("40001", "2026-09-28T09:40:00.000+0000", jiraHuman, stampedComment("Drafted intake.", marker))
	facts := r.jiraWebhook(jiraBot, jiraIssue("10008", "SCRUM-8", "To Do", jiraHuman, []any{forged}, nil))
	reaction := fresh(t, facts, "jira:team.example.com:SCRUM-8:comment:40001", "jira.comment")
	if reason := r.markerRejection(reaction.Delivery.Event.ID); reason != "artifact author is not bridge account" {
		t.Fatalf("rejection %q, want the author check's", reason)
	}
	if fs := r.firings(c.versions[c.trans]); len(fs) != 0 {
		t.Fatalf("a comment the bridge did not write fired transition: %+v", fs)
	}
}
