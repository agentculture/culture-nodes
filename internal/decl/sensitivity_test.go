package decl

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl/kinds"
)

// Every registered kind has a row in the one ranking table: a new kind
// cannot land and silently fall into the fail-closed default.
func TestSensitivityTableCoversEveryKind(t *testing.T) {
	registered := map[string]bool{}
	for _, k := range append(kinds.Actions(), kinds.Triggers()...) {
		registered[k.Name] = true
		s, ok := KindSystem(k.Name)
		if !ok {
			t.Errorf("kind %q has no sensitivity system", k.Name)
			continue
		}
		if _, ok := systemAudience[s]; !ok || s == SystemUnknown {
			t.Errorf("kind %q maps to unranked system %q", k.Name, s)
		}
	}
	for name := range kindSystem {
		if !registered[name] {
			t.Errorf("sensitivity table names unregistered kind %q", name)
		}
	}
}

func TestAudienceRanking(t *testing.T) {
	cases := []struct {
		source, target string
		widens         bool
	}{
		{"jira.issue.created", "discord.post", true},  // the spec's example
		{"jira.comment", "github.comment", true},      // team -> org
		{"github.pr.created", "jira.comment", false},  // org -> team narrows
		{"jira.issue.created", "jira.comment", false}, // same system
		{"jira.issue.created", "agent.work", false},   // team -> team
		{"human.decision", "agent.work", true},        // a person's answer -> team
		{"code.result", "agent.work", true},           // a code result (operators) -> team
		{"code.result", "code.run", false},            // same system
		{"github.pr.approved", "discord.post", true},  // org -> public
		{"timer", "discord.post", true},               // engine records -> public
		{"no.such.trigger", "human.ask", true},        // unknown source is restricted
		{"human.decision", "no.such.action", true},    // unknown target is public
		// t30b (d4): an unknown repository ranks org as a source and public
		// as a target, so GitHub into GitHub widens until visibility is known.
		{"github.pr.created", "github.review_reply", true},
	}
	for _, c := range cases {
		got := Widens(SourceSensitivity(c.source), TargetSensitivity(c.target))
		if got != c.widens {
			t.Errorf("%s -> %s widens=%v, want %v", c.source, c.target, got, c.widens)
		}
	}
	if s := SourceSensitivity("github.pr.created", "jira.comment"); s.System != SystemJira {
		t.Errorf("mixed source mark = %v, want the narrower jira", s)
	}
	if s := SourceSensitivity(); s.Audience != AudienceRestricted {
		t.Errorf("no source kind = %v, want restricted", s)
	}
	if s := SourceSensitivity("jira.comment", "bogus"); s.Audience != AudienceRestricted {
		t.Errorf("an unknown source kind among known ones = %v, want restricted", s)
	}
}

func TestWideningReferencesWarnsOncePerWideningReference(t *testing.T) {
	jira := Declaration{Name: "jira-intake", Trigger: Trigger{Kind: "jira.issue.created"}, Action: Action{Kind: "jira.comment"}}
	gh := Declaration{Name: "gh", Trigger: Trigger{Kind: "github.pr.created"}, Action: Action{Kind: "github.comment"}}
	resolve := func(name string) (Declaration, bool) {
		switch name {
		case "jira-intake":
			return jira, true
		case "gh":
			return gh, true
		}
		return Declaration{}, false
	}
	with, _ := json.Marshal(map[string]any{"text": "{jira-intake:owner} {jira-intake:owner} {gh:author} {1:x:dflt} {ghost:y}", "tags": []any{"{summary}"}})
	d := Declaration{Name: "announce", Trigger: Trigger{Kind: "jira.issue.created"}, Action: Action{Kind: "discord.post", With: with}}
	ws, err := WideningReferences(d, resolve, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Widening{}
	for _, w := range ws {
		got[w.Reference.Step+":"+w.Reference.Name] = w
	}
	if len(ws) != 5 || len(got) != 5 {
		t.Fatalf("widenings = %+v, want one each for jira-intake:owner, gh:author, 1:x, ghost:y, 0:summary", ws)
	}
	if w := got["jira-intake:owner"]; !w.SourceKnown || w.Source.System != SystemJira || w.Target.System != SystemDiscord {
		t.Errorf("jira-intake:owner = %+v", w)
	}
	if w := got["0:summary"]; !w.SourceKnown || w.Source.System != SystemJira {
		t.Errorf("step 0 marks the trigger's system: %+v", w)
	}
	for _, k := range []string{"1:x", "ghost:y"} {
		if w := got[k]; w.SourceKnown || !strings.Contains(w.Warning("listed"), "known only at firing time") {
			t.Errorf("%s should be an unresolved-source warning: %+v %q", k, w, w.Warning("listed"))
		}
	}
	if w := got["jira-intake:owner"].Warning("listed"); !strings.Contains(w, "jira (team audience)") || !strings.Contains(w, "discord (public audience)") {
		t.Errorf("warning text %q must name both systems and audiences", w)
	}

	// Narrowing or same-audience references never warn.
	narrow := Declaration{Name: "back", Trigger: Trigger{Kind: "github.pr.created"}, Action: Action{Kind: "jira.comment", With: json.RawMessage(`{"body":"{title} {gh:author}"}`)}}
	if ws, err := WideningReferences(narrow, resolve, nil); err != nil || len(ws) != 0 {
		t.Fatalf("narrowing references warned: %+v %v", ws, err)
	}
}
