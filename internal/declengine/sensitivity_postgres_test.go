package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// publishActiveBy publishes and activates d under author, the identity the
// sensitivity check resolves as the owner of the variables it produces.
func publishActiveBy(t *testing.T, db *postgres.Store, ns, author string, d decl.Declaration) postgres.DeclarationVersion {
	t.Helper()
	body, err := d.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	v, err := db.PublishDeclaration(context.Background(), postgres.PublishDeclarationInput{NamespaceID: ns, Name: d.Name, Body: body, Author: author})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDeclarationActivation(context.Background(), ns, v.ID, "activate", author, ""); err != nil {
		t.Fatal(err)
	}
	return v
}

// announceDeclaration renders the triggering Jira issue's summary into a
// Discord post: the spec's own example of a widening (team -> public).
func announceDeclaration(text string, exposes ...string) decl.Declaration {
	d := active("announce").Declaration
	d.Trigger.Kind = "jira.issue.created"
	d.Condition = "true"
	d.Action = decl.Action{Kind: "discord.post", With: json.RawMessage(`{"uses":"actor://discord","input":{"text":"` + text + `"}}`)}
	d.Exposes = exposes
	return d
}

func sensitivityEngine(t *testing.T, db *postgres.Store, calls *int) *Engine {
	t.Helper()
	t.Setenv("TCA_PG_SENS_KEY", strings.Repeat("k", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_PG_SENS_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db},
		dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
			*calls++
			return DispatchResult{}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// handleKind delivers one event of kind and returns the terminal outcome and
// reason the firing loop recorded for the declaration.
func handleKind(t *testing.T, e *Engine, db *postgres.Store, ns, declID, kind string, vars map[string]any) (string, string) {
	t.Helper()
	eventID := deliver(t, db, ns)
	if err := e.Handle(context.Background(), Event{NamespaceID: ns, ID: eventID, Kind: kind, Node: "ready", Variables: vars}); err != nil {
		t.Fatal(err)
	}
	var outcome, reason string
	if err := db.Pool().QueryRow(context.Background(), `SELECT outcome,reason FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3 ORDER BY created_at DESC,id DESC LIMIT 1`,
		ns, eventID, declID).Scan(&outcome, &reason); err != nil {
		t.Fatal(err)
	}
	return outcome, reason
}

func handleJira(t *testing.T, e *Engine, db *postgres.Store, ns, declID string, vars map[string]any) (string, string) {
	t.Helper()
	return handleKind(t, e, db, ns, declID, "jira.issue.created", vars)
}

func approvalCount(t *testing.T, db *postgres.Store, ns string) int {
	t.Helper()
	var n int
	if err := db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM declaration_exposure_approvals WHERE namespace_id=$1`, ns).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func setVisibility(t *testing.T, db *postgres.Store, ns, repo, vis string) {
	t.Helper()
	if _, err := SetRepositoryVisibility(context.Background(), db, ns, repo, vis, "operator@example.com", "test"); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresDestinationAudienceControlsDiscordExposure(t *testing.T) {
	for _, tc := range []struct{ name, actor, recorded, trigger, sourceRepo, want string }{
		{"recorded-org", "company/notify-discord", "org", "jira.issue.created", "", OutcomeFired},
		{"unrecorded", "company/other-discord", "", "jira.issue.created", "", OutcomeSensitivityBlocked},
		{"recorded-public", "company/notify-discord", "public", "jira.issue.created", "", OutcomeSensitivityBlocked},
		{"public-source", "company/other-discord", "", "github.pr.created", "acme/open", OutcomeFired},
		{"public-to-org", "company/notify-discord", "org", "github.pr.created", "acme/open", OutcomeFired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := pgtest.RequireStore(t, markerTestStore)
			ns := pgtest.MustNamespace(t, db, "tca-destination-"+tc.name).ID
			if tc.recorded != "" {
				if _, err := SetDestinationAudience(context.Background(), db, ns, tc.actor, tc.recorded, "operator@example.com", "test"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.sourceRepo != "" {
				setVisibility(t, db, ns, tc.sourceRepo, "public")
			}
			d := announceDeclaration("{summary}")
			d.Trigger.Kind = tc.trigger
			d.Action.With = json.RawMessage(`{"uses":"actor://` + tc.actor + `@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","input":{"text":"{summary}"}}`)
			v := publishActiveBy(t, db, ns, "owner@example.com", d)
			calls := 0
			e := sensitivityEngine(t, db, &calls)
			vars := map[string]any{"summary": "hello"}
			if tc.sourceRepo != "" {
				vars["repository"] = tc.sourceRepo
			}
			outcome, reason := handleKind(t, e, db, ns, v.DeclarationID, tc.trigger, vars)
			if outcome != tc.want {
				t.Fatalf("outcome %s (%s), want %s", outcome, reason, tc.want)
			}
			if tc.want == OutcomeFired && (calls != 1 || approvalCount(t, db, ns) != 0) {
				t.Fatalf("calls=%d approvals=%d", calls, approvalCount(t, db, ns))
			}
			if tc.want == OutcomeFired && !strings.Contains(reason, "destination audience: discord") {
				t.Fatalf("fired explanation omits destination audience: %s", reason)
			}
			if tc.want == OutcomeSensitivityBlocked && (!strings.Contains(reason, "discord (public audience)") || calls != 0) {
				t.Fatalf("reason=%s calls=%d", reason, calls)
			}
		})
	}
}

func TestPostgresDestinationAudienceNewestAndActorIsolation(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ns := pgtest.MustNamespace(t, db, "tca-destination-history").ID
	ctx := context.Background()
	for _, audience := range []string{"public", "org"} {
		if _, err := SetDestinationAudience(ctx, db, ns, "company/notify-discord", audience, "operator@example.com", "test"); err != nil {
			t.Fatal(err)
		}
	}
	p := PostgresBackend{Store: db}
	if got, ok, err := p.DestinationAudience(ctx, ns, "company/notify-discord"); err != nil || !ok || got != decl.AudienceOrg {
		t.Fatalf("newest=%v %v %v", got, ok, err)
	}
	if _, ok, err := p.DestinationAudience(ctx, ns, "company/other-discord"); err != nil || ok {
		t.Fatalf("other actor has record: %v %v", ok, err)
	}
	list, err := ListDestinationAudiences(ctx, db, ns)
	if err != nil || len(list) != 1 || list[0].Audience != "org" {
		t.Fatalf("list=%+v %v", list, err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE destination_audiences SET audience='public' WHERE namespace_id=$1`, ns); err == nil {
		t.Fatal("destination row was mutable")
	}
}

// d4, part 1: GitHub's audience per repository, read from the namespace's
// repository_visibility record. A public repository ranks public, a private
// one org; an unknown repository is public as a target and org as a source.
func TestPostgresGitHubAudiencePerRepository(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-exposure-repo").ID
	calls := 0
	e := sensitivityEngine(t, db, &calls)
	setVisibility(t, db, ns, "Acme/Open", "public")
	setVisibility(t, db, ns, "acme/secret", "private")
	setVisibility(t, db, ns, "acme/secret2", "public")
	setVisibility(t, db, ns, "acme/secret2", "private") // newest row wins

	d := active("reply").Declaration
	d.Trigger.Kind, d.Condition = "github.pr.created", "true"
	d.Action = decl.Action{Kind: "github.comment", With: json.RawMessage(`{"uses":"actor://gh","input":{"repository":"{target}","number":"1","comment":"{title}"}}`)}
	v := publishActiveBy(t, db, ns, "alice@example.com", d)
	for _, c := range []struct {
		source, target string
		blocked        bool
		audience       string
	}{
		{"acme/secret", "acme/secret2", false, ""},
		{"acme/secret", "ACME/OPEN", true, "github acme/open (public audience)"},
		{"acme/secret", "acme/nobody", true, "github acme/nobody (public audience)"},
		{"acme/open", "acme/nobody", false, ""},
		{"acme/nobody", "acme/secret", false, ""},
		{"acme/nobody", "acme/open", true, "github acme/nobody (org audience)"},
	} {
		outcome, reason := handleKind(t, e, db, ns, v.DeclarationID, "github.pr.created", map[string]any{"repository": c.source, "target": c.target, "title": "t"})
		if blocked := outcome == OutcomeSensitivityBlocked; blocked != c.blocked || !strings.Contains(reason, c.audience) {
			t.Errorf("%s -> %s: %q %q, want blocked=%v naming %q", c.source, c.target, outcome, reason, c.blocked, c.audience)
		}
	}
	if n := approvalCount(t, db, ns); n != 0 {
		t.Fatalf("unlisted github widenings opened %d tasks", n)
	}

	cur, err := ListRepositoryVisibility(ctx, db, ns)
	if err != nil || len(cur) != 3 {
		t.Fatalf("current visibility = %+v, %v", cur, err)
	}
	for _, r := range cur {
		if r.Repository == "acme/secret2" && r.Visibility != "private" {
			t.Fatalf("newest row did not win: %+v", r)
		}
	}
	for _, bad := range [][2]string{{"not-a-repo", "public"}, {"a/b", "internal"}, {"", "private"}} {
		if _, err := SetRepositoryVisibility(ctx, db, ns, bad[0], bad[1], "op", ""); !errors.Is(err, ErrRepositoryVisibilityInvalid) {
			t.Errorf("SetRepositoryVisibility(%q, %q) err = %v", bad[0], bad[1], err)
		}
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE repository_visibility SET visibility='public' WHERE namespace_id=$1`, ns); err == nil {
		t.Fatal("a repository visibility row was rewritten in place")
	}
}

// d4, part 2 end to end: a LISTED widening blocks while pending and opens
// exactly one task across versions; only the owner, as a human, decides; a
// refusal keeps blocking; an approval fires, and survives a republish;
// decisions are append-only; removing the entry withdraws the approval and
// relisting it needs the owner again.
func TestPostgresExposureListedApprovalLifecycle(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-exposure").ID
	calls := 0
	e := sensitivityEngine(t, db, &calls)
	v1 := publishActiveBy(t, db, ns, "alice@example.com", announceDeclaration("New issue: {summary}", "summary"))
	declID := v1.DeclarationID
	vars := map[string]any{"summary": "customer X is churning"}

	for i := 0; i < 2; i++ {
		outcome, reason := handleJira(t, e, db, ns, declID, vars)
		if outcome != OutcomeSensitivityBlocked {
			t.Fatalf("event %d: outcome %q (%s), want %q", i, outcome, reason, OutcomeSensitivityBlocked)
		}
		for _, want := range []string{"{summary}", "jira (team audience)", "discord (public audience)", "alice@example.com", "pending"} {
			if !strings.Contains(reason, want) {
				t.Errorf("blocked reason %q does not name %s", reason, want)
			}
		}
	}
	// A second version of the same declaration asks the same question.
	publishActiveBy(t, db, ns, "alice@example.com", announceDeclaration("Filed: {summary}", "summary"))
	if outcome, _ := handleJira(t, e, db, ns, declID, vars); outcome != OutcomeSensitivityBlocked {
		t.Fatalf("v2 pending: outcome %q", outcome)
	}
	if calls != 0 {
		t.Fatalf("a blocked widening dispatched %d times", calls)
	}
	if n := approvalCount(t, db, ns); n != 1 {
		t.Fatalf("three blocked events over two versions opened %d approval tasks, want exactly 1", n)
	}
	inbox, err := ListSensitivityApprovals(ctx, db, ns, "alice@example.com", SensitivityPending)
	if err != nil || len(inbox) != 1 {
		t.Fatalf("owner inbox = %+v, %v", inbox, err)
	}
	task := inbox[0]
	if task.DeclarationName != "announce" || task.Variable != "summary" || task.SourceSystem != "jira" || task.TargetSystem != "discord" || task.DeclarationVersionID != v1.ID {
		t.Fatalf("approval task = %+v", task)
	}

	// Only a human, and only the owner.
	if _, err := DecideSensitivityApproval(ctx, db, ns, task.ID, ActivationPrincipal{Kind: PrincipalAgent, Author: "alice@example.com"}, SensitivityApproved, ""); !errors.Is(err, ErrSensitivityNotHuman) {
		t.Fatalf("agent approval err = %v, want ErrSensitivityNotHuman", err)
	}
	if _, err := DecideSensitivityApproval(ctx, db, ns, task.ID, ActivationPrincipal{Kind: PrincipalHuman, Author: "bob@example.com"}, SensitivityApproved, ""); !errors.Is(err, ErrSensitivityNotOwner) {
		t.Fatalf("non-owner approval err = %v, want ErrSensitivityNotOwner", err)
	}
	if ds, _ := ListSensitivityDecisions(ctx, db, ns, task.ID); len(ds) != 0 {
		t.Fatalf("refused deciders still recorded decisions: %+v", ds)
	}

	owner := ActivationPrincipal{Kind: PrincipalHuman, Author: "alice@example.com"}
	refusal, err := DecideSensitivityApproval(ctx, db, ns, task.ID, owner, SensitivityRefused, "not for the public channel")
	if err != nil {
		t.Fatal(err)
	}
	if outcome, reason := handleJira(t, e, db, ns, declID, vars); outcome != OutcomeSensitivityBlocked || !strings.Contains(reason, "refused") {
		t.Fatalf("after refusal: %q %q, want still blocked and naming the refusal", outcome, reason)
	}
	// t40b: a blind second decision on the refused task is refused and
	// appends nothing; changing the answer is a correction naming the head,
	// and a correction naming anything else is stale.
	if _, err := DecideSensitivityApproval(ctx, db, ns, task.ID, owner, SensitivityApproved, "again"); !errors.Is(err, ErrSensitivityAlreadyDecided) {
		t.Fatalf("second decision on a refused task err = %v, want ErrSensitivityAlreadyDecided", err)
	}
	if _, err := CorrectSensitivityApproval(ctx, db, ns, task.ID, owner, SensitivityApproved, "stale", "01NOTTHEHEAD"); !errors.Is(err, ErrSensitivityStaleCorrection) {
		t.Fatalf("stale correction err = %v, want ErrSensitivityStaleCorrection", err)
	}
	if _, err := CorrectSensitivityApproval(ctx, db, ns, task.ID, ActivationPrincipal{Kind: PrincipalAgent, Author: "alice@example.com"}, SensitivityApproved, "", refusal.ID); !errors.Is(err, ErrSensitivityNotHuman) {
		t.Fatalf("agent correction err = %v, want ErrSensitivityNotHuman", err)
	}
	if ds, _ := ListSensitivityDecisions(ctx, db, ns, task.ID); len(ds) != 1 {
		t.Fatalf("refused repeats appended decisions: %+v", ds)
	}
	approval, err := CorrectSensitivityApproval(ctx, db, ns, task.ID, owner, SensitivityApproved, "fine after all", refusal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecideSensitivityApproval(ctx, db, ns, task.ID, owner, SensitivityApproved, "double click"); !errors.Is(err, ErrSensitivityAlreadyDecided) {
		t.Fatalf("second decision on an approved task err = %v, want ErrSensitivityAlreadyDecided", err)
	}
	if approval.SupersedesID != refusal.ID {
		t.Fatalf("approval supersedes %q, want the refusal %q", approval.SupersedesID, refusal.ID)
	}
	if outcome, reason := handleJira(t, e, db, ns, declID, vars); outcome != OutcomeFired || calls != 1 {
		t.Fatalf("after approval: %q %q calls=%d, want fired once", outcome, reason, calls)
	}

	// Republishing keeps the approved entry: per name, not per version.
	publishActiveBy(t, db, ns, "alice@example.com", announceDeclaration("Issue filed: {summary}", "summary"))
	if outcome, reason := handleJira(t, e, db, ns, declID, vars); outcome != OutcomeFired || calls != 2 {
		t.Fatalf("republished: %q %q calls=%d, want fired on the standing approval", outcome, reason, calls)
	}
	if n := approvalCount(t, db, ns); n != 1 {
		t.Fatalf("republish opened more tasks: %d", n)
	}

	// Append-only: both decisions remain, and neither can be rewritten.
	ds, err := ListSensitivityDecisions(ctx, db, ns, task.ID)
	if err != nil || len(ds) != 2 || ds[0].Decision != SensitivityRefused || ds[1].Decision != SensitivityApproved {
		t.Fatalf("decision history = %+v, %v", ds, err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE declaration_exposure_decisions SET decision='approved' WHERE id=$1`, refusal.ID); err == nil {
		t.Fatal("an exposure decision was rewritten in place")
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM declaration_exposure_approvals WHERE id=$1`, task.ID); err == nil {
		t.Fatal("an exposure approval task was deleted")
	}

	// Removing the entry withdraws it: blocked as unlisted, no new task.
	publishActiveBy(t, db, ns, "alice@example.com", announceDeclaration("Dropped: {summary}"))
	if outcome, reason := handleJira(t, e, db, ns, declID, vars); outcome != OutcomeSensitivityBlocked || !strings.Contains(reason, "not in exposes") {
		t.Fatalf("entry removed: %q %q, want blocked as unlisted", outcome, reason)
	}
	if got, _ := GetSensitivityApproval(ctx, db, ns, task.ID); got.Status != SensitivityWithdrawn {
		t.Fatalf("after removal the approval is %q, want withdrawn", got.Status)
	}
	// Relisting it does not revive the old approval; the owner decides again.
	publishActiveBy(t, db, ns, "alice@example.com", announceDeclaration("Back: {summary}", "summary"))
	if outcome, reason := handleJira(t, e, db, ns, declID, vars); outcome != OutcomeSensitivityBlocked || !strings.Contains(reason, SensitivityWithdrawn) {
		t.Fatalf("relisted: %q %q, want blocked naming the withdrawn approval", outcome, reason)
	}
	again, err := DecideSensitivityApproval(ctx, db, ns, task.ID, owner, SensitivityApproved, "again")
	if err != nil || again.SupersedesID != approval.ID {
		t.Fatalf("re-approval = %+v, %v", again, err)
	}
	if outcome, reason := handleJira(t, e, db, ns, declID, vars); outcome != OutcomeFired {
		t.Fatalf("re-approved: %q %q, want fired", outcome, reason)
	}
	if n := approvalCount(t, db, ns); n != 1 {
		t.Fatalf("withdraw/relist opened more tasks: %d", n)
	}
}

// A different author publishing the producing declaration is a different
// owner: the old approval no longer applies, and a new task is opened for
// the new owner.
func TestPostgresExposureOwnerChangeNeedsNewApproval(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-exposure-owner").ID
	calls := 0
	e := sensitivityEngine(t, db, &calls)
	v := publishActiveBy(t, db, ns, "alice@example.com", announceDeclaration("{summary}", "summary"))
	vars := map[string]any{"summary": "s"}
	if outcome, _ := handleJira(t, e, db, ns, v.DeclarationID, vars); outcome != OutcomeSensitivityBlocked {
		t.Fatalf("pending: %q", outcome)
	}
	alice, _ := ListSensitivityApprovals(ctx, db, ns, "alice@example.com", "")
	if len(alice) != 1 {
		t.Fatalf("alice's inbox = %+v", alice)
	}
	if _, err := DecideSensitivityApproval(ctx, db, ns, alice[0].ID, ActivationPrincipal{Kind: PrincipalHuman, Author: "alice@example.com"}, SensitivityApproved, ""); err != nil {
		t.Fatal(err)
	}
	if outcome, _ := handleJira(t, e, db, ns, v.DeclarationID, vars); outcome != OutcomeFired {
		t.Fatalf("approved: %q", outcome)
	}
	// Bob publishes the producing declaration (here the step-0 source is the
	// declaration itself): the variable now belongs to bob.
	publishActiveBy(t, db, ns, "bob@example.com", announceDeclaration("{summary}!", "summary"))
	outcome, reason := handleJira(t, e, db, ns, v.DeclarationID, vars)
	if outcome != OutcomeSensitivityBlocked || !strings.Contains(reason, "bob@example.com") {
		t.Fatalf("new owner: %q %q, want blocked awaiting bob", outcome, reason)
	}
	bob, _ := ListSensitivityApprovals(ctx, db, ns, "bob@example.com", SensitivityPending)
	if len(bob) != 1 || bob[0].Variable != "summary" || approvalCount(t, db, ns) != 2 {
		t.Fatalf("bob's inbox = %+v (tasks %d), want one new task", bob, approvalCount(t, db, ns))
	}
	// Alice's standing approval cannot be spent on bob's variable.
	if _, err := DecideSensitivityApproval(ctx, db, ns, bob[0].ID, ActivationPrincipal{Kind: PrincipalHuman, Author: "alice@example.com"}, SensitivityApproved, ""); !errors.Is(err, ErrSensitivityNotOwner) {
		t.Fatalf("old owner deciding the new task err = %v", err)
	}
}

// An unlisted widening blocks without opening a task, and publish warns
// with each entry's state: unlisted, listed without a task, pending,
// approved, refused.
func TestPostgresExposureUnlistedAndPublishWarnings(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-exposure-warn").ID
	calls := 0
	e := sensitivityEngine(t, db, &calls)
	intake := active("jira-intake").Declaration
	intake.Trigger.Kind, intake.Condition = "jira.issue.created", "true"
	intake.Action = decl.Action{Kind: "jira.comment", With: json.RawMessage(`{"uses":"actor://jira","input":{}}`)}
	publishActiveBy(t, db, ns, "carol@example.com", intake)

	unlisted := announceDeclaration("{summary}")
	v := publishActiveBy(t, db, ns, "alice@example.com", unlisted)
	outcome, reason := handleJira(t, e, db, ns, v.DeclarationID, map[string]any{"summary": "s"})
	if outcome != OutcomeSensitivityBlocked || !strings.Contains(reason, `add "summary" to exposes`) {
		t.Fatalf("unlisted: %q %q", outcome, reason)
	}
	if n := approvalCount(t, db, ns); n != 0 {
		t.Fatalf("an unlisted widening opened %d tasks", n)
	}

	d := announceDeclaration("{jira-intake:owner} {jira-intake:owner} {summary} {reporter} {1:x:y} {extra}", "jira-intake:owner", "summary", "reporter", "1:x")
	byEntry := func() map[string]string {
		t.Helper()
		ws, err := SensitivityWarnings(ctx, db, ns, d, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, w := range ws {
			if !strings.HasPrefix(w, "sensitivity: ") {
				t.Errorf("warning %q lacks the sensitivity prefix", w)
			}
			for _, e := range []string{"jira-intake:owner", "summary", "reporter", "1:x", "extra"} {
				if strings.Contains(w, "(exposes entry \""+e+"\")") {
					out[e] = w
				}
			}
		}
		if len(ws) != 5 || len(out) != 5 {
			t.Fatalf("warnings = %q, want one per widening reference", ws)
		}
		return out
	}
	ws := byEntry()
	for entry, want := range map[string]string{
		"extra":             `exposure unlisted: firing is blocked; add "extra" to exposes`,
		"summary":           `no approval task yet; the first blocked firing opens one for owner "alice@example.com"`,
		"jira-intake:owner": `no approval task yet; the first blocked firing opens one for owner "carol@example.com"`,
		"1:x":               "owner known only at firing time",
	} {
		if !strings.Contains(ws[entry], want) {
			t.Errorf("warning for %s = %q, want %q", entry, ws[entry], want)
		}
	}

	// Open tasks the way a blocked firing would, decide two, and re-read.
	backend := PostgresBackend{Store: db}
	open := func(variable, owner string) SensitivityApproval {
		a, err := backend.RequestSensitivityApproval(ctx, SensitivityApprovalRequest{NamespaceID: ns, DeclarationName: "announce", Variable: variable, Owner: owner,
			DeclarationID: v.DeclarationID, DeclarationVersionID: v.ID, SourceDeclarationID: v.DeclarationID, SourceVersionID: v.ID, EventID: "evt",
			Source: decl.SourceSensitivity("jira.issue.created"), Target: decl.TargetSensitivity("discord.post")})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	summary, owner, reporter := open("summary", "alice@example.com"), open("jira-intake:owner", "carol@example.com"), open("reporter", "alice@example.com")
	if _, err := DecideSensitivityApproval(ctx, db, ns, summary.ID, ActivationPrincipal{Kind: PrincipalHuman, Author: "alice@example.com"}, SensitivityApproved, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := DecideSensitivityApproval(ctx, db, ns, reporter.ID, ActivationPrincipal{Kind: PrincipalHuman, Author: "alice@example.com"}, SensitivityRefused, ""); err != nil {
		t.Fatal(err)
	}
	ws = byEntry()
	for entry, want := range map[string]string{
		"summary":           "exposure listed, approved (owner \"alice@example.com\", approval " + summary.ID,
		"jira-intake:owner": "exposure listed, pending (owner \"carol@example.com\", approval " + owner.ID,
		"reporter":          "exposure listed, refused",
	} {
		if !strings.Contains(ws[entry], want) {
			t.Errorf("warning for %s = %q, want %q", entry, ws[entry], want)
		}
	}

	comment := d
	comment.Action.Kind = "jira.comment"
	comment.Action.With = json.RawMessage(`{"body":"{jira-intake:owner} {summary}"}`)
	if ws, err := SensitivityWarnings(ctx, db, ns, comment, nil); err != nil || len(ws) != 0 {
		t.Fatalf("same-audience references warned: %q %v", ws, err)
	}
}

// A reference with no value renders its default or the placeholder, which
// exposes nothing: no block and no task. A narrowing or same-audience render
// is never blocked either.
func TestPostgresSensitivityOnlyPresentWideningValuesBlock(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ns := pgtest.MustNamespace(t, db, "tca-sensitivity-absent").ID
	calls := 0
	e := sensitivityEngine(t, db, &calls)
	v := publishActiveBy(t, db, ns, "alice@example.com", announceDeclaration("New issue: {summary:(none)}"))
	if outcome, reason := handleJira(t, e, db, ns, v.DeclarationID, map[string]any{"other": "x"}); outcome != OutcomeFired {
		t.Fatalf("absent variable: %q %q, want fired", outcome, reason)
	}

	same := active("comment-back").Declaration
	same.Trigger.Kind, same.Condition = "jira.issue.created", "true"
	same.Action = decl.Action{Kind: "jira.comment", With: json.RawMessage(`{"uses":"actor://jira","input":{"body":"{summary}"}}`)}
	ns2 := pgtest.MustNamespace(t, db, "tca-sensitivity-same").ID
	v2 := publishActiveBy(t, db, ns2, "alice@example.com", same)
	if outcome, reason := handleJira(t, e, db, ns2, v2.DeclarationID, map[string]any{"summary": "s"}); outcome != OutcomeFired {
		t.Fatalf("same-audience render: %q %q, want fired", outcome, reason)
	}
	if approvalCount(t, db, ns)+approvalCount(t, db, ns2) != 0 {
		t.Fatal("a non-widening render opened an approval task")
	}
}

// 0068 retires t30's per-version tables: they keep any history but refuse
// new rows, so nothing can keep writing the old model.
func TestPostgresRetiredPerVersionApprovalsRefuseRows(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ns := pgtest.MustNamespace(t, db, "tca-exposure-retired").ID
	v := publishActiveBy(t, db, ns, "alice@example.com", announceDeclaration("{summary}"))
	_, err := db.Pool().Exec(context.Background(), `INSERT INTO declaration_sensitivity_approvals(id,namespace_id,declaration_id,declaration_version,source_declaration_id,source_version,variable,source_system,source_audience,target_system,target_audience,owner,first_event_id)
 VALUES('x',$1,$2,$3,$2,$3,'summary','jira','team','discord','public','alice@example.com','e')`, ns, v.DeclarationID, v.ID)
	if err == nil || !strings.Contains(err.Error(), "retired by 0068") {
		t.Fatalf("insert into the retired table: err = %v", err)
	}
}
