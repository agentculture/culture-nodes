package decl_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
)

// withStartFrom inserts a start_from value into the minimal fixture.
func withStartFrom(t *testing.T, value string) string {
	t.Helper()
	base := string(fixture(t, "minimal.json"))
	return strings.Replace(base, `"start_node":{"name":"root","deadline":"none"},`, `"start_node":{"name":"root","deadline":"none"},"start_from":`+value+`,`, 1)
}

// Task t38d (d6): start_from is part of the canonical declaration, so the
// digest changes when it is added or changed, and a declaration without it
// keeps its existing digest (the golden test above pins that half).
func TestStartFromChangesTheDigest(t *testing.T) {
	digests := map[string]string{}
	for name, source := range map[string]string{
		"absent":      string(fixture(t, "minimal.json")),
		"any":         withStartFrom(t, `"any"`),
		"host":        withStartFrom(t, `{"host":"thor"}`),
		"other host":  withStartFrom(t, `{"host":"orin"}`),
		"actor kind":  withStartFrom(t, `{"actor_kind":"codex"}`),
		"both":        withStartFrom(t, `{"host":"thor","actor_kind":"codex"}`),
		"both, other": withStartFrom(t, `{"host":"thor","actor_kind":"claude"}`),
	} {
		d, err := decl.Parse([]byte(source), decl.FormatJSON)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		digest, err := d.Digest()
		if err != nil {
			t.Fatal(err)
		}
		for other, seen := range digests {
			if seen == digest {
				t.Fatalf("%s and %s share digest %s: start_from is not part of the canonical form", name, other, digest)
			}
		}
		digests[name] = digest
	}
	both, err := decl.Parse([]byte(withStartFrom(t, `{"actor_kind":"codex","host":"thor"}`)), decl.FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := both.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(canonical, []byte(`"start_from":{"actor_kind":"codex","host":"thor"}`)) {
		t.Fatalf("canonical form = %s, want start_from with sorted keys", canonical)
	}
	anyDecl, err := decl.Parse([]byte(withStartFrom(t, `"any"`)), decl.FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if anyDecl.StartFrom == nil || !anyDecl.StartFrom.Any {
		t.Fatalf("start_from any parsed as %+v", anyDecl.StartFrom)
	}
	if c, _ := anyDecl.CanonicalJSON(); !bytes.Contains(c, []byte(`"start_from":"any"`)) {
		t.Fatalf("canonical form = %s, want start_from \"any\"", c)
	}
}

// The validator refuses unknown keys, an actor_kind outside the closed set,
// an empty host, an empty object and any other scalar -- from JSON and YAML.
func TestStartFromValidatorRefusals(t *testing.T) {
	for name, value := range map[string]string{
		"unknown key":          `{"host":"thor","region":"eu"}`,
		"label key":            `{"label":"trusted"}`,
		"unknown actor kind":   `{"actor_kind":"gpt"}`,
		"empty host":           `{"host":""}`,
		"empty object":         `{}`,
		"other scalar":         `"all"`,
		"non-string host":      `{"host":7}`,
		"any inside an object": `{"any":true}`,
		"padded host":          `{"host":" thor"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decl.Parse([]byte(withStartFrom(t, value)), decl.FormatJSON); err == nil {
				t.Fatalf("start_from %s was accepted", value)
			}
		})
	}
	yaml := append(fixture(t, "minimal.yaml"), []byte("start_from:\n  actor_kind: robot\n")...)
	if _, err := decl.Parse(yaml, decl.FormatYAML); err == nil {
		t.Fatal("YAML start_from.actor_kind robot was accepted")
	}
	yaml = append(fixture(t, "minimal.yaml"), []byte("start_from:\n  host: thor\n  actor_kind: qwen\n")...)
	d, err := decl.Parse(yaml, decl.FormatYAML)
	if err != nil {
		t.Fatalf("YAML start_from {host, actor_kind}: %v", err)
	}
	if d.StartFrom == nil || d.StartFrom.Host != "thor" || d.StartFrom.ActorKind != "qwen" {
		t.Fatalf("YAML start_from parsed as %+v", d.StartFrom)
	}
	for kind := range decl.ActorKinds {
		if _, err := decl.Parse([]byte(withStartFrom(t, `{"actor_kind":"`+kind+`"}`)), decl.FormatJSON); err != nil {
			t.Errorf("actor_kind %s refused: %v", kind, err)
		}
	}
}

// Matches is AND over the given keys; an unset node type never matches a
// key that asks for it; any matches every node.
func TestStartFromMatchesIsAnd(t *testing.T) {
	both := decl.StartFrom{Host: "thor", ActorKind: "codex"}
	for _, tc := range []struct {
		sf          decl.StartFrom
		host, kind  string
		want        bool
		description string
	}{
		{both, "thor", "codex", true, "both keys match"},
		{both, "orin", "codex", false, "host differs"},
		{both, "thor", "claude", false, "actor kind differs"},
		{both, "", "codex", false, "host unset"},
		{decl.StartFrom{ActorKind: "codex"}, "", "codex", true, "only actor kind asked"},
		{decl.StartFrom{Host: "thor"}, "thor", "", true, "only host asked"},
		{decl.StartFrom{Any: true}, "", "", true, "any"},
	} {
		if got := tc.sf.Matches(tc.host, tc.kind); got != tc.want {
			t.Errorf("%s: %+v.Matches(%q,%q) = %v, want %v", tc.description, tc.sf, tc.host, tc.kind, got, tc.want)
		}
	}
}
