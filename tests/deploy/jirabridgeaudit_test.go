package deploytest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Jira credential may be relayed into the actor-owned env file, but no
// control-plane, compose, runner, or committed config may consume it.
func TestJiraCredentialIsActorOnly(t *testing.T) {
	root := repoRootDir(t)
	for _, rel := range []string{
		"deploy/prod/compose.thor.yml",
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
	// the read shape instead: exactly one issue-scoped URL, the GET-only
	// pagination helper's, and exactly one POST anywhere in the file, the
	// control-plane event emit. A second occurrence of either is a new
	// endpoint someone must argue for here.
	raw, err := os.ReadFile(filepath.Join(root, "examples/pr-upkeep/sweep.py"))
	if err != nil {
		t.Fatal(err)
	}
	sweep := string(raw)
	if got := strings.Count(sweep, "/rest/api/3/issue/"); got != 1 {
		t.Errorf("sweep.py has %d issue-scoped Jira endpoints, want exactly the pagination read; comment authority belongs to the Jira actor bridge", got)
	}
	if !strings.Contains(sweep, `f"https://{site}/rest/api/3/issue/{issue_key}/{collection}?{query}", basic=basic`) {
		t.Error("sweep.py's one issue-scoped Jira endpoint is no longer the GET-only pagination read (_extend_jira_issue_collection via _get_json)")
	}
	if got := strings.Count(sweep, `method="POST"`); got != 1 {
		t.Errorf("sweep.py has %d POSTs, want exactly the control-plane event emit", got)
	}
	if !strings.Contains(sweep, `"/v1alpha1/events", data=body, method="POST"`) {
		t.Error("sweep.py's one POST is no longer the control-plane event emit")
	}
}
