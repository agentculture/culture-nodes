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
	// comment-post path, ever. Jira Cloud's write surface lives under
	// /rest/api/3/issue/ (comment posting included); the sweep's only
	// Jira endpoint is the search read. The write pair belongs to the
	// Jira actor bridge alone (spec boundary c5 as corrected by the
	// 2026-08-18 partial-harvest deviation record).
	raw, err := os.ReadFile(filepath.Join(root, "examples/pr-upkeep/sweep.py"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "/rest/api/3/issue/") {
		t.Error("the sweep gained a Jira write-shaped endpoint; comment authority belongs to the Jira actor bridge")
	}
}
