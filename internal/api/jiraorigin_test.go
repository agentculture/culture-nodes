package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	t38fFiring   = "01K6TCA38F0000000000000001"
	commentMark  = "cn1:" + t38fFiring + ":jira.comment:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + ":" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	issueMark    = "cn1:" + t38fFiring + ":jira.issue:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + ":" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	markedIssue  = "SCRUM-43"
	markedSite   = "team.example.com"
	markedPrefix = "jira:" + markedSite + ":" + markedIssue + ":"
)

func markedFacts(t *testing.T, bot string) ([]jiraFact, int) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "jira_issue_marked.json"))
	if err != nil {
		t.Fatal(err)
	}
	var issue map[string]any
	if err := json.Unmarshal(body, &issue); err != nil {
		t.Fatal(err)
	}
	return jiraEmissionsReport(issue, markedSite, "SCRUM", bot)
}

func factPayload(t *testing.T, f jiraFact) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(f.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func bySourceKey(facts []jiraFact) map[string]jiraFact {
	out := map[string]jiraFact{}
	for _, f := range facts {
		out[f.SourceKey] = f
	}
	return out
}

func origin(marker, kind, id, author, bot string) map[string]any {
	return map[string]any{"marker": marker, "artifact_kind": kind, "artifact_id": id, "author": author, "bridge_account": bot}
}

// A bot comment carrying a marker raises the neutral jira.comment with the
// origin the engine verifies; the legacy fact for it stays self-echo; a
// person's comment carrying a copied marker names that person as author.
func TestJiraEmissionsCarryStampedOrigin(t *testing.T) {
	facts, withheld := markedFacts(t, "bot")
	if withheld != 0 {
		t.Fatalf("withheld = %d with a configured bot, want 0", withheld)
	}
	keys := bySourceKey(facts)
	bot, ok := keys[markedPrefix+"comment:22"]
	if !ok || bot.Name != "jira.comment" {
		t.Fatalf("no neutral jira.comment for the bot's marked comment: %+v", keys)
	}
	if got := factPayload(t, bot)["origin"]; !reflect.DeepEqual(got, origin(commentMark, "jira.comment", "22", "bot", "bot")) {
		t.Fatalf("bot comment origin = %v", got)
	}
	if string(bot.Watermark) != `{"comment_id":"22"}` || bot.Subject != markedIssue {
		t.Fatalf("bot comment watermark %s subject %s", bot.Watermark, bot.Subject)
	}
	if _, legacy := keys[markedPrefix+"history:comment:22"]; legacy {
		t.Fatal("the legacy fact for the bot's own comment is no longer self-echo")
	}
	for _, id := range []string{"23"} {
		for key := range keys {
			if strings.HasSuffix(key, ":"+id) {
				t.Fatalf("the bot's unmarked comment %s raised %s", id, key)
			}
		}
	}
	human := keys[markedPrefix+"comment:25"]
	if got := factPayload(t, human)["origin"]; !reflect.DeepEqual(got, origin(commentMark, "jira.comment", "25", "human", "bot")) {
		t.Fatalf("copied-marker origin = %v, want the human as author (the last valid marker)", got)
	}
	if _, legacy := keys[markedPrefix+"history:comment:25"]; !legacy {
		t.Fatal("the human's comment lost its legacy fact")
	}
	if p := factPayload(t, keys[markedPrefix+"comment:21"]); p["origin"] != nil || p["body"] != "please pick this up" {
		t.Fatalf("unmarked human comment payload = %v", p)
	}
	if p := factPayload(t, keys[markedPrefix+"comment:26"]); p["origin"] != nil {
		t.Fatalf("an invalid marker produced an origin: %v", p)
	}
	transition, ok := keys[markedPrefix+"transitioned:In Progress:comment:24"]
	if !ok || transition.Name != "jira.issue.transitioned" {
		t.Fatalf("no neutral transition for the transition's marker comment: %+v", keys)
	}
	want := map[string]any{"source": "jira", "issue": markedIssue, "from_status": "To Do", "to_status": "In Progress", "site": markedSite,
		"origin": origin(issueMark, "jira.issue", "24", "bot", "bot")}
	if got := factPayload(t, transition); !reflect.DeepEqual(got, want) {
		t.Fatalf("transition reaction = %v, want %v", got, want)
	}
	created := keys[markedPrefix+"created"]
	if got := factPayload(t, created)["origin"]; !reflect.DeepEqual(got, origin(issueMark, "jira.issue", "10042", "bot", "bot")) {
		t.Fatalf("created origin = %v", got)
	}
}

// No configured bot account: no origin anywhere (fail closed), counted so
// the handler logs it.
func TestJiraEmissionsWithoutBotWithholdOrigin(t *testing.T) {
	facts, withheld := markedFacts(t, "")
	if withheld == 0 {
		t.Fatal("no withheld origin reported without a bot account")
	}
	for _, f := range facts {
		if factPayload(t, f)["origin"] != nil {
			t.Fatalf("%s %s carries an origin without a configured bot", f.Name, f.SourceKey)
		}
	}
}

func TestLastCN1MarkerMatchesStampingPattern(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "adapters", "jira", "src", "jira_bridge", "stamping.py"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `_FIELD = r"[^:]{1,256}"`) || !strings.Contains(string(raw), `rf"cn1:{_FIELD}:{_FIELD}:[0-9a-fA-F]{{48}}:[0-9a-fA-F]{{64}}\Z"`) {
		t.Fatal("stamping.py's _MARKER changed; update cn1Marker in jiraorigin.go and pr_upkeep_jira.py")
	}
	if got := lastCN1Marker("x " + commentMark + "\n" + issueMark + "\ncn1:a:b:zz:zz"); got != issueMark {
		t.Fatalf("lastCN1Marker = %q", got)
	}
}
