package deploytest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func jiraDeployFunction(t *testing.T) string {
	t.Helper()
	script := deployScriptText(t)
	start := strings.Index(script, "deploy_jira() {")
	if start == -1 {
		t.Fatal("deploy.sh has no deploy_jira function")
	}
	end := strings.Index(script[start:], "\n}\n\n# --- the two-host")
	if end == -1 {
		t.Fatal("could not bound deploy_jira function")
	}
	return script[start : start+end+2]
}

func jiraBridgeJiraEnvPath(t *testing.T, c *fakeCluster, host string) string {
	t.Helper()
	return filepath.Join(c.hostHome(t, host), ".culture-nodes", "jira-bridge-jira.env")
}

func TestDeployJiraMergesFourTargetAllowlistWithoutClobberingCredentials(t *testing.T) {
	c := newFakeCluster(t)
	path := jiraBridgeJiraEnvPath(t, c, thorFake)
	seedFile(t, path, "JIRA_ACCOUNT_EMAIL=robot@example.invalid\nJIRA_API_TOKEN=keep-this-token\nUNRELATED=keep-this-too\n")
	seedFile(t, filepath.Join(c.hostHome(t, thorFake), ".culture-nodes", "jira-bridge-auth.env"), "TOKEN=present\n")

	snippet := `set -euo pipefail
REMOTE_DIR=culture-nodes-prod
JIRA_SITE=team.example.invalid
say() { printf '==> %s\n' "$*"; }
resolve_actor_row_id() { printf 'actor-row-id\n'; }
assert_unit_healthy() { :; }
ssh() {
  host=$1
  shift
  case "$*" in
    *"uv tool install"*|*"systemctl --user"*) return 0 ;;
    *"command -v jira-bridge"*) printf '/usr/bin/true\n'; return 0 ;;
  esac
  command ssh "$host" "$@"
}
` + jiraDeployFunction(t) + `
deploy_jira "` + thorFake + `"
`
	stdout, stderr, code := runSnippet(t, c, snippet)
	if code != 0 {
		t.Fatalf("deploy_jira exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	env := readEnvFile(t, path)
	for key, want := range map[string]string{
		"JIRA_ACCOUNT_EMAIL":             "robot@example.invalid",
		"JIRA_API_TOKEN":                 "keep-this-token",
		"UNRELATED":                      "keep-this-too",
		"JIRA_TRANSITION_TARGETS":        "In Progress,Pending,In Review,Done",
		"JIRA_TRANSITION_PROJECT_PREFIX": "SCRUM-",
	} {
		if got := env.values[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if strings.Contains(stdout+stderr, "In Progress,Pending,In Review,Done") || strings.Contains(stdout+stderr, "SCRUM-") {
		t.Errorf("deploy output exposed transition configuration values:\n%s%s", stdout, stderr)
	}
}

func TestGrantCheckNamesDeployManagedJiraTransitionKeys(t *testing.T) {
	lane, err := os.ReadFile(grantCheckPath(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"JIRA_TRANSITION_TARGETS", "JIRA_TRANSITION_PROJECT_PREFIX"} {
		if !strings.Contains(string(lane), key) {
			t.Errorf("grant-check.sh does not name deploy-managed key %s", key)
		}
	}
}
