// Package deploytest -- this file is task t5's (presentable-floor-before-oauth
// plan, issue #253): a deploy may not destroy a runner grant, and may not ship
// a host that lacks one a reachable workflow declares.
//
// The incident, on 2026-08-29. install_jira_runner_env did `cat >
// ~/.culture-nodes/runner-secrets.env` with two empty values whenever the
// deploying shell held no Jira pair. thor's runner-secrets.env also held three
// hand-granted sweep credentials (GITHUB_TOKEN, SONAR_TOKEN,
// NODES_EVENT_TOKEN), and the #243 cutover deploy truncated the file to 36
// bytes. The runner boundary then refused every sweep attempt with
// `rejected_input: environment_refs names GITHUB_TOKEN, SONAR_TOKEN,
// NODES_EVENT_TOKEN, not set in this worker process's own environment` -- 183
// of 275 runs over the following 16 hours, while the same workflow digest had
// completed 92 times before the deploy. Environment, not code.
//
// Three properties are pinned here, each closing a different half of that:
//
//  1. The Jira lane MERGES. It replaces its own two keys and touches no
//     other, and when the pair is unset on a host that already has the file
//     it refuses by name rather than writing empty values over the grants.
//  2. The deploy REFUSES A HOST THAT IS MISSING A GRANT, before anything is
//     shipped -- diffing the environmentRefs of every workflow the control
//     plane can start today against the key NAMES on the host, and naming the
//     missing key. Values are never read off the host and never printed, on
//     any path, so the refusal is safe to paste into an issue.
//  3. Every lane that rewrites either file leaves a timestamped copy of the
//     prior bytes and prints the command that restores it.
package deploytest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The two fake hosts. Named apart from the production `thor`/`orin` on
// purpose: every path here writes files, and a fixture that reads like the
// real host name is one copy-paste away from a probe run against one.
const (
	thorFake = "thor-fake"
	orinFake = "orin-fake"
)

// The fixture grants. Every value is a marker whose only job is to be searched
// for afterwards: acceptance criterion 4 is that no path of the check prints
// one, and criterion 1 is that the Jira lane does not eat them.
const (
	fixtureGitHubToken = "fixture-github-token-VALUE-must-never-be-printed"
	fixtureSonarToken  = "fixture-sonar-token-VALUE-must-never-be-printed"
	fixtureEventToken  = "fixture-event-token-VALUE-must-never-be-printed"
	fixtureJiraEmail   = "fixture-jira@example.invalid"
	fixtureJiraToken   = "fixture-jira-token-VALUE-must-never-be-printed"
)

// --- locating the lanes ---------------------------------------------------

func deployProdDir(t *testing.T) string {
	t.Helper()
	return filepath.Dir(installSecretsPath(t))
}

func grantCheckPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(deployProdDir(t), "lanes", "grant-check.sh")
}

func envBackupPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(deployProdDir(t), "lanes", "env-backup.sh")
}

func runnerEnvLanePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(deployProdDir(t), "lanes", "runner-env-write.sh")
}

func readLane(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// --- the two grant files on a fake host -----------------------------------

func runnerEnvPath(t *testing.T, c *fakeCluster, host string) string {
	t.Helper()
	return filepath.Join(c.hostHome(t, host), ".culture-nodes", "runner.env")
}

func runnerSecretsPath(t *testing.T, c *fakeCluster, host string) string {
	t.Helper()
	return filepath.Join(c.hostHome(t, host), ".culture-nodes", "runner-secrets.env")
}

// fiveKeyRunnerSecrets is the file thor actually held before the incident: the
// Jira pair the deploy lane owns, and three credentials granted by hand that
// no lane in this repo writes.
func fiveKeyRunnerSecrets() string {
	return "JIRA_ACCOUNT_EMAIL=" + fixtureJiraEmail + "\n" +
		"JIRA_API_TOKEN=" + fixtureJiraToken + "\n" +
		"GITHUB_TOKEN=" + fixtureGitHubToken + "\n" +
		"SONAR_TOKEN=" + fixtureSonarToken + "\n" +
		"NODES_EVENT_TOKEN=" + fixtureEventToken + "\n"
}

func seedFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// backupsOf returns the timestamped backups beside path, oldest name first.
func backupsOf(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatalf("glob %s.bak-*: %v", path, err)
	}
	sort.Strings(matches)
	return matches
}

// --- criterion 1: the Jira lane merges ------------------------------------

// TestJiraLaneLeavesAnExistingRunnerSecretsAloneWhenThePairIsUnset is the
// incident itself, inverted. Five keys go in, the deploying shell holds no
// Jira pair (the harness never gives a script one -- issue #134), and all five
// are still there afterwards with their values byte-exact.
func TestJiraLaneLeavesAnExistingRunnerSecretsAloneWhenThePairIsUnset(t *testing.T) {
	c := newFakeCluster(t)
	before := fiveKeyRunnerSecrets()
	for _, host := range []string{thorFake, orinFake} {
		seedFile(t, runnerSecretsPath(t, c, host), before)
	}

	stdout, stderr, code := c.runSplit(t, installSecretsPath(t), []string{thorFake, orinFake})
	if code != 0 {
		t.Fatalf("install-secrets.sh exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	for _, host := range []string{thorFake, orinFake} {
		after := readFileString(t, runnerSecretsPath(t, c, host))
		if after != before {
			t.Errorf("runner-secrets.env on %s changed with the Jira pair unset:\nbefore:\n%s\nafter:\n%s",
				host, before, after)
		}
	}
	// A refusal that is not announced is indistinguishable from a lane that
	// quietly did nothing, so the message is part of the contract.
	if !strings.Contains(stderr, "runner-secrets.env") || !strings.Contains(stderr, "JIRA_ACCOUNT_EMAIL") {
		t.Errorf("the Jira lane did not name its refusal on stderr; stderr was:\n%s", stderr)
	}
}

// TestJiraLaneReplacesOnlyItsOwnTwoKeys is the other half: when the pair IS
// supplied the lane must still not be a whole-file write.
func TestJiraLaneReplacesOnlyItsOwnTwoKeys(t *testing.T) {
	c := newFakeCluster(t)
	for _, host := range []string{thorFake, orinFake} {
		seedFile(t, runnerSecretsPath(t, c, host), fiveKeyRunnerSecrets())
	}

	stdout, stderr, code := c.runSplit(t, installSecretsPath(t), []string{thorFake, orinFake},
		"JIRA_ACCOUNT_EMAIL=rotated@example.invalid", "JIRA_API_TOKEN=rotated-jira-token")
	if code != 0 {
		t.Fatalf("install-secrets.sh exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	for _, host := range []string{thorFake, orinFake} {
		env := readEnvFile(t, runnerSecretsPath(t, c, host))
		for key, want := range map[string]string{
			"JIRA_ACCOUNT_EMAIL": "rotated@example.invalid",
			"JIRA_API_TOKEN":     "rotated-jira-token",
			"GITHUB_TOKEN":       fixtureGitHubToken,
			"SONAR_TOKEN":        fixtureSonarToken,
			"NODES_EVENT_TOKEN":  fixtureEventToken,
		} {
			if got := env.values[key]; got != want {
				t.Errorf("%s on %s = %q, want %q", key, host, got, want)
			}
		}
		if len(env.order) != 5 {
			t.Errorf("runner-secrets.env on %s holds %d lines (%v), want the same five keys",
				host, len(env.order), env.order)
		}
	}
}

// TestFirstDeployStillGrantsTheEmptyJiraPair keeps the #128 behaviour a
// merge could plausibly have broken: on a host with NO runner-secrets.env the
// two NAMES are still granted with empty values, because the runner boundary
// refuses an operation whose environment_refs names something absent from the
// process, and pr-upkeep's sweep names the pair unconditionally.
func TestFirstDeployStillGrantsTheEmptyJiraPair(t *testing.T) {
	c := newFakeCluster(t)
	stdout, stderr, code := c.runSplit(t, installSecretsPath(t), []string{thorFake, orinFake})
	if code != 0 {
		t.Fatalf("install-secrets.sh exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, host := range []string{thorFake, orinFake} {
		env := readEnvFile(t, runnerSecretsPath(t, c, host))
		for _, key := range []string{"JIRA_ACCOUNT_EMAIL", "JIRA_API_TOKEN"} {
			if _, ok := env.values[key]; !ok {
				t.Errorf("first deploy on %s did not grant the name %s; the sweep's "+
					"environment_refs would be refused before it ran", host, key)
			}
		}
	}
}

// --- criterion 3: the timestamped backups ---------------------------------

// runRunnerEnvLane executes the real runner.env replacement block against one
// fake host, the way tests/test_deploy_runner_env.py does -- the lane is
// sourced by deploy.sh rather than executed, so the block is run with the
// variables deploy.sh would have set.
func runRunnerEnvLane(t *testing.T, c *fakeCluster, host string, extraEnv ...string) (string, string, int) {
	t.Helper()
	snippet := "set -euo pipefail\n" +
		"say() { printf '==> %s\\n' \"$*\"; }\n" +
		"HOST=" + host + "\n" +
		"SCRIPT_DIR=" + deployProdDir(t) + "\n" +
		". " + envBackupPath(t) + "\n" +
		". " + runnerEnvLanePath(t) + "\n"
	return runSnippet(t, c, snippet, extraEnv...)
}

func runSnippet(t *testing.T, c *fakeCluster, snippet string, extraEnv ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("bash", "-c", snippet)
	cmd.Env = c.env(extraEnv...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return stdout.String(), stderr.String(), exitErr.ExitCode()
	}
	t.Fatalf("run snippet: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	return "", "", -1
}

// runnerEnvLaneEnv is what deploy.sh has in scope by the time it sources the
// runner.env lane: a revision, the sweep source grant, and a control-plane URL.
func runnerEnvLaneEnv() []string {
	return []string{
		"REVISION=0123456789abcdef0123456789abcdef01234567",
		"NODES_API_URL=http://192.0.2.44:18080",
		"PR_UPKEEP_SWEEP_SOURCE_URL=https://example.invalid/sweep.py",
		"PR_UPKEEP_SWEEP_SOURCE_SHA256=" + strings.Repeat("a", 64),
		"PR_UPKEEP_SWEEP_JIRA_SOURCE_URL=https://example.invalid/jira.py",
		"PR_UPKEEP_SWEEP_JIRA_SOURCE_SHA256=" + strings.Repeat("b", 64),
		"PR_UPKEEP_SWEEP_EMIT_SOURCE_URL=https://example.invalid/emit.py",
		"PR_UPKEEP_SWEEP_EMIT_SOURCE_SHA256=" + strings.Repeat("c", 64),
	}
}

// TestBothGrantFilesAreBackedUpWithTheirPriorBytes is acceptance criterion 5.
// One deploy touches both files -- install-secrets.sh's Jira lane rewrites
// runner-secrets.env, deploy.sh's runner-env lane rewrites runner.env -- and
// each must leave the bytes it replaced beside it, plus the command that puts
// them back.
func TestBothGrantFilesAreBackedUpWithTheirPriorBytes(t *testing.T) {
	c := newFakeCluster(t)
	secretsBefore := fiveKeyRunnerSecrets()
	runnerBefore := "NODES_API_URL=http://192.0.2.44:18080\n" +
		"PR_UPKEEP_REPOSITORIES='{\"cycle\":0,\"repositories\":[]}'\n" +
		"OLD_KEY=prior-bytes\n"
	seedFile(t, runnerSecretsPath(t, c, thorFake), secretsBefore)
	seedFile(t, runnerEnvPath(t, c, thorFake), runnerBefore)

	secretsOut, secretsErr, code := c.runSplit(t, installSecretsPath(t), []string{thorFake, orinFake},
		"JIRA_ACCOUNT_EMAIL=rotated@example.invalid", "JIRA_API_TOKEN=rotated-jira-token")
	if code != 0 {
		t.Fatalf("install-secrets.sh exited %d\nstdout:\n%s\nstderr:\n%s", code, secretsOut, secretsErr)
	}
	runnerOut, runnerErr, code := runRunnerEnvLane(t, c, thorFake, runnerEnvLaneEnv()...)
	if code != 0 {
		t.Fatalf("runner-env lane exited %d\nstdout:\n%s\nstderr:\n%s", code, runnerOut, runnerErr)
	}

	for _, tc := range []struct {
		file   string
		path   string
		before string
		output string
	}{
		{"runner-secrets.env", runnerSecretsPath(t, c, thorFake), secretsBefore, secretsOut},
		{"runner.env", runnerEnvPath(t, c, thorFake), runnerBefore, runnerOut},
	} {
		backups := backupsOf(t, tc.path)
		if len(backups) != 1 {
			t.Fatalf("%s left %d backups (%v), want exactly one", tc.file, len(backups), backups)
		}
		if got := readFileString(t, backups[0]); got != tc.before {
			t.Errorf("%s backup holds %q, want the prior bytes %q", tc.file, got, tc.before)
		}
		// The restore command has to be in the deploy log: an operator
		// reading it at 03:00 should not have to reconstruct the path.
		if !strings.Contains(tc.output, filepath.Base(backups[0])) ||
			!strings.Contains(tc.output, "restore") {
			t.Errorf("%s lane printed no restore command naming %s; output was:\n%s",
				tc.file, filepath.Base(backups[0]), tc.output)
		}
	}
}

// TestBackupsDoNotAccumulateForever -- each backup is a second copy of live
// credentials, so a deploy that adds one per run and never removes one turns
// ~/.culture-nodes into an unbounded credential archive.
func TestBackupsDoNotAccumulateForever(t *testing.T) {
	c := newFakeCluster(t)
	path := runnerSecretsPath(t, c, thorFake)
	seedFile(t, path, fiveKeyRunnerSecrets())
	for i := range 14 {
		seedFile(t, path+".bak-20260101T0000"+string(rune('0'+i/10))+string(rune('0'+i%10))+"Z", "old\n")
	}
	snippet := "set -euo pipefail\n. " + envBackupPath(t) + "\nbackup_env_file " + thorFake + " runner-secrets.env\n"
	stdout, stderr, code := runSnippet(t, c, snippet)
	if code != 0 {
		t.Fatalf("backup_env_file exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if backups := backupsOf(t, path); len(backups) > 10 {
		t.Errorf("%d backups remain (%v); the lane keeps every copy of a credential file forever",
			len(backups), backups)
	}
}

// --- criterion 2: the deploy-time grant check -----------------------------

// workflowVersion is the shape GET /v1alpha1/workflows returns, cut down to
// the four fields the check reads.
type workflowVersion struct {
	WorkflowKey  string          `json:"workflow_key"`
	Version      int             `json:"version"`
	NormalizedIR json.RawMessage `json:"normalized_ir"`
	Digest       string          `json:"digest"`
	// Source is not read by the check at all. It is here because it is most
	// of what the endpoint actually returns, and the size of the answer is
	// itself a property under test -- see the large-payload case below.
	Source string `json:"source,omitempty"`
}

// ir builds a normalized_ir with one code node declaring refs, and either a
// trigger or none.
func ir(t *testing.T, onEvent string, refs ...string) json.RawMessage {
	t.Helper()
	spec := map[string]any{
		"entry": "sweep",
		"nodes": map[string]any{
			"sweep": map[string]any{
				"kind": "code",
				"operation": map[string]any{
					"image":           "python:3.12-slim",
					"environmentRefs": refs,
				},
			},
		},
	}
	if onEvent != "" {
		spec["triggers"] = []any{map[string]any{"onEvent": onEvent}}
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "nodes.culture.dev/v1alpha1",
		"kind":       "Workflow",
		"spec":       spec,
	})
	if err != nil {
		t.Fatalf("marshal normalized_ir: %v", err)
	}
	return body
}

// fakeControlPlane serves the two endpoints the check reads.
func fakeControlPlane(t *testing.T, versions []workflowVersion, schedules []map[string]any) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1alpha1/workflows", func(w http.ResponseWriter, _ *http.Request) {
		writeFixtureJSON(t, w, map[string]any{"items": versions})
	})
	mux.HandleFunc("/v1alpha1/schedules", func(w http.ResponseWriter, _ *http.Request) {
		writeFixtureJSON(t, w, map[string]any{"items": schedules})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

func writeFixtureJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode fixture: %v", err)
	}
}

// sweepRefs is the live sweep node's grant list, which is what makes the
// missing-key fixture the incident rather than an invention.
var sweepRefs = []string{
	"PR_UPKEEP_SWEEP_SOURCE_URL", "PR_UPKEEP_SWEEP_SOURCE_SHA256",
	"PR_UPKEEP_REPOSITORIES", "GITHUB_TOKEN", "SONAR_TOKEN",
	"JIRA_ACCOUNT_EMAIL", "JIRA_API_TOKEN", "NODES_API_URL", "NODES_EVENT_TOKEN",
}

// grantedHost seeds one fake host with a complete grant: the deploy-managed
// keys in runner.env and the five secrets in runner-secrets.env.
func grantedHost(t *testing.T, c *fakeCluster, host string, secrets string) {
	t.Helper()
	seedFile(t, runnerEnvPath(t, c, host),
		"NODES_API_URL=http://192.0.2.44:18080\n"+
			"PR_UPKEEP_SWEEP_SOURCE_URL=https://example.invalid/sweep.py\n"+
			"PR_UPKEEP_SWEEP_SOURCE_SHA256="+strings.Repeat("a", 64)+"\n"+
			"PR_UPKEEP_REPOSITORIES='{\"cycle\":0,\"repositories\":[]}'\n")
	seedFile(t, runnerSecretsPath(t, c, host), secrets)
}

// extraEnv is appended after NODES_API_URL, so a caller may override PATH or
// TMPDIR for it -- grantcheckfailclosed_test.go breaks one dependency of this
// lane per test, and several of them are reachable only through the
// environment.
func runGrantCheck(t *testing.T, c *fakeCluster, host, apiURL string, extraEnv ...string) (string, string, int) {
	t.Helper()
	snippet := "set -euo pipefail\n" +
		"say() { printf '==> %s\\n' \"$*\"; }\n" +
		"HOST=" + host + "\n" +
		". " + grantCheckPath(t) + "\n"
	return runSnippet(t, c, snippet, append([]string{"NODES_API_URL=" + apiURL}, extraEnv...)...)
}

// TestGrantCheckPassesWhenEveryReachableRefIsGranted is the green path, and
// also the read-only proof: a check that is allowed to run before every deploy
// must not itself be able to change either file.
func TestGrantCheckPassesWhenEveryReachableRefIsGranted(t *testing.T) {
	c := newFakeCluster(t)
	grantedHost(t, c, thorFake, fiveKeyRunnerSecrets())
	url := fakeControlPlane(t,
		[]workflowVersion{{WorkflowKey: "pr-upkeep-sweep-cycle", Version: 2,
			NormalizedIR: ir(t, "pr-upkeep.sweep.due", sweepRefs...)}},
		[]map[string]any{{"id": "sch_1", "name": "sweep", "event_name": "pr-upkeep.sweep.due", "enabled": true}})

	before := readFileString(t, runnerSecretsPath(t, c, thorFake))
	stdout, stderr, code := runGrantCheck(t, c, thorFake, url)
	if code != 0 {
		t.Fatalf("grant check refused a fully granted host (exit %d)\nstdout:\n%s\nstderr:\n%s",
			code, stdout, stderr)
	}
	if readFileString(t, runnerSecretsPath(t, c, thorFake)) != before {
		t.Errorf("the grant check modified runner-secrets.env; it must be read-only")
	}
	if backups := backupsOf(t, runnerSecretsPath(t, c, thorFake)); len(backups) != 0 {
		t.Errorf("the grant check wrote %v; a read-only lane has nothing to back up", backups)
	}
}

// TestGrantCheckFailsNamingTheMissingKey is acceptance criterion 2 and the
// incident's fix: the host is missing exactly the grant thor lost.
func TestGrantCheckFailsNamingTheMissingKey(t *testing.T) {
	c := newFakeCluster(t)
	withoutSonar := strings.ReplaceAll(fiveKeyRunnerSecrets(), "SONAR_TOKEN="+fixtureSonarToken+"\n", "")
	grantedHost(t, c, thorFake, withoutSonar)
	url := fakeControlPlane(t,
		[]workflowVersion{{WorkflowKey: "pr-upkeep-sweep-cycle", Version: 2,
			NormalizedIR: ir(t, "pr-upkeep.sweep.due", sweepRefs...)}},
		[]map[string]any{{"id": "sch_1", "name": "sweep", "event_name": "pr-upkeep.sweep.due", "enabled": true}})

	stdout, stderr, code := runGrantCheck(t, c, thorFake, url)
	if code == 0 {
		t.Fatalf("grant check passed a host missing SONAR_TOKEN\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "SONAR_TOKEN") {
		t.Errorf("the refusal does not name the missing key; stderr was:\n%s", stderr)
	}
	if !strings.Contains(stderr, "pr-upkeep-sweep-cycle") {
		t.Errorf("the refusal does not name the workflow that declares it; stderr was:\n%s", stderr)
	}
}

// TestGrantCheckIgnoresASupersededVersionsExtraRef is the scoping half. prod
// carries 104 published versions; diffing all of them flags grants nothing can
// ask for and turns the gate into noise nobody reads.
func TestGrantCheckIgnoresASupersededVersionsExtraRef(t *testing.T) {
	c := newFakeCluster(t)
	grantedHost(t, c, thorFake, fiveKeyRunnerSecrets())
	url := fakeControlPlane(t,
		[]workflowVersion{
			{WorkflowKey: "pr-upkeep-sweep-cycle", Version: 1,
				NormalizedIR: ir(t, "pr-upkeep.sweep.due", append(sweepRefs, "SUPERSEDED_ONLY_TOKEN")...)},
			{WorkflowKey: "pr-upkeep-sweep-cycle", Version: 2,
				NormalizedIR: ir(t, "pr-upkeep.sweep.due", sweepRefs...)},
			// Reachable by nothing: no trigger, and no schedule names it.
			{WorkflowKey: "hand-started-only", Version: 1,
				NormalizedIR: ir(t, "", "NEVER_REACHABLE_TOKEN")},
		},
		[]map[string]any{{"id": "sch_1", "name": "sweep", "event_name": "pr-upkeep.sweep.due", "enabled": true}})

	stdout, stderr, code := runGrantCheck(t, c, thorFake, url)
	if code != 0 {
		t.Fatalf("grant check exited %d over refs only a superseded or unreachable "+
			"version declares\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, name := range []string{"SUPERSEDED_ONLY_TOKEN", "NEVER_REACHABLE_TOKEN"} {
		if strings.Contains(stdout+stderr, name) {
			t.Errorf("%s was reported; only the latest version of a workflow the control "+
				"plane can start today is in scope", name)
		}
	}
}

// TestGrantCheckPrintsNoGrantValueOnAnyPath is acceptance criterion 4. The
// refusal is meant to be pasteable into an issue, so the check may print key
// NAMES and never a value -- on the passing path and the failing one alike.
func TestGrantCheckPrintsNoGrantValueOnAnyPath(t *testing.T) {
	values := []string{fixtureGitHubToken, fixtureSonarToken, fixtureEventToken, fixtureJiraToken, fixtureJiraEmail}
	for _, tc := range []struct {
		name    string
		secrets string
	}{
		{"every ref granted", fiveKeyRunnerSecrets()},
		{"one ref missing", strings.ReplaceAll(fiveKeyRunnerSecrets(), "SONAR_TOKEN="+fixtureSonarToken+"\n", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeCluster(t)
			grantedHost(t, c, thorFake, tc.secrets)
			url := fakeControlPlane(t,
				[]workflowVersion{{WorkflowKey: "pr-upkeep-sweep-cycle", Version: 2,
					NormalizedIR: ir(t, "pr-upkeep.sweep.due", sweepRefs...)}},
				[]map[string]any{{"id": "sch_1", "name": "sweep", "event_name": "pr-upkeep.sweep.due", "enabled": true}})
			stdout, stderr, _ := runGrantCheck(t, c, thorFake, url)
			for _, value := range values {
				if strings.Contains(stdout+stderr, value) {
					t.Errorf("the grant check printed a granted VALUE (%q); it may print names only", value)
				}
			}
		})
	}
}

// TestGrantCheckSkipsAHostWithNoRunnerEnvYet -- a first deploy has no grants
// at all, and refusing it would mean a host could never be brought up.
func TestGrantCheckSkipsAHostWithNoRunnerEnvYet(t *testing.T) {
	c := newFakeCluster(t)
	c.hostHome(t, thorFake)
	url := fakeControlPlane(t,
		[]workflowVersion{{WorkflowKey: "pr-upkeep-sweep-cycle", Version: 2,
			NormalizedIR: ir(t, "pr-upkeep.sweep.due", sweepRefs...)}},
		nil)
	stdout, stderr, code := runGrantCheck(t, c, thorFake, url)
	if code != 0 {
		t.Fatalf("grant check refused a first deploy (exit %d)\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout+stderr, "first deploy") {
		t.Errorf("the skip was not announced; output was:\n%s\n%s", stdout, stderr)
	}
}

// TestGrantCheckSaysSoWhenItCannotReadTheControlPlane -- an unreachable
// control plane is a state the deploy may well be the fix for, so the check
// declines rather than blocking. What it must never do is pass silently.
func TestGrantCheckSaysSoWhenItCannotReadTheControlPlane(t *testing.T) {
	c := newFakeCluster(t)
	grantedHost(t, c, thorFake, fiveKeyRunnerSecrets())
	stdout, stderr, code := runGrantCheck(t, c, thorFake, "http://127.0.0.1:1")
	if code != 0 {
		t.Fatalf("grant check exited %d when the control plane was unreachable; it should "+
			"decline, not block\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout+stderr, "WARNING") {
		t.Errorf("an unverified deploy was not announced; output was:\n%s\n%s", stdout, stderr)
	}
}

// --- criterion 2, the fail-closed half ------------------------------------

// rawControlPlane serves two verbatim bodies. The typed fake next door can
// only produce answers this check understands, and the whole point of the
// tests below is the answers it does not.
func rawControlPlane(t *testing.T, workflows, schedules string) string {
	t.Helper()
	body := func(payload string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, payload); err != nil {
				t.Errorf("write fixture body: %v", err)
			}
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1alpha1/workflows", body(workflows))
	mux.HandleFunc("/v1alpha1/schedules", body(schedules))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

// grantCheckGreenLine is the one sentence in this lane that asserts the diff
// actually happened. No path that skipped a declaration may print it.
const grantCheckGreenLine = "every environment ref a startable workflow declares is granted"

// TestGrantCheckRefusesACurrentVersionItCannotRead -- the fail-open the gate
// was built to close, one level up. A normalized_ir that will not parse used
// to be coerced to {}, which put the workflow out of scope, which printed the
// green line: the check would announce that a fully granted host was fine
// while never having read the declaration that says otherwise.
func TestGrantCheckRefusesACurrentVersionItCannotRead(t *testing.T) {
	c := newFakeCluster(t)
	grantedHost(t, c, thorFake, fiveKeyRunnerSecrets())
	url := fakeControlPlane(t,
		[]workflowVersion{{WorkflowKey: "pr-upkeep-sweep-cycle", Version: 2,
			NormalizedIR: json.RawMessage(`"{ this is not the IR"`)}},
		[]map[string]any{{"id": "sch_1", "name": "sweep", "event_name": "pr-upkeep.sweep.due", "enabled": true}})

	stdout, stderr, code := runGrantCheck(t, c, thorFake, url)
	if code == 0 {
		t.Fatalf("grant check passed a control plane whose current workflow version it could "+
			"not read\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "pr-upkeep-sweep-cycle") {
		t.Errorf("the refusal does not name the version it could not read; stderr was:\n%s", stderr)
	}
	if strings.Contains(stdout+stderr, grantCheckGreenLine) {
		t.Errorf("the check claimed the grants were diffed after skipping a declaration; "+
			"output was:\n%s\n%s", stdout, stderr)
	}
}

// TestGrantCheckRefusesAnAnswerItCannotRead -- same defect at the document
// level. Every one of these bodies used to reduce to "zero workflows in
// scope", which is indistinguishable in the report from a control plane that
// has published nothing.
func TestGrantCheckRefusesAnAnswerItCannotRead(t *testing.T) {
	const goodSchedules = `{"items":[{"id":"sch_1","event_name":"pr-upkeep.sweep.due","enabled":true}]}`
	for _, tc := range []struct {
		name      string
		workflows string
		schedules string
	}{
		{"an empty body", "", goodSchedules},
		{"a body that is not JSON", "<html>502 Bad Gateway</html>", goodSchedules},
		{"a body that is not an object", `[{"workflow_key":"pr-upkeep-sweep-cycle"}]`, goodSchedules},
		{"an items that is not a list", `{"items":{"pr-upkeep-sweep-cycle":{}}}`, goodSchedules},
		{"unreadable schedules", `{"items":[]}`, `{"items":"pr-upkeep.sweep.due"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeCluster(t)
			grantedHost(t, c, thorFake, fiveKeyRunnerSecrets())
			url := rawControlPlane(t, tc.workflows, tc.schedules)

			stdout, stderr, code := runGrantCheck(t, c, thorFake, url)
			if code == 0 {
				t.Fatalf("grant check passed on %s\nstdout:\n%s\nstderr:\n%s", tc.name, stdout, stderr)
			}
			if strings.Contains(stdout+stderr, grantCheckGreenLine) {
				t.Errorf("the check claimed the grants were diffed after failing to read the "+
					"answer; output was:\n%s\n%s", stdout, stderr)
			}
		})
	}
}

// TestGrantCheckPassesAControlPlaneWithNothingPublished -- the other side of
// failing closed, and the reason it is not simply "refuse anything unusual".
// Go marshals an empty slice as null, so `{"items":null}` is the ordinary way
// a control plane says it has published nothing. That is an answer this check
// read and understood, not one it failed on.
func TestGrantCheckPassesAControlPlaneWithNothingPublished(t *testing.T) {
	c := newFakeCluster(t)
	grantedHost(t, c, thorFake, fiveKeyRunnerSecrets())
	url := rawControlPlane(t, `{"items":null}`, `{"items":null}`)

	stdout, stderr, code := runGrantCheck(t, c, thorFake, url)
	if code != 0 {
		t.Fatalf("grant check refused a control plane that has published nothing (exit %d)\n"+
			"stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "scope: 0 workflow version(s)") {
		t.Errorf("an empty scope was not reported as such; stdout was:\n%s", stdout)
	}
}

// TestGrantCheckIgnoresASupersededVersionItCannotRead -- failing closed must
// not widen the scope. prod carries ~104 published versions and most are
// superseded; refusing a deploy over the IR of a version nothing can start is
// the cried wolf the scoping rule exists to prevent.
func TestGrantCheckIgnoresASupersededVersionItCannotRead(t *testing.T) {
	c := newFakeCluster(t)
	grantedHost(t, c, thorFake, fiveKeyRunnerSecrets())
	url := fakeControlPlane(t,
		[]workflowVersion{
			{WorkflowKey: "pr-upkeep-sweep-cycle", Version: 1,
				NormalizedIR: json.RawMessage(`"{ this is not the IR"`)},
			{WorkflowKey: "pr-upkeep-sweep-cycle", Version: 2,
				NormalizedIR: ir(t, "pr-upkeep.sweep.due", sweepRefs...)},
		},
		[]map[string]any{{"id": "sch_1", "name": "sweep", "event_name": "pr-upkeep.sweep.due", "enabled": true}})

	stdout, stderr, code := runGrantCheck(t, c, thorFake, url)
	if code != 0 {
		t.Fatalf("grant check refused a deploy over a superseded version's unreadable IR "+
			"(exit %d)\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, grantCheckGreenLine) {
		t.Errorf("the current version was readable and fully granted, so the check owed a "+
			"verdict; stdout was:\n%s", stdout)
	}
}

// TestGrantCheckDiffsAnAnswerTooLargeToPassAsAnEnvironmentValue -- the fail
// open that mattered most, because it was the only one production ever hit.
// prod publishes ~104 workflow versions and each carries its whole source, so
// GET /v1alpha1/workflows?limit=500 answers with megabytes. Handing that to
// python3 as an environment value exceeds the exec argument limit; the shell
// says `Argument list too long`, the reader never starts, and the lane used to
// call that "UNVERIFIED, proceeding" -- so on the one control plane this gate
// exists to guard, it had never diffed a single grant. The host below is
// missing exactly the key the incident lost, and the check has to say so.
func TestGrantCheckDiffsAnAnswerTooLargeToPassAsAnEnvironmentValue(t *testing.T) {
	c := newFakeCluster(t)
	withoutSonar := strings.ReplaceAll(fiveKeyRunnerSecrets(), "SONAR_TOKEN="+fixtureSonarToken+"\n", "")
	grantedHost(t, c, thorFake, withoutSonar)

	// ~4 MB, comfortably past a Linux ARG_MAX of 2 MB and every smaller one.
	filler := strings.Repeat("# a published workflow source line\n", 1000)
	versions := []workflowVersion{{WorkflowKey: "pr-upkeep-sweep-cycle", Version: 2,
		NormalizedIR: ir(t, "pr-upkeep.sweep.due", sweepRefs...), Source: filler}}
	for i := 0; i < 120; i++ {
		versions = append(versions, workflowVersion{
			WorkflowKey:  fmt.Sprintf("superseded-%d", i),
			Version:      1,
			NormalizedIR: ir(t, "", "IRRELEVANT_TOKEN"),
			Source:       filler,
		})
	}
	url := fakeControlPlane(t, versions,
		[]map[string]any{{"id": "sch_1", "name": "sweep", "event_name": "pr-upkeep.sweep.due", "enabled": true}})

	stdout, stderr, code := runGrantCheck(t, c, thorFake, url)
	if strings.Contains(stdout+stderr, "Argument list too long") {
		t.Fatalf("the reader was handed the answer as an exec argument; it must be read from a "+
			"file\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if code == 0 {
		t.Fatalf("grant check passed a host missing SONAR_TOKEN when the answer was large\n"+
			"stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "SONAR_TOKEN") {
		t.Errorf("the refusal does not name the missing key; stderr was:\n%s", stderr)
	}
}

// --- the two wiring guards ------------------------------------------------

// TestDeploySourcesTheGrantCheckBeforeItShipsAnything -- the check is worth
// nothing if it runs after the archive, the image build and the stack stop.
func TestDeploySourcesTheGrantCheckBeforeItShipsAnything(t *testing.T) {
	script := readLane(t, filepath.Join(deployProdDir(t), "deploy.sh"))
	source := `source "$SCRIPT_DIR/lanes/grant-check.sh"`
	at := strings.Index(script, source)
	if at < 0 {
		t.Fatalf("deploy.sh does not source lanes/grant-check.sh")
	}
	ship := strings.Index(script, "git archive --format=tar")
	if ship < 0 || at > ship {
		t.Errorf("deploy.sh sources the grant check after it ships the archive; a refusal "+
			"must come while there is still nothing to undo (grant check at %d, ship at %d)", at, ship)
	}
}

// TestGrantCheckKnowsEveryKeyTheRunnerEnvLaneWritesItself is the guard that
// keeps the check from failing its own deploy. The runner.env lane rewrites
// that file LATER in the same run, so on the deploy that first introduces a
// new deploy-managed key the host does not have it yet -- and the check would
// refuse a deploy that was about to grant it. The names the lane writes are
// therefore declared in grant-check.sh, and this test derives the same list
// from the lane so the declaration cannot fall behind it.
func TestGrantCheckKnowsEveryKeyTheRunnerEnvLaneWritesItself(t *testing.T) {
	declared := map[string]bool{}
	check := readLane(t, grantCheckPath(t))
	const marker = "GRANT_CHECK_DEPLOY_GRANTS='"
	at := strings.Index(check, marker)
	if at < 0 {
		t.Fatalf("grant-check.sh declares no GRANT_CHECK_DEPLOY_GRANTS")
	}
	rest := check[at+len(marker):]
	for _, name := range strings.Fields(rest[:strings.Index(rest, "'")]) {
		declared[name] = true
	}
	for _, name := range runnerEnvKeysWritten(t) {
		if !declared[name] {
			t.Errorf("lanes/runner-env-write.sh writes %s but grant-check.sh does not list it in "+
				"GRANT_CHECK_DEPLOY_GRANTS; the deploy that first grants it would be refused by "+
				"its own preflight", name)
		}
	}
}

// runnerEnvKeysWritten reads the names out of the lane's one `printf | ssh`
// payload: a bare `NAME=` literal, or a `"$NAME_LINE"` the lane assembled
// above.
func runnerEnvKeysWritten(t *testing.T) []string {
	t.Helper()
	lane := readLane(t, runnerEnvLanePath(t))
	start := strings.Index(lane, "{ printf '%s\\n' \\")
	if start < 0 {
		t.Fatalf("cannot find the runner.env payload block in lanes/runner-env-write.sh")
	}
	end := strings.Index(lane[start:], "\n\t} | ssh")
	if end < 0 {
		t.Fatalf("cannot find the end of the runner.env payload block")
	}
	var names []string
	for _, line := range strings.Split(lane[start:start+end], "\n") {
		field := strings.Trim(strings.TrimSpace(line), "\\ \t'\"")
		switch {
		case strings.HasPrefix(field, "$") && strings.HasSuffix(field, "_LINE"):
			names = append(names, strings.TrimSuffix(strings.TrimPrefix(field, "$"), "_LINE"))
		case strings.Contains(field, "="):
			if name := field[:strings.Index(field, "=")]; name != "" && name == strings.ToUpper(name) {
				names = append(names, name)
			}
		}
	}
	if len(names) < 5 {
		t.Fatalf("only %d key names parsed out of the runner.env payload (%v); the parser has "+
			"drifted from the lane and is no longer guarding anything", len(names), names)
	}
	return names
}

// TestReadmeDocumentsTheFiveRunnerGrantsAndTheRollback -- criterion 6. An
// operator who has just seen this refusal needs one place that says which file
// each grant lives in and how to put the previous bytes back.
func TestReadmeDocumentsTheFiveRunnerGrantsAndTheRollback(t *testing.T) {
	readme := readLane(t, filepath.Join(deployProdDir(t), "README.md"))
	for _, want := range []string{
		"PR_UPKEEP_", "JIRA_ACCOUNT_EMAIL", "JIRA_API_TOKEN",
		"GITHUB_TOKEN", "SONAR_TOKEN", "NODES_EVENT_TOKEN",
		"runner-secrets.env", "runner.env", ".bak-",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("deploy/prod/README.md does not mention %q", want)
		}
	}
}
