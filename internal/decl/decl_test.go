package decl_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/decl"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDeclarationGoldenCanonicalForms(t *testing.T) {
	y, err := decl.Parse(fixture(t, "minimal.yaml"), decl.FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	j, err := decl.Parse(fixture(t, "minimal.json"), decl.FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if y.Condition != "true" || y.Trigger.ReentryLimit != 3 || y.Trigger.HopLimit != 20 || y.Trigger.RateCeiling != "30/h" {
		t.Fatalf("defaults: %+v", y)
	}
	a, err := y.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	b, err := j.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.TrimSpace(fixture(t, "minimal.canonical.json"))
	if !bytes.Equal(a, b) || !bytes.Equal(a, want) {
		t.Fatalf("canonical mismatch\nyaml %s\njson %s\nwant %s", a, b, want)
	}
	digest, err := y.Digest()
	if err != nil {
		t.Fatal(err)
	}
	other, err := j.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if digest != other || digest != strings.TrimSpace(string(fixture(t, "minimal.digest.txt"))) || digest != contracts.Digest(want) {
		t.Fatalf("digest mismatch: %s %s", digest, other)
	}
}

func TestExactlyOneUnitAndFanOutHint(t *testing.T) {
	base := string(fixture(t, "minimal.json"))
	for _, tc := range []struct{ name, source, hint string }{
		{"missing name", strings.Replace(base, `"name":"sample",`, "", 1), "name"},
		{"missing trigger", strings.Replace(base, `"trigger":{"kind":"timer"},`, "", 1), "trigger"},
		{"missing action", strings.Replace(base, `,"action":{"kind":"discord.post"}`, "", 1), "action"},
		{"missing start", strings.Replace(base, `"start_node":{"name":"root","deadline":"none"},`, "", 1), "start_node"},
		{"missing landing", strings.Replace(base, `,"landing_node":{"name":"waiting","deadline":"1h"}`, "", 1), "landing_node"},
		{"two triggers", strings.Replace(base, `"trigger":{"kind":"timer"}`, `"triggers":[{"kind":"timer"},{"kind":"jira.issue.created"}]`, 1), "fan-out"},
		{"two actions", strings.Replace(base, `"action":{"kind":"discord.post"}`, `"actions":[{"kind":"discord.post"},{"kind":"jira.comment"}]`, 1), "fan-out"},
		{"duplicate trigger key", strings.Replace(base, `"trigger":{"kind":"timer"}`, `"trigger":{"kind":"timer"},"trigger":{"kind":"jira.issue.created"}`, 1), "trigger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decl.Parse([]byte(tc.source), decl.FormatJSON); err == nil || !strings.Contains(err.Error(), tc.hint) {
				t.Fatalf("want %q error, got %v", tc.hint, err)
			}
		})
	}
}

func TestNodesRequireDeadlineOrNone(t *testing.T) {
	base := string(fixture(t, "minimal.json"))
	for _, node := range []string{"root", "waiting"} {
		source := strings.Replace(base, `"name":"`+node+`","deadline":"none"`, `"name":"`+node+`"`, 1)
		if node == "waiting" {
			source = strings.Replace(base, `"name":"waiting","deadline":"1h"`, `"name":"waiting"`, 1)
		}
		if _, err := decl.Parse([]byte(source), decl.FormatJSON); err == nil || !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("%s: %v", node, err)
		}
	}
}

func TestTriggerLimitsOverrideIncludingZero(t *testing.T) {
	base := string(fixture(t, "minimal.json"))
	source := strings.Replace(base, `"kind":"timer"`, `"kind":"timer","reentry_limit":0,"hop_limit":7,"rate_ceiling":"5/h"`, 1)
	d, err := decl.Parse([]byte(source), decl.FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if d.Trigger.ReentryLimit != 0 || d.Trigger.HopLimit != 7 || d.Trigger.RateCeiling != "5/h" {
		t.Fatalf("limits: %+v", d.Trigger)
	}
	for _, field := range []string{`"reentry_limit":-1`, `"hop_limit":0`, `"rate_ceiling":"0/h"`} {
		bad := strings.Replace(base, `"kind":"timer"`, `"kind":"timer",`+field, 1)
		if _, err := decl.Parse([]byte(bad), decl.FormatJSON); err == nil {
			t.Errorf("accepted %s", field)
		}
	}
}

func TestYAMLDuplicateTriggerNamesFanOut(t *testing.T) {
	source := append(fixture(t, "minimal.yaml"), []byte("trigger:\n  kind: jira.issue.created\n")...)
	if _, err := decl.Parse(source, decl.FormatYAML); err == nil || !strings.Contains(err.Error(), "fan-out") {
		t.Fatalf("want fan-out hint, got %v", err)
	}
}

func TestExplicitConditionAndPayloadArePreserved(t *testing.T) {
	source := strings.Replace(string(fixture(t, "minimal.json")), `"trigger":{"kind":"timer"}`, `"condition":"event.priority == 'High'","trigger":{"kind":"timer","with":{"z":2,"a":1}}`, 1)
	d, err := decl.Parse([]byte(source), decl.FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if d.Condition != "event.priority == 'High'" {
		t.Fatalf("condition: %q", d.Condition)
	}
	canonical, err := d.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(canonical, []byte(`"with":{"a":1,"z":2}`)) {
		t.Fatalf("payload was not canonicalized: %s", canonical)
	}
}

func TestKindsMustBeRegistered(t *testing.T) {
	base := string(fixture(t, "minimal.json"))
	for name, source := range map[string]string{
		"unknown trigger": strings.Replace(base, `"kind":"timer"`, `"kind":"github.pr.teleported"`, 1),
		"unknown action":  strings.Replace(base, `"kind":"discord.post"`, `"kind":"fax.send"`, 1),
	} {
		if _, err := decl.Parse([]byte(source), decl.FormatJSON); err == nil || !strings.Contains(err.Error(), "registered vocabulary") {
			t.Errorf("%s: err = %v, want a registered-vocabulary refusal", name, err)
		}
	}
}
