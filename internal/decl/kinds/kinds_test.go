package kinds

import "testing"

func TestRegisteredKindsHaveSignatures(t *testing.T) {
	artifacts := map[ArtifactType]bool{
		ArtifactNone: true, ArtifactAgentWork: true, ArtifactCodeResult: true,
		ArtifactDiscordMessage: true, ArtifactGitHubPR: true,
		ArtifactGitHubComment: true, ArtifactJiraIssue: true,
		ArtifactJiraComment: true, ArtifactHumanDecision: true,
		ArtifactTimer: true, ArtifactNode: true, ArtifactActionResult: true,
		ArtifactDeclaration: true,
	}
	for _, kind := range Actions() {
		checkSignature(t, "action", kind, artifacts)
	}
	for _, kind := range Triggers() {
		checkSignature(t, "trigger", kind, artifacts)
	}
}

func checkSignature(t *testing.T, category string, kind Kind, artifacts map[ArtifactType]bool) {
	t.Helper()
	if kind.Name == "" || kind.Version == 0 || len(kind.Produces) == 0 || len(kind.Consumes) == 0 {
		t.Errorf("%s kind has incomplete signature: %+v", category, kind)
	}
	for _, artifact := range append(append([]ArtifactType(nil), kind.Consumes...), kind.Produces...) {
		if !artifacts[artifact] {
			t.Errorf("%s kind %q uses unregistered artifact type %q", category, kind.Name, artifact)
		}
	}
}

func TestClosedVersionedVocabulary(t *testing.T) {
	wantActions := []string{"agent.work", "discord.post", "github.comment", "github.review_reply", "jira.comment", "jira.transition", "jira.create", "code.run", "human.ask", "activate"}
	wantTriggers := []string{"github.pr.approved", "github.pr.created", "jira.issue.created", "jira.comment", "human.decision", "code.result", "node.expired", "action.failed", "action.timed_out", "action.rejected", "action.capacity_exhausted", "action.budget_exhausted", "timer", "declaration.proposed", "declaration.overlap"}
	assertNames(t, "action", wantActions, Actions())
	assertNames(t, "trigger", wantTriggers, Triggers())
}

func assertNames(t *testing.T, category string, want []string, got []Kind) {
	t.Helper()
	seen := make(map[string]bool, len(got))
	for _, kind := range got {
		if seen[kind.Name] {
			t.Errorf("duplicate %s kind %q", category, kind.Name)
		}
		seen[kind.Name] = true
	}
	for _, name := range want {
		if !seen[name] {
			t.Errorf("missing %s kind %q", category, name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s vocabulary has %d entries, want closed set of %d", category, len(got), len(want))
	}
}
