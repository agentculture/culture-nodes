package deploytest

import (
	"os"
	"strings"
	"testing"
)

// JIRA_API_BASE is the REST base a SCOPED Jira Cloud service-account token
// authenticates at: the Atlassian gateway answers, the site URL returns 401.
// It is therefore not derivable from JIRA_SITE, and both consumers -- the
// sweep (through runner-secrets.env) and the bridge (through
// jira-bridge-jira.env) -- need the deploy to carry it.
//
// Everything here runs against the fake cluster; no host is contacted.

// TestInstallSecretsGrantsTheApiBaseNameOnAFirstDeploy: the runner boundary
// refuses an operation whose environment_refs name anything absent from the
// worker process, and the sweep node now names JIRA_API_BASE unconditionally.
// So on a host with no runner-secrets.env the NAME must be granted, empty --
// exactly the #128 argument that granted the empty pair.
func TestInstallSecretsGrantsTheApiBaseNameOnAFirstDeploy(t *testing.T) {
	c := newFakeCluster(t)
	stdout, stderr, code := c.runSplit(t, installSecretsPath(t), []string{thorFake, orinFake})
	if code != 0 {
		t.Fatalf("install-secrets.sh exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, host := range []string{thorFake, orinFake} {
		env := readEnvFile(t, runnerSecretsPath(t, c, host))
		value, ok := env.values["JIRA_API_BASE"]
		if !ok {
			t.Errorf("first deploy on %s did not grant the name JIRA_API_BASE; the sweep's "+
				"environment_refs would be refused before it ran", host)
			continue
		}
		if value != "" {
			t.Errorf("JIRA_API_BASE on %s = %q, want the empty grant that means the site URL",
				host, value)
		}
	}
}

// TestInstallSecretsMergesTheApiBaseBesideThePair: a shell that holds the
// base writes it, and nothing else in the file moves.
func TestInstallSecretsMergesTheApiBaseBesideThePair(t *testing.T) {
	const gateway = "https://api.atlassian.com/ex/jira/0610b05c-63f8-4935-bd7f-a30f907bba8c"
	c := newFakeCluster(t)
	for _, host := range []string{thorFake, orinFake} {
		seedFile(t, runnerSecretsPath(t, c, host), fiveKeyRunnerSecrets())
	}

	stdout, stderr, code := c.runSplit(t, installSecretsPath(t), []string{thorFake, orinFake},
		"JIRA_ACCOUNT_EMAIL=service@example.invalid", "JIRA_API_TOKEN=scoped-token",
		"JIRA_API_BASE="+gateway)
	if code != 0 {
		t.Fatalf("install-secrets.sh exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if strings.Contains(stdout+stderr, "scoped-token") {
		t.Errorf("the deploy output exposed the Jira token:\n%s%s", stdout, stderr)
	}

	for _, host := range []string{thorFake, orinFake} {
		env := readEnvFile(t, runnerSecretsPath(t, c, host))
		for key, want := range map[string]string{
			"JIRA_ACCOUNT_EMAIL": "service@example.invalid",
			"JIRA_API_TOKEN":     "scoped-token",
			"JIRA_API_BASE":      gateway,
			"GITHUB_TOKEN":       fixtureGitHubToken,
			"SONAR_TOKEN":        fixtureSonarToken,
			"NODES_EVENT_TOKEN":  fixtureEventToken,
		} {
			if got := env.values[key]; got != want {
				t.Errorf("%s on %s = %q, want %q", key, host, got, want)
			}
		}
		if len(env.order) != 6 {
			t.Errorf("runner-secrets.env on %s holds %d keys (%v), want the five it had plus "+
				"JIRA_API_BASE", host, len(env.order), env.order)
		}
	}
}

// TestInstallSecretsLeavesAConfiguredApiBaseAloneWhenUnset is the #253 shape
// applied to the new key: rotating the pair without exporting the base must
// not silently clear the base and send every read back to the 401ing site URL.
func TestInstallSecretsLeavesAConfiguredApiBaseAloneWhenUnset(t *testing.T) {
	const gateway = "https://api.atlassian.com/ex/jira/0610b05c-63f8-4935-bd7f-a30f907bba8c"
	c := newFakeCluster(t)
	for _, host := range []string{thorFake, orinFake} {
		seedFile(t, runnerSecretsPath(t, c, host),
			fiveKeyRunnerSecrets()+"JIRA_API_BASE="+gateway+"\n")
	}

	stdout, stderr, code := c.runSplit(t, installSecretsPath(t), []string{thorFake, orinFake},
		"JIRA_ACCOUNT_EMAIL=rotated@example.invalid", "JIRA_API_TOKEN=rotated-jira-token")
	if code != 0 {
		t.Fatalf("install-secrets.sh exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, host := range []string{thorFake, orinFake} {
		env := readEnvFile(t, runnerSecretsPath(t, c, host))
		if got := env.values["JIRA_API_BASE"]; got != gateway {
			t.Errorf("JIRA_API_BASE on %s = %q after a pair rotation, want it untouched (%q)",
				host, got, gateway)
		}
	}
}

// TestInstallSecretsClearsTheApiBaseOnlyWhenSetEmpty: the lane tells SET-and-
// empty from UNSET, so an operator can deliberately turn the gateway base off.
func TestInstallSecretsClearsTheApiBaseOnlyWhenSetEmpty(t *testing.T) {
	c := newFakeCluster(t)
	for _, host := range []string{thorFake, orinFake} {
		seedFile(t, runnerSecretsPath(t, c, host),
			fiveKeyRunnerSecrets()+"JIRA_API_BASE=https://api.atlassian.com/ex/jira/abc\n")
	}

	stdout, stderr, code := c.runSplit(t, installSecretsPath(t), []string{thorFake, orinFake},
		"JIRA_API_BASE=")
	if code != 0 {
		t.Fatalf("install-secrets.sh exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, host := range []string{thorFake, orinFake} {
		env := readEnvFile(t, runnerSecretsPath(t, c, host))
		if got := env.values["JIRA_API_BASE"]; got != "" {
			t.Errorf("JIRA_API_BASE on %s = %q, want it cleared", host, got)
		}
		// Setting only the base must not disturb the pair, and must not be
		// mistaken for the half-a-pair refusal.
		if got := env.values["JIRA_ACCOUNT_EMAIL"]; got != fixtureJiraEmail {
			t.Errorf("JIRA_ACCOUNT_EMAIL on %s = %q, want it untouched", host, got)
		}
	}
}

// TestDeployJiraWritesTheApiBaseIntoTheBridgeEnv: the bridge reads the base
// from its own process environment, so the deploy-managed key has to land in
// jira-bridge-jira.env beside the transition allowlist -- without clobbering
// the credential pair install-secrets put in the same file.
func TestDeployJiraWritesTheApiBaseIntoTheBridgeEnv(t *testing.T) {
	const gateway = "https://api.atlassian.com/ex/jira/0610b05c-63f8-4935-bd7f-a30f907bba8c"
	c := newFakeCluster(t)
	path := jiraBridgeJiraEnvPath(t, c, thorFake)
	seedFile(t, path, "JIRA_ACCOUNT_EMAIL=robot@example.invalid\nJIRA_API_TOKEN=keep-this-token\n")
	seedFile(t, jiraBridgeAuthEnvPath(t, c, thorFake), "TOKEN=present\n")

	stdout, stderr, code := runSnippet(t, c, jiraDeploySnippet(t, gateway))
	if code != 0 {
		t.Fatalf("deploy_jira exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	env := readEnvFile(t, path)
	for key, want := range map[string]string{
		"JIRA_API_BASE":      gateway,
		"JIRA_ACCOUNT_EMAIL": "robot@example.invalid",
		"JIRA_API_TOKEN":     "keep-this-token",
	} {
		if got := env.values[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// TestDeployJiraLeavesAConfiguredApiBaseAloneWhenUnset: the two transition
// keys have safe defaults, so writing them on every deploy restores the
// intended value. This one does not -- an empty base is a working config for
// an unscoped token and a 401 for a scoped one -- so an ordinary deploy that
// does not export it must leave the host's alone rather than clear it.
func TestDeployJiraLeavesAConfiguredApiBaseAloneWhenUnset(t *testing.T) {
	const gateway = "https://api.atlassian.com/ex/jira/0610b05c-63f8-4935-bd7f-a30f907bba8c"
	c := newFakeCluster(t)
	path := jiraBridgeJiraEnvPath(t, c, thorFake)
	seedFile(t, path, "JIRA_ACCOUNT_EMAIL=robot@example.invalid\nJIRA_API_BASE="+gateway+"\n")
	seedFile(t, jiraBridgeAuthEnvPath(t, c, thorFake), "TOKEN=present\n")

	stdout, stderr, code := runSnippet(t, c, jiraDeploySnippetWithoutApiBase(t))
	if code != 0 {
		t.Fatalf("deploy_jira exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	env := readEnvFile(t, path)
	if got := env.values["JIRA_API_BASE"]; got != gateway {
		t.Errorf("JIRA_API_BASE = %q after a deploy that did not export it, want it untouched (%q)",
			got, gateway)
	}
	// The keys that DO have defaults are still written on the same pass.
	if got := env.values["JIRA_TRANSITION_PROJECT_PREFIX"]; got != "SCRUM-" {
		t.Errorf("JIRA_TRANSITION_PROJECT_PREFIX = %q, want the deploy-managed default", got)
	}
}

// An exported-but-empty base is the deliberate way to clear it.
func TestDeployJiraClearsTheApiBaseWhenExportedEmpty(t *testing.T) {
	c := newFakeCluster(t)
	path := jiraBridgeJiraEnvPath(t, c, thorFake)
	seedFile(t, path, "JIRA_API_BASE=https://api.atlassian.com/ex/jira/abc\n")
	seedFile(t, jiraBridgeAuthEnvPath(t, c, thorFake), "TOKEN=present\n")

	stdout, stderr, code := runSnippet(t, c, jiraDeploySnippet(t, ""))
	if code != 0 {
		t.Fatalf("deploy_jira exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	env := readEnvFile(t, path)
	if value, ok := env.values["JIRA_API_BASE"]; !ok || value != "" {
		t.Errorf("JIRA_API_BASE = %q (present=%v), want it cleared", value, ok)
	}
}

func TestGrantCheckNamesTheApiBase(t *testing.T) {
	lane, err := os.ReadFile(grantCheckPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lane), "JIRA_API_BASE") {
		t.Error("grant-check.sh does not name deploy-managed key JIRA_API_BASE; the deploy " +
			"that first grants it would be refused by its own preflight")
	}
}

// TestTheSweepNodeGrantsTheApiBase pins the other half: the workflow has to
// NAME the value, or the runner never puts it in the sweep's environment.
func TestTheSweepNodeGrantsTheApiBase(t *testing.T) {
	root := repoRootDir(t)
	for _, rel := range []string{
		"examples/pr-upkeep/sweep-cycle.workflow.yaml",
		"examples/pr-upkeep/README.md",
	} {
		raw, err := os.ReadFile(root + "/" + rel)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "JIRA_API_BASE") {
			t.Errorf("%s does not name the granted value JIRA_API_BASE", rel)
		}
	}
}

func jiraBridgeAuthEnvPath(t *testing.T, c *fakeCluster, host string) string {
	t.Helper()
	return c.hostHome(t, host) + "/.culture-nodes/jira-bridge-auth.env"
}

// jiraDeploySnippet runs the REAL deploy_jira body against the fake cluster,
// the way jiratransitionconfig_test.go does, with only the remote install and
// systemd calls stubbed.
func jiraDeploySnippet(t *testing.T, apiBase string) string {
	t.Helper()
	return jiraDeployPreamble(t, "JIRA_API_BASE="+shellQuote(apiBase)+"\n")
}

// jiraDeploySnippetWithoutApiBase never assigns the name at all, which is the
// state an ordinary deploy shell is in.
func jiraDeploySnippetWithoutApiBase(t *testing.T) string {
	t.Helper()
	return jiraDeployPreamble(t, "")
}

func jiraDeployPreamble(t *testing.T, apiBaseAssignment string) string {
	t.Helper()
	return `set -euo pipefail
REMOTE_DIR=culture-nodes-prod
JIRA_SITE=team.example.invalid
` + apiBaseAssignment + `say() { printf '==> %s\n' "$*"; }
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
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
