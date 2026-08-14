// Package deploytest -- this file is task t11's (upkeep-actors-jira plan,
// issue #69 item 1): prod.env's rotation MERGES key-by-key instead of
// replacing the file wholesale, and a key can still be deliberately removed.
//
// The defect these tests pin, observed rather than theorised. prod.env holds
// two populations: the six secrets install-secrets.sh generates itself, and
// roughly eight more that accrete afterwards -- NODES_NAMESPACE_ID and
// THOR_IP written by deploy.sh, NODES_ACTOR_CODEX_*_TOKEN and
// NODES_ACTOR_NOTIFY_TOKEN written by this script's own later lanes, and
// NODES_ACTOR_CLAUDE_TOKEN / DISCORD_WEBHOOK_URL relayed from outside. The
// prod lane wrote the first population over the whole file, so an authorized
// FORCE=1 rotation silently deleted the second:
//
//	Aug 13 13:03  company/developer  succeeded       <- auth working
//	   ... FORCE=1 rotation rewrote prod.env ...
//	Aug 14 06:42  company/developer  policy_denied   <- 401, token gone
//
// Nobody noticed for ~18 hours, because the damage is latent: the running
// worker held the token in memory until it restarted.
//
// Unlike this package's static text assertions (codexsecrets_test.go's
// argv-discipline scans), these are BEHAVIORAL: the script is executed for
// real against a fake `ssh` that runs each remote command locally under a
// per-host HOME. Merge semantics are a property of what the file contains
// afterwards, and only running the thing proves that -- claim c41's honesty
// condition h32 asks for a rotation performed with an externally-issued key
// present, not for a grep that says it ought to survive one.
package deploytest

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// removeSecretPath locates deploy/prod/remove-secret.sh beside
// install-secrets.sh (installSecretsPath is provided by
// codexsecrets_test.go in this package).
func removeSecretPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(installSecretsPath(t)), "remove-secret.sh")
}

// --- the fake cluster -----------------------------------------------------

// fakeCluster runs deploy/prod scripts with a stub `ssh` first on PATH. The
// stub takes install-secrets.sh's own calling convention -- `ssh <host>
// <one command string>`, stdin passed through untouched -- and executes the
// command string locally with HOME pointed at a per-host directory, so
// `~/.culture-nodes/prod.env` resolves somewhere different for thor than for
// orin exactly as it would on two machines.
//
// Nothing here fakes the merge itself: the remote command string under test
// is the one the script really sends, run by a real bash.
type fakeCluster struct {
	root         string // per-host HOME parent
	operatorHome string // HOME of the invoking operator (local mirror files)
	confirmDir   string
	binDir       string
}

func newFakeCluster(t *testing.T) *fakeCluster {
	t.Helper()
	base := t.TempDir()
	c := &fakeCluster{
		root:         filepath.Join(base, "hosts"),
		operatorHome: filepath.Join(base, "operator"),
		confirmDir:   filepath.Join(base, "confirm"),
		binDir:       filepath.Join(base, "bin"),
	}
	for _, dir := range []string{c.root, c.operatorHome, c.confirmDir, c.binDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// The stub logs nothing and interprets nothing: shift off the host,
	// re-home, hand the rest to bash. `exec bash -c "$*"` keeps stdin
	// attached, which is what carries the secret material in every lane.
	stub := "#!/usr/bin/env bash\n" +
		"host=$1; shift\n" +
		"export HOME=\"$FAKE_SSH_HOME_ROOT/$host\"\n" +
		"mkdir -p \"$HOME\"\n" +
		"exec bash -c \"$*\"\n"
	if err := os.WriteFile(filepath.Join(c.binDir, "ssh"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	return c
}

func (c *fakeCluster) hostHome(t *testing.T, host string) string {
	t.Helper()
	home := filepath.Join(c.root, host)
	if err := os.MkdirAll(filepath.Join(home, ".culture-nodes"), 0o700); err != nil {
		t.Fatalf("mkdir host home %s: %v", home, err)
	}
	return home
}

func (c *fakeCluster) prodEnvPath(t *testing.T, host string) string {
	t.Helper()
	return filepath.Join(c.hostHome(t, host), ".culture-nodes", "prod.env")
}

// seedProdEnv installs a prod.env on host as if a previous deploy had left
// it there.
func (c *fakeCluster) seedProdEnv(t *testing.T, host, body string) {
	t.Helper()
	if err := os.WriteFile(c.prodEnvPath(t, host), []byte(body), 0o600); err != nil {
		t.Fatalf("seed prod.env on %s: %v", host, err)
	}
}

// confirmRotation pre-writes the destructive-confirmation file the prod lane
// consumes for host, with its verdict already edited to `rotate`.
//
// The protocol itself (refuse first, name what breaks, single-use, windowed)
// is destructiveconfirm_test.go's subject and is not re-proven here; these
// tests are about what a CONFIRMED rotation does to the file. The filename
// and the verdict line are the protocol's contract, so writing them directly
// is standing where an operator who has read the file stands.
func (c *fakeCluster) confirmRotation(t *testing.T, host string) {
	t.Helper()
	file := filepath.Join(c.confirmDir, "CONFIRM-rotate-prod-env-"+host+".md")
	body := "# Destructive action requires confirmation\n\nLane:  prod-env\nHost:  " + host + "\n\nverdict: rotate\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatalf("write confirmation for %s: %v", host, err)
	}
}

// run executes a deploy/prod script under the fake cluster and returns its
// combined output plus exit code.
func (c *fakeCluster) run(t *testing.T, script string, args []string, extraEnv ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append([]string{
		"PATH=" + c.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + c.operatorHome,
		"CONFIRM_DIR=" + c.confirmDir,
		"FAKE_SSH_HOME_ROOT=" + c.root,
		// No control plane exists in a test: the human-inbox lane must
		// resolve nothing and install nothing, which is its own documented
		// refusal (actor-placement.sh: "Failure is never a fallback").
		"NODES_API_URL=http://127.0.0.1:1",
		"NODES_API_TIMEOUT_SECONDS=1",
	}, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return string(out), exitErr.ExitCode()
	}
	t.Fatalf("run %s: %v (output: %s)", filepath.Base(script), err, out)
	return "", -1
}

// envFile is a parsed KEY=VALUE file. order keeps every key in encounter
// order INCLUDING repeats, so a merge that appends a second POSTGRES_PASSWORD
// line instead of replacing the first is detectable.
type envFile struct {
	values map[string]string
	order  []string
	raw    string
}

func readEnvFile(t *testing.T, path string) envFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	parsed := envFile{values: map[string]string{}, raw: string(raw)}
	for _, line := range strings.Split(parsed.raw, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		parsed.values[key] = value
		parsed.order = append(parsed.order, key)
	}
	if len(parsed.order) == 0 {
		t.Fatalf("%s parsed to no KEY=VALUE lines; content was %q", path, parsed.raw)
	}
	return parsed
}

func (e envFile) assertNoDuplicateKeys(t *testing.T, path string) {
	t.Helper()
	seen := map[string]int{}
	for _, key := range e.order {
		seen[key]++
	}
	for key, count := range seen {
		if count > 1 {
			t.Errorf("%s carries %d lines for %s; a merge updates a key in place, it does not append a rival assignment", path, count, key)
		}
	}
}

// accretedProdEnv is a prod.env as it really looks on a host that has been
// deployed to and had actors registered against it: the generated population
// first, then the keys other lanes and other people put there. The values are
// obvious placeholders -- the point of each is only that it is NOT something
// install-secrets.sh can regenerate.
const accretedProdEnv = `POSTGRES_USER=nodes
POSTGRES_DB=nodes
POSTGRES_PASSWORD=old-generated-postgres-password
MINIO_ROOT_USER=nodesroot
MINIO_ROOT_PASSWORD=old-generated-minio-password
NODES_HUMAN_DECISION_TOKEN_SECRET=old-generated-human-decision-secret
NODES_CALLBACK_TOKEN_SECRET=old-generated-callback-secret
NODES_CALLBACK_BASE_URL=http://thor:18080
NODES_RUNNER_SECRET=old-generated-runner-secret
NODES_NAMESPACE_ID=01JQZZZZZZZZZZZZZZZZZZZZZZ
THOR_IP=192.0.2.10
NODES_ACTOR_CLAUDE_TOKEN=externally-issued-claude-bridge-token
DISCORD_WEBHOOK_URL=https://discord.example/api/webhooks/placeholder
`

// accretedKeys are the ones NO lane of install-secrets.sh generates, so a
// rotation has no value of its own to write for any of them. Their survival
// is the whole subject of claim c41 / honesty condition h32.
var accretedKeys = map[string]string{
	"NODES_NAMESPACE_ID":       "01JQZZZZZZZZZZZZZZZZZZZZZZ",
	"THOR_IP":                  "192.0.2.10",
	"NODES_ACTOR_CLAUDE_TOKEN": "externally-issued-claude-bridge-token",
	"DISCORD_WEBHOOK_URL":      "https://discord.example/api/webhooks/placeholder",
}

// hexSecret matches what gen() produces (openssl rand -hex 32).
var hexSecret = regexp.MustCompile(`^[0-9a-f]{64}$`)

// --- criterion 1: a rotation preserves every key it does not own ----------

// TestProdEnvRotationPreservesExternallyIssuedKeys is h32 in executable form.
// A confirmed FORCE_PROD=1 rotation runs against a prod.env that already
// carries an externally-issued NODES_ACTOR_CLAUDE_TOKEN (plus the other
// accreted keys); afterwards the generated secrets must be new AND every
// accreted key must still be there, byte for byte.
func TestProdEnvRotationPreservesExternallyIssuedKeys(t *testing.T) {
	c := newFakeCluster(t)
	for _, host := range []string{"thor", "orin"} {
		c.seedProdEnv(t, host, accretedProdEnv)
		c.confirmRotation(t, host)
	}

	out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"}, "FORCE_PROD=1")
	if code != 0 {
		t.Fatalf("rotation exited %d; output:\n%s", code, out)
	}

	for _, host := range []string{"thor", "orin"} {
		path := c.prodEnvPath(t, host)
		env := readEnvFile(t, path)
		env.assertNoDuplicateKeys(t, path)
		assertAccretedKeysSurvived(t, host, env)
		assertGeneratedKeysWereRotated(t, host, env)
	}

	// Both hosts hold their OWN runner secret, not one value merged twice.
	thor := readEnvFile(t, c.prodEnvPath(t, "thor"))
	orin := readEnvFile(t, c.prodEnvPath(t, "orin"))
	if thor.values["NODES_RUNNER_SECRET"] == orin.values["NODES_RUNNER_SECRET"] {
		t.Error("thor and orin ended up with the same NODES_RUNNER_SECRET; each host's runner bearer is per-host")
	}
}

// assertAccretedKeysSurvived is the half of the rotation contract this whole
// file exists for: the keys the rotation does NOT own must come out
// byte-identical. A missing one is the incident (NODES_ACTOR_CLAUDE_TOKEN
// destroyed by a FORCE rotation), so it is reported as destruction by name.
func assertAccretedKeysSurvived(t *testing.T, host string, env envFile) {
	t.Helper()
	for key, want := range accretedKeys {
		got, present := env.values[key]
		if !present {
			t.Errorf("%s: rotation DESTROYED %s — the rotation owns no value for that key and must leave it alone", host, key)
			continue
		}
		if got != want {
			t.Errorf("%s: rotation rewrote %s to %q, want the untouched %q", host, key, got, want)
		}
	}
}

// assertGeneratedKeysWereRotated is the other half: merging is not an excuse
// to stop rotating. Every key the lane generates must hold a fresh secret,
// and the generated block's own non-secret values must still be written.
func assertGeneratedKeysWereRotated(t *testing.T, host string, env envFile) {
	t.Helper()
	for _, key := range []string{
		"POSTGRES_PASSWORD", "MINIO_ROOT_PASSWORD",
		"NODES_HUMAN_DECISION_TOKEN_SECRET", "NODES_CALLBACK_TOKEN_SECRET",
		"NODES_RUNNER_SECRET",
	} {
		got := env.values[key]
		if strings.HasPrefix(got, "old-generated-") {
			t.Errorf("%s: %s still holds the pre-rotation value %q; a confirmed rotation must replace the secrets it generates", host, key, got)
		}
		if !hexSecret.MatchString(got) {
			t.Errorf("%s: %s = %q, want a freshly generated 32-byte hex secret", host, key, got)
		}
	}
	if got := env.values["NODES_CALLBACK_BASE_URL"]; got != "http://thor:18080" {
		t.Errorf("%s: NODES_CALLBACK_BASE_URL = %q, want the generated block's value", host, got)
	}
}

// TestProdEnvFirstInstallWritesTheWholeGeneratedSet guards the other end of
// the merge: on a host with no prod.env at all, the lane must still create
// the complete generated block. A merge loop that only ever updated existing
// keys would leave a brand-new machine with an empty file and no failure.
func TestProdEnvFirstInstallWritesTheWholeGeneratedSet(t *testing.T) {
	c := newFakeCluster(t)
	c.hostHome(t, "thor")
	c.hostHome(t, "orin")

	out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"})
	if code != 0 {
		t.Fatalf("first install exited %d; output:\n%s", code, out)
	}

	for _, host := range []string{"thor", "orin"} {
		path := c.prodEnvPath(t, host)
		env := readEnvFile(t, path)
		env.assertNoDuplicateKeys(t, path)
		for _, key := range []string{
			"POSTGRES_USER", "POSTGRES_DB", "POSTGRES_PASSWORD",
			"MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD",
			"NODES_HUMAN_DECISION_TOKEN_SECRET", "NODES_CALLBACK_TOKEN_SECRET",
			"NODES_CALLBACK_BASE_URL", "NODES_RUNNER_SECRET",
		} {
			if _, present := env.values[key]; !present {
				t.Errorf("%s: fresh install left %s out of prod.env", host, key)
			}
		}
	}
}

// TestProdEnvUnforcedRerunChangesNothing keeps the pre-existing guard honest
// under the new merge: without FORCE_PROD the lane still refuses, and refusing
// means the file is byte-identical afterwards — a merge must not become a
// quiet way to rotate a live database password.
func TestProdEnvUnforcedRerunChangesNothing(t *testing.T) {
	c := newFakeCluster(t)
	for _, host := range []string{"thor", "orin"} {
		c.seedProdEnv(t, host, accretedProdEnv)
	}

	out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"})
	if code != 0 {
		t.Fatalf("unforced re-run exited %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "kept existing prod.env") {
		t.Errorf("unforced re-run did not report keeping the existing prod.env; output:\n%s", out)
	}

	for _, host := range []string{"thor", "orin"} {
		env := readEnvFile(t, c.prodEnvPath(t, host))
		if env.values["POSTGRES_PASSWORD"] != "old-generated-postgres-password" {
			t.Errorf("%s: an unforced re-run rotated POSTGRES_PASSWORD to %q", host, env.values["POSTGRES_PASSWORD"])
		}
		for key, want := range accretedKeys {
			if got := env.values[key]; got != want {
				t.Errorf("%s: an unforced re-run changed %s from %q to %q", host, key, want, got)
			}
		}
	}
}

// TestProdEnvMergeSurvivesAFileWithNoTrailingNewline pins the one way a
// key-by-key merge can destroy a credential all by itself. The merge appends
// a key it does not find; appending to a file whose last line has no newline
// concatenates the new assignment onto the old one, and the last accreted
// value — here the relayed claude token — silently stops existing under a
// name nothing reads.
//
// prod.env is hand-edited in practice (that is how half its keys got there),
// and an editor or an `echo -n` that leaves off the final newline is not
// exotic. This is the same failure class as the wholesale rewrite: a
// rotation quietly eating a key it does not own.
func TestProdEnvMergeSurvivesAFileWithNoTrailingNewline(t *testing.T) {
	c := newFakeCluster(t)
	for _, host := range []string{"thor", "orin"} {
		c.seedProdEnv(t, host, strings.TrimSuffix(accretedProdEnv, "\n"))
		c.confirmRotation(t, host)
	}

	out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"}, "FORCE_PROD=1")
	if code != 0 {
		t.Fatalf("rotation exited %d; output:\n%s", code, out)
	}

	for _, host := range []string{"thor", "orin"} {
		path := c.prodEnvPath(t, host)
		env := readEnvFile(t, path)
		env.assertNoDuplicateKeys(t, path)
		if got := env.values["DISCORD_WEBHOOK_URL"]; got != accretedKeys["DISCORD_WEBHOOK_URL"] {
			t.Errorf("%s: the file's last line was consumed by an append — DISCORD_WEBHOOK_URL = %q, want %q", host, got, accretedKeys["DISCORD_WEBHOOK_URL"])
		}
		// Whatever was appended must be its own assignment, not a suffix.
		for _, line := range strings.Split(strings.TrimSuffix(env.raw, "\n"), "\n") {
			if strings.Count(line, "=") > 0 && strings.Contains(strings.SplitN(line, "=", 2)[1], "NODES_ACTOR_CODEX") {
				t.Errorf("%s: a key was appended onto the end of another line: %q", host, line)
			}
		}
	}
}

// --- criterion 3: the same idiom, not a second one -----------------------

// prodEnvMergeLoop is the key-by-key merge install-secrets.sh already used
// for its actor-token lanes before this task: read a KEY=VALUE line, replace
// that key's line if present, append it otherwise. Task t11's third
// acceptance criterion is that the prod lane REUSES this rather than growing
// a second mechanism, so the assertions below pin the loop to exactly one
// definition (PROD_ENV_MERGE) that every prod.env-writing lane references.
//
// One definition rather than three verbatim copies is the stronger form of
// the same requirement: the copies had already drifted once — the missing
// trailing-newline guard TestProdEnvMergeSurvivesAFileWithNoTrailingNewline
// covers lived in the pasted helpers, not in some third mechanism.
const prodEnvMergeLoop = `while IFS= read -r line; do k=${line%%=*}; [ -z "$k" ] && continue; ` +
	`if grep -q "^${k}=" ~/.culture-nodes/prod.env; then sed -i "s|^${k}=.*|${line}|" ~/.culture-nodes/prod.env; ` +
	`else printf "%s\n" "$line" >> ~/.culture-nodes/prod.env; fi; done`

// prodEnvWriters are the shell functions that write prod.env. Each must
// delegate to the shared merge rather than carry its own copy.
var prodEnvWriters = []string{"install_env", "update_actor_token_line", "update_env_line_on_host"}

func TestProdEnvLaneReusesTheExistingMergeIdiom(t *testing.T) {
	script := readInstallSecrets(t)

	if strings.Contains(script, "cat > ~/.culture-nodes/prod.env") {
		t.Error("install-secrets.sh still writes prod.env wholesale (`cat > ~/.culture-nodes/prod.env`); that is the line that deleted NODES_ACTOR_CLAUDE_TOKEN")
	}
	if count := strings.Count(script, prodEnvMergeLoop); count != 1 {
		t.Errorf("the key-by-key merge loop is written out %d time(s), want exactly 1 (PROD_ENV_MERGE); copies drift", count)
	}
	if !strings.Contains(script, "PROD_ENV_MERGE='"+prodEnvMergeLoop) &&
		!strings.Contains(script, prodEnvMergeLoop+"'") {
		t.Error("the merge loop is not the body of a single PROD_ENV_MERGE definition")
	}

	// Every lane that writes prod.env references that one definition.
	for _, writer := range prodEnvWriters {
		body := shellFunctionBody(t, script, writer)
		if !strings.Contains(body, "$PROD_ENV_MERGE") {
			t.Errorf("%s writes prod.env without the shared merge idiom:\n%s", writer, body)
		}
	}

	// And the prod lane must still be gated: merging is not permission to
	// rotate a live database password.
	guard := shellFunctionBody(t, script, "install_env")
	if !strings.Contains(guard, "-e ~/.culture-nodes/prod.env") || !strings.Contains(guard, "FORCE") {
		t.Errorf("install_env lost its FORCE-guarded existence check:\n%s", guard)
	}
}

// shellFunctionBody returns the text of `name() { ... }` from the script,
// from the opening line to the first line that is exactly "}" — sufficient
// for this file, whose functions are all top-level and closed that way.
func shellFunctionBody(t *testing.T, script, name string) string {
	t.Helper()
	lines := strings.Split(script, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, name+"()") {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == "}" {
				return strings.Join(lines[i:j+1], "\n")
			}
		}
		t.Fatalf("shell function %s is never closed", name)
	}
	t.Fatalf("install-secrets.sh declares no %s function", name)
	return ""
}

// --- criterion 2: removal is still possible ------------------------------

// TestRemoveSecretAddsThenRemovesAKey is honesty condition h37: merge-only
// would trade a silent-destruction bug for a file that can only grow, so the
// documented removal path is exercised end to end — install the key through
// the normal relay lane, then take it away through remove-secret.sh and find
// it gone while everything around it survives.
func TestRemoveSecretAddsThenRemovesAKey(t *testing.T) {
	const relayed = "externally-issued-claude-bridge-token"
	c := newFakeCluster(t)
	c.hostHome(t, "thor")
	c.hostHome(t, "orin")

	// Add: the documented relay lane installs NODES_ACTOR_CLAUDE_TOKEN into
	// the control plane's prod.env.
	out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"}, "NODES_ACTOR_CLAUDE_TOKEN="+relayed)
	if code != 0 {
		t.Fatalf("install exited %d; output:\n%s", code, out)
	}
	before := readEnvFile(t, c.prodEnvPath(t, "thor"))
	if got := before.values["NODES_ACTOR_CLAUDE_TOKEN"]; got != relayed {
		t.Fatalf("the add step did not install NODES_ACTOR_CLAUDE_TOKEN (got %q); nothing to remove", got)
	}

	// Dry run: reports the line, prints no value, changes nothing.
	out, code = c.run(t, removeSecretPath(t), []string{"NODES_ACTOR_CLAUDE_TOKEN", "thor"})
	if code != 0 {
		t.Fatalf("dry run exited %d; output:\n%s", code, out)
	}
	if strings.Contains(out, relayed) {
		t.Error("the dry run printed the secret's value; it must name the key and redact what it holds")
	}
	if !strings.Contains(out, "NODES_ACTOR_CLAUDE_TOKEN") {
		t.Errorf("the dry run does not name the key it would remove; output:\n%s", out)
	}
	if readEnvFile(t, c.prodEnvPath(t, "thor")).values["NODES_ACTOR_CLAUDE_TOKEN"] != relayed {
		t.Error("the dry run removed the key anyway; removal must take an explicit --yes")
	}

	// Remove.
	out, code = c.run(t, removeSecretPath(t), []string{"NODES_ACTOR_CLAUDE_TOKEN", "--yes", "thor"})
	if code != 0 {
		t.Fatalf("removal exited %d; output:\n%s", code, out)
	}
	after := readEnvFile(t, c.prodEnvPath(t, "thor"))
	if got, present := after.values["NODES_ACTOR_CLAUDE_TOKEN"]; present {
		t.Errorf("NODES_ACTOR_CLAUDE_TOKEN survived its removal with value %q", got)
	}
	if strings.Contains(after.raw, relayed) {
		t.Error("the removed key's value is still somewhere in prod.env")
	}
	for _, key := range []string{"POSTGRES_PASSWORD", "NODES_CALLBACK_BASE_URL", "NODES_RUNNER_SECRET"} {
		if before.values[key] != after.values[key] {
			t.Errorf("removing one key changed %s from %q to %q", key, before.values[key], after.values[key])
		}
	}
}

// TestRemoveSecretIsHarmlessWhenTheKeyIsAbsent asserts the removal path
// reports an absent key rather than failing the run or truncating the file —
// an operator cleaning up after a partially-applied change should be able to
// run it twice.
func TestRemoveSecretIsHarmlessWhenTheKeyIsAbsent(t *testing.T) {
	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", accretedProdEnv)

	out, code := c.run(t, removeSecretPath(t), []string{"NEVER_INSTALLED_KEY", "--yes", "thor"})
	if code != 0 {
		t.Fatalf("removing an absent key exited %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "NEVER_INSTALLED_KEY") {
		t.Errorf("output does not name the absent key; output:\n%s", out)
	}
	env := readEnvFile(t, c.prodEnvPath(t, "thor"))
	for key, want := range accretedKeys {
		if got := env.values[key]; got != want {
			t.Errorf("removing an absent key changed %s from %q to %q", key, want, got)
		}
	}
}

// TestRemoveSecretRefusesAKeyItCannotSafelyMatch asserts the key name is
// validated before it is interpolated into anything: remove-secret.sh puts
// the KEY (never a value) into the ssh argv, and a key carrying shell or
// regex metacharacters would make `sed -i "/^${KEY}=/d"` delete lines nobody
// named — the exact class of over-deletion this whole task exists to stop.
func TestRemoveSecretRefusesAKeyItCannotSafelyMatch(t *testing.T) {
	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", accretedProdEnv)

	for _, key := range []string{".*", "NODES_ACTOR.*", "A;rm -rf /", "A B", ""} {
		out, code := c.run(t, removeSecretPath(t), []string{key, "--yes", "thor"})
		if code == 0 {
			t.Errorf("remove-secret.sh accepted the key %q; output:\n%s", key, out)
		}
	}
	env := readEnvFile(t, c.prodEnvPath(t, "thor"))
	if env.raw != accretedProdEnv {
		t.Errorf("a refused removal still modified prod.env:\n%s", env.raw)
	}
}
