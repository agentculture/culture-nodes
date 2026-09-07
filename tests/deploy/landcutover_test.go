// Task t5 (loop-closure-claude-codex, spec c13/c38, issue #315): the
// culture-land engine account and its two credentials.
//
// The land node is deterministic code the runner executes AS culture-land:
// it fetches a handover ref, rebases, runs the gate chain, pushes to the PR
// branch with the Contents-write bridge-push.env and replies on the thread
// with a SEPARATE pull-requests:write token. It runs no bridge and no engine
// binary, and it never merges a PR (human-merges-pr stays the only merge
// path, by node design -- the token can technically call the merge API).
//
// Everything here runs against FAKE hosts through shims on PATH, the same
// harness tests/test_deploy_cutover.py uses for the qwen/pi/colleague lanes:
// `ssh` maps culture-<engine>@<host> to a fake home under a per-host root
// and runs the "remote" command there (exit 255 when the account was never
// bootstrapped), every side-effecting tool appends one line to a shared
// log, and the hosts are named thor-fake / orin-fake so a test can never
// reach a real machine. `sudo` and a recording deploy are ON the PATH so
// their absence from the log is a fact rather than a gap in the harness.
//
// What t5's acceptance names, and this file checks:
//
//   - `cutover.sh thor land --dry-run` prints secrets -> deploy -> register,
//     never calls bootstrap-accounts.sh or sudo, and leaves the shim log
//     EMPTY;
//   - a real run under the shims delivers bridge-push.env AND land-pr.env
//     into the account, both mode 0600, and register-actor.sh is called with
//     os_user=culture-land and handover_remote metadata;
//   - install-secrets.sh did not grow (999 lines): the new logic lives in
//     lanes/land-secrets.sh, sourced from it;
//   - the README documents the two token scopes and states the land node
//     never merges, and the root bootstrap stays a counted hand-turn.
package deploytest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	landThorFake = "thor-fake"
	landOrinFake = "orin-fake"
)

// The shims. Each is the bash the python harness writes, so the two suites
// exercise cutover.sh against the same fake fleet.
const landSSHShim = `#!/usr/bin/env bash
while [ "$1" = -o ]; do shift 2; done
target=$1; shift
printf 'ssh[%s] %s\n' "$target" "$*" >> "$FAKE_LOG"
case "$target" in
  *@*)
    user=${target%@*}; host=${target#*@}
    [ "$host" = localhost ] && host=$FAKE_LOCAL_HOST
    home="$FAKE_HOSTS/$host/home/$user"
    ;;
  *)
    host=$target; user=${host%-fake}
    home="$FAKE_HOSTS/$host/home/$user"
    ;;
esac
[ -d "$home" ] || exit 255
cd "$home" || exit 255
exec env FAKE_HOST="$host" FAKE_USER="$user" HOME="$home" bash -c "$*"
`

const landGetentShim = `#!/usr/bin/env bash
[ "$1" = hosts ] || exit 1
printf '192.168.1.5 %s\n' "$2"
`

const landCurlShim = `#!/usr/bin/env bash
printf 'curl %s\n' "$*" >> "$FAKE_LOG"
exit 22
`

const landOpensslShim = `#!/usr/bin/env bash
printf 'openssl %s\n' "$*" >> "$FAKE_LOG"
printf 'fake-minted-token-value\n'
`

const landSudoShim = `#!/usr/bin/env bash
printf 'sudo %s\n' "$*" >> "$FAKE_LOG"
exit 0
`

const landDeployShim = `#!/usr/bin/env bash
printf 'deploy %s\n' "$*" >> "$FAKE_LOG"
exit 0
`

// A stateful psql for register-actor.sh: it stores what an INSERT wrote and
// answers the next SELECT from it, so the script's own append-only
// idempotency (unchanged endpoint + metadata -> no INSERT) is what a second
// run exercises, rather than a canned answer.
const landPsqlShim = `#!/usr/bin/env python3
import json
import os
import re
import sys

query = sys.argv[-1]
with open(os.environ["FAKE_LOG"], "a") as handle:
    handle.write("psql %s\n" % query.split()[0])
state = os.environ["FAKE_ACTOR_STATE"]
overlay_match = re.search(r"'(\{.*?\})'::jsonb", query)
overlay = overlay_match.group(1) if overlay_match else ""
if "INSERT INTO actors" in query:
    endpoint = re.search(r"'(https?://[^']+)'", query)
    with open(state, "w") as handle:
        json.dump({"endpoint": endpoint.group(1) if endpoint else "", "overlay": overlay, "query": query}, handle)
elif "FROM actors" in query and os.path.exists(state):
    with open(state) as handle:
        row = json.load(handle)
    sys.stdout.write("1|%s|%s" % (row["endpoint"], "t" if row["overlay"] == overlay else "f"))
`

type landHarness struct {
	t          *testing.T
	root       string
	log        string
	hosts      string
	bin        string
	actorState string
	operator   string
}

func newLandHarness(t *testing.T) *landHarness {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not on PATH; the stateful fake psql needs it")
	}
	root := t.TempDir()
	h := &landHarness{
		t:          t,
		root:       root,
		log:        filepath.Join(root, "calls.log"),
		hosts:      filepath.Join(root, "hosts"),
		bin:        filepath.Join(root, "bin"),
		actorState: filepath.Join(root, "actor.state"),
		operator:   filepath.Join(root, "operator"),
	}
	for _, dir := range []string{h.bin, h.operator} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(h.log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"ssh":         landSSHShim,
		"getent":      landGetentShim,
		"curl":        landCurlShim,
		"openssl":     landOpensslShim,
		"sudo":        landSudoShim,
		"fake-deploy": landDeployShim,
		"fake-psql":   landPsqlShim,
	} {
		if err := os.WriteFile(filepath.Join(h.bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The login users' homes, so `ssh thor-fake` (no account) resolves.
	for _, host := range []string{landThorFake, landOrinFake} {
		login := strings.TrimSuffix(host, "-fake")
		if err := os.MkdirAll(filepath.Join(h.hosts, host, "home", login), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// accountHome is where the fake ssh lands for culture-<engine>@<host>.
func (h *landHarness) accountHome(host, engine string) string {
	return filepath.Join(h.hosts, host, "home", "culture-"+engine)
}

// bootstrap is what the ROOT hand-turn leaves behind: an account with a home.
func (h *landHarness) bootstrap(host, engine string) string {
	home := h.accountHome(host, engine)
	if err := os.MkdirAll(filepath.Join(home, ".culture-nodes"), 0o700); err != nil {
		h.t.Fatal(err)
	}
	return home
}

func (h *landHarness) env(extra ...string) []string {
	env := []string{
		"PATH=" + h.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + h.operator,
		"FAKE_LOG=" + h.log,
		"FAKE_HOSTS=" + h.hosts,
		"FAKE_LOCAL_HOST=spark-fake",
		"FAKE_ACTOR_STATE=" + h.actorState,
		"CUTOVER_DEPLOY_CMD=" + filepath.Join(h.bin, "fake-deploy"),
		"PSQL_CMD=" + filepath.Join(h.bin, "fake-psql"),
		"NODES_NAMESPACE_ID=namespace_1",
	}
	return append(env, extra...)
}

// run executes the real cutover.sh under the shims and returns stdout,
// stderr and the exit code separately: the script's contract is results on
// stdout and diagnostics on stderr, never mixed, and the assertions below
// lean on that split.
func (h *landHarness) run(extraEnv []string, args ...string) (stdout, stderr string, code int) {
	h.t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(prodComposeDir(h.t), "cutover.sh")}, args...)...)
	cmd.Env = h.env(extraEnv...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code = 0
	if err != nil {
		var exitErr *exec.ExitError
		if !asExitError(err, &exitErr) {
			h.t.Fatalf("run cutover.sh: %v", err)
		}
		code = exitErr.ExitCode()
	}
	return out.String(), errb.String(), code
}

func (h *landHarness) calls() []string {
	raw, err := os.ReadFile(h.log)
	if err != nil {
		h.t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func (h *landHarness) resetLog() {
	if err := os.WriteFile(h.log, nil, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// landSteps maps step name -> verdict from `step <name>: <verdict> — ...`.
func landSteps(stdout string) (order []string, verdicts map[string]string) {
	verdicts = map[string]string{}
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(line, "step ") {
			continue
		}
		rest := strings.TrimPrefix(line, "step ")
		name, after, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		verdict := strings.Fields(strings.TrimSpace(after))[0]
		order = append(order, strings.TrimSpace(name))
		verdicts[strings.TrimSpace(name)] = verdict
	}
	return order, verdicts
}

var landStepOrder = []string{"account-exists", "compose-declares-token-key", "secrets", "deploy", "register"}

func indexOfCall(calls []string, needle string) int {
	for i, line := range calls {
		if strings.Contains(line, needle) {
			return i
		}
	}
	return -1
}

// readProdFile (claudetokenplacement_test.go) reads a deploy/prod file by
// relative name; the static tests below reuse it.

// --- dry run --------------------------------------------------------------

func TestLandCutoverDryRunPrintsSecretsDeployRegisterAndTouchesNothing(t *testing.T) {
	h := newLandHarness(t)
	h.bootstrap(landThorFake, "land")

	stdout, stderr, code := h.run(nil, landThorFake, "land", "--dry-run")
	if code != 0 {
		t.Fatalf("dry run exit=%d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	order, verdicts := landSteps(stdout)
	if strings.Join(order, ",") != strings.Join(landStepOrder, ",") {
		t.Errorf("step order = %v, want %v\n%s", order, landStepOrder, stdout)
	}
	for name, verdict := range verdicts {
		if !strings.HasPrefix(verdict, "would-") {
			t.Errorf("step %s verdict %q under --dry-run; every step must be would-*", name, verdict)
		}
	}
	// The land account runs no bridge, so there is no compose token key to
	// declare: the step is a would-skip that says so, not a would-refuse.
	if verdicts["compose-declares-token-key"] != "would-skip" {
		t.Errorf("compose-declares-token-key = %q, want would-skip (land runs no bridge)", verdicts["compose-declares-token-key"])
	}
	// The exact commands, not a paraphrase: this is what the operator reads
	// before the first real run.
	for _, want := range []string{
		"install_land_account_env thor-fake",
		"bridge-push.env",
		"land-pr.env",
		"fake-deploy thor-fake",
		"register-actor.sh --runner-account company/land-thor",
		"--os-user culture-land",
		"--metadata handover_remote=ssh://culture-land@thor/home/culture-land/git/culture-nodes-land",
		"--metadata repository_identity=agentculture/culture-nodes",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run stdout lacks %q\n%s", want, stdout)
		}
	}
	for _, bad := range []string{"--metadata model=", "--metadata harness=", "NODES_ACTOR_LAND"} {
		if strings.Contains(stdout, bad) {
			t.Errorf("dry-run stdout carries %q; the land actor has no model, no harness tag and no bridge token", bad)
		}
	}
	if calls := h.calls(); len(calls) != 0 {
		t.Errorf("a dry run left side effects in the shim log: %v", calls)
	}
	if _, err := os.Stat(h.actorState); !os.IsNotExist(err) {
		t.Error("a dry run wrote an actor row")
	}
}

func TestLandCutoverIsRefusedOnSpark(t *testing.T) {
	h := newLandHarness(t)
	_, stderr, code := h.run(nil, "spark-fake", "land", "--dry-run")
	if code == 0 {
		t.Fatal("cutover.sh spark land was accepted; the runner host is thor/orin and spark has no land lane")
	}
	if !strings.Contains(stderr, "hint:") {
		t.Errorf("refusal carries no hint: line\n%s", stderr)
	}
	if calls := h.calls(); len(calls) != 0 {
		t.Errorf("a refused invocation touched a host: %v", calls)
	}
}

// --- the real run -----------------------------------------------------------

func TestLandCutoverDeliversBothCredentialFilesAndRegistersTheAccount(t *testing.T) {
	h := newLandHarness(t)
	home := h.bootstrap(landThorFake, "land")
	tokens := []string{"GITHUB_TOKEN_WORKER=ghp-contents-write-token", "GITHUB_TOKEN_LAND_PR=ghp-pull-requests-write-token"}

	stdout, stderr, code := h.run(tokens, landThorFake, "land", "--yes")
	if code != 0 {
		t.Fatalf("real run exit=%d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	order, verdicts := landSteps(stdout)
	if strings.Join(order, ",") != strings.Join(landStepOrder, ",") {
		t.Errorf("step order = %v, want %v", order, landStepOrder)
	}
	for _, name := range []string{"secrets", "deploy", "register"} {
		if verdicts[name] != "run" {
			t.Errorf("step %s = %q, want run", name, verdicts[name])
		}
	}

	// The two credential files landed in the ACCOUNT, mode 600, each carrying
	// exactly its own token: the Contents-write push credential and the
	// pull-requests:write reply credential are never aliased.
	for file, want := range map[string]string{
		"bridge-push.env": "GITHUB_TOKEN_WORKER=ghp-contents-write-token\n",
		"land-pr.env":     "GITHUB_TOKEN_LAND_PR=ghp-pull-requests-write-token\n",
	} {
		path := filepath.Join(home, ".culture-nodes", file)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s was not delivered: %v", file, err)
			continue
		}
		if string(raw) != want {
			t.Errorf("%s = %q, want %q", file, raw, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s is mode %o, want 600", file, mode)
		}
	}

	// The order the acceptance names, read off the shared log.
	calls := h.calls()
	secrets := indexOfCall(calls, "land-pr.env")
	deployed := indexOfCall(calls, "deploy thor-fake")
	registered := indexOfCall(calls, "psql INSERT")
	if secrets < 0 || deployed < 0 || registered < 0 || !(secrets < deployed && deployed < registered) {
		t.Errorf("expected secrets < deploy < register in the log, got secrets=%d deploy=%d register=%d:\n%s",
			secrets, deployed, registered, strings.Join(calls, "\n"))
	}
	// No token ever rides an ssh argv: the shim logs the remote command
	// string, so a token in it would appear here.
	for _, line := range calls {
		if strings.Contains(line, "ghp-") {
			t.Errorf("a credential value appeared in a logged argv: %s", line)
		}
		if strings.HasPrefix(line, "sudo ") {
			t.Errorf("cutover.sh reached for sudo: %s", line)
		}
		if strings.Contains(line, "bootstrap-accounts") {
			t.Errorf("cutover.sh ran the root bootstrap: %s", line)
		}
	}

	// The registration: os_user=culture-land, handover_remote, an endpoint-
	// less agent row the runner executes for -- never a bridge endpoint.
	raw, err := os.ReadFile(h.actorState)
	if err != nil {
		t.Fatalf("register-actor.sh wrote no row: %v", err)
	}
	var row struct {
		Endpoint string `json:"endpoint"`
		Overlay  string `json:"overlay"`
		Query    string `json:"query"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	if row.Endpoint != "" {
		t.Errorf("land actor registered with endpoint %q; it runs no bridge", row.Endpoint)
	}
	overlay := map[string]string{}
	if err := json.Unmarshal([]byte(row.Overlay), &overlay); err != nil {
		t.Fatalf("overlay %q is not JSON: %v", row.Overlay, err)
	}
	for key, want := range map[string]string{
		"os_user":             "culture-land",
		"handover_remote":     "ssh://culture-land@thor/home/culture-land/git/culture-nodes-land",
		"repository_identity": "agentculture/culture-nodes",
	} {
		if overlay[key] != want {
			t.Errorf("metadata %s = %q, want %q", key, overlay[key], want)
		}
	}
	if _, has := overlay["auth_token_env"]; has {
		t.Error("land actor carries auth_token_env; there is no bridge for the worker to authenticate to")
	}
	if !strings.Contains(row.Query, "'agent', 'runner', NULL") {
		t.Errorf("land row is not an endpoint-less agent/runner row: %s", row.Query)
	}
	if !strings.Contains(stdout, "company/land-thor") {
		t.Errorf("success line does not name the actor\n%s", stdout)
	}
}

func TestLandCutoverRefusesToRunSecretsWithoutBothTokens(t *testing.T) {
	h := newLandHarness(t)
	h.bootstrap(landThorFake, "land")

	// Only the push token: the reply credential is missing, and a cutover
	// that "ran secrets" and delivered one file is the half-success shape
	// #300 records.
	stdout, stderr, code := h.run([]string{"GITHUB_TOKEN_WORKER=ghp-x"}, landThorFake, "land", "--yes")
	if code == 0 {
		t.Fatalf("cutover ran with GITHUB_TOKEN_LAND_PR unset\n%s", stdout)
	}
	if !strings.Contains(stdout, "step secrets: refuse") {
		t.Errorf("secrets step did not refuse\n%s", stdout)
	}
	if !strings.Contains(stderr, "GITHUB_TOKEN_LAND_PR") || !strings.Contains(stderr, "hint:") {
		t.Errorf("refusal does not name the missing variable with a hint\n%s", stderr)
	}
	calls := h.calls()
	if indexOfCall(calls, "deploy ") >= 0 || indexOfCall(calls, "psql") >= 0 {
		t.Errorf("deploy or register ran after a refused secrets step: %v", calls)
	}
	if _, err := os.Stat(filepath.Join(h.accountHome(landThorFake, "land"), ".culture-nodes", "bridge-push.env")); !os.IsNotExist(err) {
		t.Error("a refused secrets step still delivered bridge-push.env")
	}
}

func TestLandCutoverSecondRunSkipsSecretsAndRegistration(t *testing.T) {
	h := newLandHarness(t)
	h.bootstrap(landThorFake, "land")
	tokens := []string{"GITHUB_TOKEN_WORKER=ghp-a", "GITHUB_TOKEN_LAND_PR=ghp-b"}
	if stdout, stderr, code := h.run(tokens, landThorFake, "land", "--yes"); code != 0 {
		t.Fatalf("first run exit=%d\n%s\n%s", code, stdout, stderr)
	}
	h.resetLog()

	// No tokens in the environment this time: the files are present, so the
	// step is a skip, not a refusal -- a re-run resumes rather than repeats.
	stdout, stderr, code := h.run(nil, landThorFake, "land", "--yes")
	if code != 0 {
		t.Fatalf("second run exit=%d\n%s\n%s", code, stdout, stderr)
	}
	_, verdicts := landSteps(stdout)
	if verdicts["secrets"] != "skip" {
		t.Errorf("second run secrets = %q, want skip", verdicts["secrets"])
	}
	if verdicts["register"] != "skip" || !strings.Contains(stdout, "unchanged") {
		t.Errorf("second run register = %q (want skip via register-actor's own 'unchanged')\n%s", verdicts["register"], stdout)
	}
	if indexOfCall(h.calls(), "psql INSERT") >= 0 {
		t.Error("second run inserted another actor revision")
	}
}

func TestLandCutoverForceRelaysTheTokensAgain(t *testing.T) {
	h := newLandHarness(t)
	home := h.bootstrap(landThorFake, "land")
	if _, stderr, code := h.run([]string{"GITHUB_TOKEN_WORKER=ghp-a", "GITHUB_TOKEN_LAND_PR=ghp-b"}, landThorFake, "land", "--yes"); code != 0 {
		t.Fatalf("first run failed: %s", stderr)
	}
	stdout, stderr, code := h.run([]string{"GITHUB_TOKEN_WORKER=ghp-c", "GITHUB_TOKEN_LAND_PR=ghp-d", "FORCE_LAND=1"}, landThorFake, "land", "--yes")
	if code != 0 {
		t.Fatalf("forced run exit=%d\n%s\n%s", code, stdout, stderr)
	}
	if _, verdicts := landSteps(stdout); verdicts["secrets"] != "run" {
		t.Errorf("FORCE_LAND=1 secrets = %q, want run", verdicts["secrets"])
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".culture-nodes", "land-pr.env"))
	if string(raw) != "GITHUB_TOKEN_LAND_PR=ghp-d\n" {
		t.Errorf("land-pr.env after FORCE_LAND=1 = %q", raw)
	}
}

// --- the unbootstrapped account is the hand-turn, named ---------------------

func TestLandCutoverNamesTheRootHandTurnWhenTheAccountIsMissing(t *testing.T) {
	h := newLandHarness(t) // no culture-land on thor-fake
	stdout, stderr, code := h.run([]string{"GITHUB_TOKEN_WORKER=a", "GITHUB_TOKEN_LAND_PR=b"}, landThorFake, "land", "--yes")
	if code == 0 {
		t.Fatal("cutover succeeded against an account that does not exist")
	}
	if !strings.Contains(stdout, "step account-exists: refuse") || !strings.Contains(stdout, "culture-land@thor-fake") {
		t.Errorf("refusal does not name the account\n%s", stdout)
	}
	if !strings.Contains(stderr, "sudo bash deploy/prod/lanes/unix-user.sh bootstrap land") {
		t.Errorf("refusal does not print the hand-typed bootstrap for land\n%s", stderr)
	}
	for _, line := range h.calls() {
		if strings.HasPrefix(line, "sudo ") || strings.HasPrefix(line, "deploy ") {
			t.Errorf("cutover acted after the account refusal: %s", line)
		}
	}
}

// --- static: the lane, the account lane, the line budget -------------------

func TestLandSecretsLaneIsRelayedStdinOnlyAndSourcedByInstallSecrets(t *testing.T) {
	lane := readProdFile(t, filepath.Join("lanes", "land-secrets.sh"))
	if !strings.Contains(lane, "install_land_account_env() {") {
		t.Fatal("lanes/land-secrets.sh does not define install_land_account_env")
	}
	for _, want := range []string{"bridge-push.env", "land-pr.env", "GITHUB_TOKEN_WORKER", "GITHUB_TOKEN_LAND_PR"} {
		if !strings.Contains(lane, want) {
			t.Errorf("land-secrets.sh never names %s", want)
		}
	}
	if strings.Count(lane, "chmod 600") < 2 {
		t.Error("land-secrets.sh does not chmod 600 both credential files")
	}
	if strings.Contains(lane, "openssl") {
		t.Error("land-secrets.sh mints a credential; both tokens are externally issued and relayed")
	}
	// Token values cross to the account on ssh STDIN, never in the remote
	// command string: a `ssh ... "$GITHUB_TOKEN_..."` would put the secret
	// in the process table of both machines.
	if regexp.MustCompile(`ssh[^\n|]*\$\{?GITHUB_TOKEN`).MatchString(lane) {
		t.Error("land-secrets.sh interpolates a token into an ssh argv")
	}
	for _, alias := range []string{"GITHUB_TOKEN_LAND_PR=${GITHUB_TOKEN_WORKER}", "GITHUB_TOKEN_WORKER=${GITHUB_TOKEN_LAND_PR}", "GITHUB_TOKEN_LAND_PR=$GITHUB_TOKEN_WORKER"} {
		if strings.Contains(lane, alias) {
			t.Errorf("the push and reply credentials are aliased: %s", alias)
		}
	}
	if strings.Contains(lane, "sudo") {
		t.Error("land-secrets.sh reaches for sudo")
	}

	secrets := readInstallSecrets(t)
	if !strings.Contains(secrets, `lanes/land-secrets.sh"`) {
		t.Error("install-secrets.sh does not source lanes/land-secrets.sh")
	}
	if !strings.Contains(secrets, "install_land_account_env \"$THOR\"") {
		t.Error("install-secrets.sh never calls install_land_account_env for thor")
	}
	if got := countLinesOf(secrets); got != 999 {
		t.Errorf("install-secrets.sh is %d lines; it must stay at 999 (the land logic lives in lanes/land-secrets.sh)", got)
	}
}

func countLinesOf(contents string) int {
	lines := strings.Count(contents, "\n")
	if len(contents) > 0 && contents[len(contents)-1] != '\n' {
		lines++
	}
	return lines
}

func TestUnixUserLaneKnowsTheLandEngine(t *testing.T) {
	lane := readProdFile(t, filepath.Join("lanes", "unix-user.sh"))
	// The engine allowlist, the role clone and the hand-typed usage all name
	// land; the credential case gives it NO model credential file.
	for _, want := range []string{
		"codex|claude|qwen|pi|colleague|land)",
		"land) echo land ;;",
		"<codex|claude|qwen|pi|colleague|land>",
		"land) cred_dir=\"\"; cred_file=\"\" ;;",
	} {
		if !strings.Contains(lane, want) {
			t.Errorf("lanes/unix-user.sh lacks %q", want)
		}
	}
	// The inventory admits the second credential file and nothing else new.
	if !strings.Contains(lane, "bridge-push.env|land-pr.env|dialin") {
		t.Error("the account inventory does not admit land-pr.env; the provision would refuse a correctly provisioned land account")
	}

	bootstrap := readProdFile(t, "bootstrap-accounts.sh")
	if !regexp.MustCompile(`thor\) ENGINES="codex qwen pi land"`).MatchString(bootstrap) {
		t.Error("bootstrap-accounts.sh thor does not create culture-land; the root hand-turn must name it")
	}

	deploy := readProdFile(t, "deploy.sh")
	if !strings.Contains(deploy, "deploy_land_account() {") {
		t.Fatal("deploy.sh has no deploy_land_account lane")
	}
	body := deploy[strings.Index(deploy, "deploy_land_account() {"):]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "account_reachable") {
		t.Error("deploy_land_account is not additive: it must skip by name when culture-land is not bootstrapped")
	}
	if !strings.Contains(body, "account_prepare \"$host\" land") {
		t.Error("deploy_land_account does not prepare the account (checkout, git identity, inventory)")
	}
	for _, bad := range []string{"systemctl", "uv tool install", "sudo"} {
		if strings.Contains(body, bad) {
			t.Errorf("deploy_land_account runs %q; the land account has no unit, no bridge and never needs root", bad)
		}
	}
	if !strings.Contains(deploy, `deploy_land_account "$HOST"`) {
		t.Error("deploy.sh never calls deploy_land_account for the named host")
	}
}

func TestRegisterActorRunnerAccountShape(t *testing.T) {
	dir := t.TempDir()
	fake, _, inserts := newFakePsql(t, dir, "namespace_1", "")
	env := []string{"PSQL_CMD=" + fake, "NODES_NAMESPACE_ID=namespace_1"}

	// --os-user is REQUIRED: a runner-account actor IS the account it runs
	// as, so a row without the lane tag would be a row nothing can execute.
	output, code := runRegisterActorArgs(t, env, "--runner-account", "company/land-thor")
	if code == 0 {
		t.Fatalf("--runner-account without --os-user was accepted: %s", output)
	}
	if raw, _ := os.ReadFile(inserts); len(raw) != 0 {
		t.Fatalf("a refused registration inserted a row: %s", raw)
	}

	output, code = runRegisterActorArgs(t, env, "--runner-account", "company/land-thor", "--os-user", "culture-land",
		"--metadata", "handover_remote=ssh://culture-land@thor/home/culture-land/git/culture-nodes-land")
	if code != 0 {
		t.Fatalf("--runner-account registration exit=%d output=%q", code, output)
	}
	raw, err := os.ReadFile(inserts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "'agent', 'runner', NULL") {
		t.Errorf("runner-account row is not agent/runner with a NULL endpoint: %s", raw)
	}
	if !strings.Contains(string(raw), `"os_user": "culture-land"`) || !strings.Contains(string(raw), `"handover_remote"`) {
		t.Errorf("runner-account row lacks os_user / handover_remote metadata: %s", raw)
	}

	// An endpoint argument is a contradiction, refused before any SQL.
	output, code = runRegisterActorArgs(t, env, "--runner-account", "company/land-thor", "http://192.168.1.5:1", "--os-user", "culture-land")
	if code == 0 {
		t.Errorf("--runner-account accepted an endpoint: %s", output)
	}
}

// --- the README: two scopes, never a merge, one hand-turn --------------------

func TestReadmeDocumentsTheLandAccountTokenScopesAndTheNoMergeRule(t *testing.T) {
	readme := readProdFile(t, "README.md")
	start := strings.Index(readme, "### The culture-land account")
	if start < 0 {
		t.Fatal("deploy/prod/README.md has no '### The culture-land account' section")
	}
	section := readme[start:]
	if next := strings.Index(section[4:], "\n### "); next > 0 {
		section = section[:next+4]
	}
	for _, want := range []string{
		"bridge-push.env",
		"land-pr.env",
		"GITHUB_TOKEN_WORKER",
		"GITHUB_TOKEN_LAND_PR",
		"pull-requests:write",
		"never merges",
		"human-merges-pr",
		"bootstrap-accounts.sh",
		"hand-turn",
		"cutover.sh thor land",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the culture-land README section does not mention %q", want)
		}
	}
	// Contents scope for the push credential, spelled the way GitHub does.
	if !regexp.MustCompile(`(?i)contents[: ]+(write|read and write)`).MatchString(section) {
		t.Error("the culture-land README section does not name the Contents write scope of bridge-push.env")
	}
}
