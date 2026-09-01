package deploytest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Jira write authority remains actor-only. The control-plane API may hold the
// same read-only Basic-auth pair as the sweep solely for t16's loopback
// webhook hydration; workers, runners, and bridge configs still may not.
func TestJiraCredentialIsActorOnly(t *testing.T) {
	root := repoRootDir(t)
	for _, rel := range []string{
		"deploy/prod/compose.orin.yml",
		"deploy/prod/nodes-runner.service",
		"deploy/prod/codex-bridge.json.template",
	} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "JIRA_ACCOUNT_EMAIL") || strings.Contains(string(raw), "JIRA_API_TOKEN") {
			t.Errorf("%s gives Jira authority to a non-Jira-actor runtime", rel)
		}
	}
	thorRaw, err := os.ReadFile(filepath.Join(root, "deploy/prod/compose.thor.yml"))
	if err != nil {
		t.Fatal(err)
	}
	thor := string(thorRaw)
	if strings.Count(thor, "      JIRA_ACCOUNT_EMAIL: ${") != 1 || strings.Count(thor, "      JIRA_API_TOKEN: ${") != 1 {
		t.Error("compose.thor.yml must grant the Jira read pair exactly once, to the API webhook hydrator")
	}
	// The sweep KEEPS its Jira READ pair: fetching a backlog is not a
	// real-world act, and #76/#106 deliberately place that read in the
	// sweep. What the sweep may never hold is WRITE authority - no
	// comment-post path, ever. The write pair belongs to the Jira actor
	// bridge alone (spec boundary c5 as corrected by the 2026-08-18
	// partial-harvest deviation record).
	//
	// This guard used to forbid /rest/api/3/issue/ outright, back when the
	// sweep's only Jira endpoint was the search read. The flow-store cycle's
	// history replay (spec c2, #193, plan t1/#203) legitimately reads the
	// issue-scoped changelog and comment collections through that path, so
	// the guard was narrowed — deliberately, at the WP-A merge gate — to pin
	// the read shape instead. Approved deviation d1 (#193/#203) then split
	// that read/replay layer into pr_upkeep_jira.py: sweep.py must now have
	// zero issue-scoped URLs, the sibling module exactly one GET-only
	// pagination URL, and sweep.py exactly one POST for control-plane event
	// emission. A second occurrence of either is a new endpoint someone must
	// argue for here.
	raw, err := os.ReadFile(filepath.Join(root, "examples/pr-upkeep/sweep.py"))
	if err != nil {
		t.Fatal(err)
	}
	sweep := string(raw)
	if got := strings.Count(sweep, "/rest/api/3/issue/"); got != 0 {
		t.Errorf("sweep.py has %d issue-scoped Jira endpoints, want none after the d1 split", got)
	}
	jiraRaw, err := os.ReadFile(filepath.Join(root, "examples/pr-upkeep/pr_upkeep_jira.py"))
	if err != nil {
		t.Fatal(err)
	}
	jira := string(jiraRaw)
	if got := strings.Count(jira, "/rest/api/3/issue/"); got != 1 {
		t.Errorf("pr_upkeep_jira.py has %d issue-scoped Jira endpoints, want exactly the pagination read", got)
	}
	if !strings.Contains(jira, `f"https://{site}/rest/api/3/issue/{issue_key}/{collection}?{query}", basic=basic`) {
		t.Error("pr_upkeep_jira.py's issue-scoped endpoint is no longer the GET-only pagination read")
	}
	if strings.Contains(jira, `method="POST"`) {
		t.Error("pr_upkeep_jira.py contains a POST; the Jira layer is read-only")
	}
	if got := strings.Count(sweep, `method="POST"`); got != 1 {
		t.Errorf("sweep.py has %d POSTs, want exactly the control-plane event emit", got)
	}
	if !strings.Contains(sweep, `"/v1alpha1/events", data=body, method="POST"`) {
		t.Error("sweep.py's one POST is no longer the control-plane event emit")
	}
}
