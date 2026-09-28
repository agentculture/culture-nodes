package decl

// Task t30b (#328, owner decision d4): GitHub's audience per repository, and
// the per-variable `exposes` list a declaration carries.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl/template"
)

func referenceOf(step, name string) template.Reference {
	return template.Reference{Step: step, Name: name}
}

func visibilities(m map[string]Visibility) RepositoryVisibility {
	return func(repo string) Visibility { return m[repo] }
}

// A GitHub target ranks public for a public repository, org for a private
// one, and public -- the widest, failing closed -- for one nobody recorded.
// As a source the unknown case fails closed the other way: org, the
// narrower rank, so an unrecorded private repository's data is protected.
func TestGitHubAudiencePerRepository(t *testing.T) {
	vis := visibilities(map[string]Visibility{"acme/open": VisibilityPublic, "acme/secret": VisibilityPrivate})
	for _, c := range []struct {
		repo           string
		target, source Audience
	}{
		{"acme/open", AudiencePublic, AudiencePublic},
		{"acme/secret", AudienceOrg, AudienceOrg},
		{"acme/unknown", AudiencePublic, AudienceOrg},
		{"", AudiencePublic, AudienceOrg},
	} {
		if got := TargetSensitivityIn("github.comment", c.repo, vis); got.Audience != c.target || got.System != SystemGitHub {
			t.Errorf("target %q = %v, want %v", c.repo, got, c.target)
		}
		if got := SourceSensitivityIn(c.repo, vis, "github.pr.created"); got.Audience != c.source || got.System != SystemGitHub {
			t.Errorf("source %q = %v, want %v", c.repo, got, c.source)
		}
	}
	// A nil lookup knows nothing: every repository is unknown.
	if got := TargetSensitivityIn("github.review_reply", "acme/open", nil); got.Audience != AudiencePublic {
		t.Errorf("nil lookup target = %v, want public", got)
	}
	// Non-GitHub kinds ignore the repository entirely.
	if got := TargetSensitivityIn("jira.comment", "acme/secret", vis); got.Audience != AudienceTeam {
		t.Errorf("jira target = %v, want team", got)
	}
	// The repository names itself in the rendered mark.
	if s := TargetSensitivityIn("github.comment", "acme/secret", vis).String(); !strings.Contains(s, "acme/secret") || !strings.Contains(s, "org audience") {
		t.Errorf("mark text %q must name the repository and its audience", s)
	}

	cases := []struct {
		name               string
		sourceRepo, target string
		widens             bool
	}{
		{"public source never widens", "acme/open", "acme/unknown", false},
		{"private into private stays in the org", "acme/secret", "acme/secret", false},
		{"private into public widens", "acme/secret", "acme/open", true},
		{"private into unknown widens (unknown is public)", "acme/secret", "acme/unknown", true},
		{"unknown source into private does not widen", "acme/unknown", "acme/secret", false},
		{"unknown source into public widens", "acme/unknown", "acme/open", true},
	}
	for _, c := range cases {
		got := Widens(SourceSensitivityIn(c.sourceRepo, vis, "github.pr.created"), TargetSensitivityIn("github.comment", c.target, vis))
		if got != c.widens {
			t.Errorf("%s: widens=%v, want %v", c.name, got, c.widens)
		}
	}
	// A public repository's data is public already: even Discord is no wider.
	if Widens(SourceSensitivityIn("acme/open", vis, "github.pr.approved"), TargetSensitivity("discord.post")) {
		t.Error("public repository data into discord counted as a widening")
	}
}

// The repository an action targets is its rendered input.repository -- the
// field the github bridge reads (adapters/github/src/github_bridge/mapping.py).
// A template that is still unrendered names no repository.
func TestActionRepository(t *testing.T) {
	for with, want := range map[string]string{
		`{"uses":"actor://gh","input":{"repository":"AgentCulture/Culture-Nodes","number":"1"}}`: "agentculture/culture-nodes",
		`{"uses":"actor://gh","input":{"repository":"{0:repository}"}}`:                          "",
		`{"uses":"actor://gh","input":{"repository":7}}`:                                         "",
		`{"uses":"actor://gh","input":{}}`:                                                       "",
		`{"uses":"actor://gh"}`:                                                                  "",
	} {
		if got := ActionRepository(Action{Kind: "github.comment", With: json.RawMessage(with)}); got != want {
			t.Errorf("ActionRepository(%s) = %q, want %q", with, got, want)
		}
	}
}

func TestExposesNormalizeAndValidate(t *testing.T) {
	src := `{"name":"n","trigger":{"kind":"timer"},"action":{"kind":"agent.work"},
 "start_node":{"name":"a","deadline":"none"},"landing_node":{"name":"b","deadline":"none"},
 "exposes":["0:summary","jira-intake:reporter","summary","1:x"]}`
	d, err := Parse([]byte(src), FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(d.Exposes, ","); got != "1:x,jira-intake:reporter,summary" {
		t.Fatalf("exposes = %q, want sorted, deduplicated, step 0 written bare", got)
	}
	body, err := d.CanonicalJSON()
	if err != nil || !strings.Contains(string(body), `"exposes":["1:x","jira-intake:reporter","summary"]`) {
		t.Fatalf("canonical body %s (%v) must carry the normalized list", body, err)
	}
	// Normalization is part of the canonical form whatever the entry path.
	raw := *d
	raw.Exposes = []string{"summary", "0:summary", "1:x", "jira-intake:reporter"}
	if again, _ := raw.CanonicalJSON(); string(again) != string(body) {
		t.Fatalf("canonical form depends on authored order: %s vs %s", again, body)
	}
	// No list, no key: pre-t30b digests are unchanged.
	d.Exposes = nil
	if b, _ := d.CanonicalJSON(); strings.Contains(string(b), "exposes") {
		t.Fatalf("an empty list reached the canonical body: %s", b)
	}
	for _, bad := range []string{`""`, `"a:b:c"`, `":x"`, `"x:"`, `"{x}"`, `"Bad Step:x"`} {
		s := strings.Replace(src, `"0:summary","jira-intake:reporter","summary","1:x"`, bad, 1)
		if _, err := Parse([]byte(s), FormatJSON); err == nil {
			t.Errorf("exposes entry %s was accepted", bad)
		}
	}
}

func TestExposureEntryAndListing(t *testing.T) {
	d := Declaration{Exposes: []string{"jira-intake:reporter", "summary"}}
	for _, c := range []struct {
		step, name, entry string
		listed            bool
	}{
		{"0", "summary", "summary", true},
		{"jira-intake", "reporter", "jira-intake:reporter", true},
		{"1", "reporter", "1:reporter", false},
		{"0", "owner", "owner", false},
	} {
		ref := referenceOf(c.step, c.name)
		if got := ExposureEntry(ref); got != c.entry {
			t.Errorf("entry(%s:%s) = %q, want %q", c.step, c.name, got, c.entry)
		}
		if got := d.Lists(ExposureEntry(ref)); got != c.listed {
			t.Errorf("Lists(%q) = %v, want %v", c.entry, got, c.listed)
		}
	}
}

// Publish analysis marks each widening listed or not, and names the entry
// the author would add.
func TestWideningReferencesCarryEntryAndListing(t *testing.T) {
	d := Declaration{Name: "announce", Trigger: Trigger{Kind: "jira.issue.created"}, Exposes: []string{"summary"},
		Action: Action{Kind: "discord.post", With: json.RawMessage(`{"input":{"text":"{summary} {owner}"}}`)}}
	ws, err := WideningReferences(d, nil, nil)
	if err != nil || len(ws) != 2 {
		t.Fatalf("widenings = %+v, %v", ws, err)
	}
	for _, w := range ws {
		if want := w.Reference.Name == "summary"; w.Listed != want || w.Entry != w.Reference.Name {
			t.Errorf("%+v: listed=%v entry=%q", w, w.Listed, w.Entry)
		}
	}
	if msg := ws[1].Warning("unlisted"); !strings.Contains(msg, `add "owner" to exposes`) {
		t.Errorf("unlisted warning %q must tell the author what to add", msg)
	}
}
