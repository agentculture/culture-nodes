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

// env is the environment every script under test runs with. It is built
// from scratch rather than inherited, so nothing an operator happens to have
// exported (a live DISCORD_WEBHOOK_URL, a real NODES_ACTOR_CLAUDE_TOKEN) can
// be relayed into a test's prod.env and from there into a test log.
func (c *fakeCluster) env(extraEnv ...string) []string {
	return append([]string{
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
}

// run executes a deploy/prod script under the fake cluster and returns its
// combined output plus exit code.
func (c *fakeCluster) run(t *testing.T, script string, args []string, extraEnv ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = c.env(extraEnv...)
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

// runSplit is run with the two streams kept apart. install-secrets.sh's
// refusals are deliberately on stderr — a refusal folded into stdout is
// indistinguishable from progress to anything that reads the deploy log —
// so a test that asserts a refusal was ANNOUNCED has to look at stderr
// specifically, not at the interleaving CombinedOutput gives.
func (c *fakeCluster) runSplit(t *testing.T, script string, args []string, extraEnv ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{script}, args...)...)
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
	t.Fatalf("run %s: %v (stdout: %s stderr: %s)", filepath.Base(script), err, stdout.String(), stderr.String())
	return "", "", -1
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

// TestProdEnvMergeReplacesAValueContainingAPipe pins the second way the
// key-by-key merge could destroy a credential by itself, and the reason the
// shared definition no longer uses sed.
//
// The replace branch was `sed -i "s|^${k}=.*|${line}|"`. sed's s/// delimiter
// is part of the expression, so a replacement carrying a `|` closes it early:
//
//	line='NODES_DATABASE_URL=postgres://nodes:pa|ss@thor:5432/nodes'
//	sed -i "s|^${k}=.*|${line}|" prod.env
//	-> sed: unknown option to `s'   exit 1, file byte-identical
//
// Either way the key is NOT merged and the file keeps its previous value —
// worse than the wholesale-rewrite incident this file was written for, where
// the key at least vanished; here it holds a stale credential.
//
// The remote loop runs with no `set -e`, so how loudly that lands depends on
// how many keys ride one merge, and both were reproduced:
//
//   - MULTI-KEY (install_env's generated block): a later iteration's status
//     overwrites the failed one, the merge exits 0, and the lane prints its
//     success line. Silent.
//   - SINGLE-KEY (the relay lanes below): the failed sed is the last command,
//     so ssh returns 1 and the caller's `set -euo pipefail` aborts the run.
//
// Only the single-key form is reachable from a test today, because nothing
// in the multi-key block is operator-supplied — which is exactly the point of
// fixing the shared definition rather than a lane. Nothing install-secrets.sh
// GENERATES can trip this: `openssl rand -hex 32` and `-base64 32` emit no
// pipe. The exposed values are the ones it RELAYS from the operator's
// environment, and the webhook URL is the only relayed value reaching
// PROD_ENV_MERGE today (update_env_line_on_host). NODES_DATABASE_URL — whose
// password an external database hands out, and which a following task makes
// this lane write into the multi-key block — rides the same definition.
//
// Both branches are exercised in order, because only the replace branch ever
// ran sed: phase 1 appends the key to a host that has never had it, phase 2
// replaces it on the next run.
func TestProdEnvMergeReplacesAValueContainingAPipe(t *testing.T) {
	const appended = "https://hooks.example/services/T0|A/B0|C/first-token"
	const replaced = "postgres://nodes:pa|ss@thor:5432/nodes?sslmode=disable"

	c := newFakeCluster(t)
	c.hostHome(t, "thor")
	c.hostHome(t, "orin")

	// Phase 1 — append: a fresh host has no CULTURE_NODES_WEBHOOK_URL line,
	// so the merge takes the append branch (which never used sed).
	out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"},
		"CULTURE_NODES_WEBHOOK_URL="+appended)
	if code != 0 {
		t.Fatalf("first install exited %d; output:\n%s", code, out)
	}
	path := c.prodEnvPath(t, "thor")
	env := readEnvFile(t, path)
	env.assertNoDuplicateKeys(t, path)
	if got := env.values["CULTURE_NODES_WEBHOOK_URL"]; got != appended {
		t.Fatalf("thor: append branch stored CULTURE_NODES_WEBHOOK_URL = %q, want %q", got, appended)
	}
	generated := env.values["POSTGRES_PASSWORD"]

	// Phase 2 — replace: the key is now present, so the merge must rewrite
	// its line. This is the branch that ran sed and changed nothing.
	//
	// A non-zero exit is reported, not fatal: what the merge left in the file
	// is the actual subject, and asserting it is what names the defect.
	out, code = c.run(t, installSecretsPath(t), []string{"thor", "orin"},
		"CULTURE_NODES_WEBHOOK_URL="+replaced)
	if code != 0 {
		t.Errorf("second install exited %d; output:\n%s", code, out)
	}
	env = readEnvFile(t, path)
	env.assertNoDuplicateKeys(t, path)
	if got := env.values["CULTURE_NODES_WEBHOOK_URL"]; got != replaced {
		t.Errorf("thor: a value containing a pipe was NOT merged — CULTURE_NODES_WEBHOOK_URL = %q, want %q; the merge left the previous value in place", got, replaced)
	}
	if strings.Contains(env.raw, appended) {
		t.Errorf("thor: the superseded value is still in prod.env:\n%s", env.raw)
	}

	// The rest of the file is untouched by a merge that rewrites one line —
	// an unforced re-run rotates nothing, and the pipe must not have leaked
	// into a neighbouring assignment.
	if got := env.values["POSTGRES_PASSWORD"]; got != generated {
		t.Errorf("thor: the second run changed POSTGRES_PASSWORD from %q to %q", generated, got)
	}
	if got := env.values["NODES_CALLBACK_BASE_URL"]; got != "http://thor:18080" {
		t.Errorf("thor: NODES_CALLBACK_BASE_URL = %q, want the generated block's value", got)
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
//
// The replacement half of the loop was `sed -i "s|^${k}=.*|${line}|"` until
// TestProdEnvMergeReplacesAValueContainingAPipe below; it is now a literal
// line-by-line rewrite, because a value carrying sed's own s/// delimiter
// terminated the expression and the key was silently skipped. This text is
// updated deliberately whenever the shared definition changes — it is the
// only assertion in this file that reads the script rather than running it,
// and its job is to keep the loop singular, not to freeze its body.
const prodEnvMergeLoop = `while IFS= read -r line; do k=${line%%=*}; [ -z "$k" ] && continue; ` +
	`tmp=~/.culture-nodes/prod.env.merge.$$; : > "$tmp"; chmod 600 "$tmp"; found=0; ` +
	`while IFS= read -r cur || [ -n "$cur" ]; do ` +
	`case "$cur" in "$k"=*) printf "%s\n" "$line" >> "$tmp"; found=1;; ` +
	`*) printf "%s\n" "$cur" >> "$tmp";; esac; done < ~/.culture-nodes/prod.env; ` +
	`[ "$found" = 1 ] || printf "%s\n" "$line" >> "$tmp"; ` +
	`mv "$tmp" ~/.culture-nodes/prod.env; done`

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

// --- the deployment-settings lane (issue #124) ----------------------------
//
// install_env's FORCE_PROD guard returns BEFORE the key-by-key merge, so on a
// host that already carries a prod.env the prod lane writes nothing at all. A
// key added to the generated block after that host was provisioned could
// therefore reach it only by rotating every secret in the block alongside it —
// a live database password included. NODES_DATABASE_URL is the key that
// actually hit this, and it cost two operator hand-turns: thor's prod.env
// edited by hand mid-deploy, and orin's deploy aborting outright at
//
//	error while interpolating services.worker.environment.NODES_DATABASE_URL
//
// install_deployment_settings is the answer: an UNGUARDED add-if-absent lane
// that mints nothing, for the non-secret half of prod.env.
//
// accretedProdEnv above is the right seed for all of it without being changed
// one character: it already carries POSTGRES_PASSWORD and already lacks
// NODES_DATABASE_URL, which is exactly the pre-incident shape of a provisioned
// host. These tests run the real script against it and read the file
// afterwards — a lane whose whole contract is "what the host ends up with"
// cannot be proven by grepping the script for the string it intends to write.

// seededPostgresPassword is accretedProdEnv's POSTGRES_PASSWORD: the value
// some PREVIOUS run generated and the live database was actually initdb'd
// with. Every assertion naming it below is checking one thing — that the URL
// was composed from the HOST's own file and not from the password this run
// minted in memory. A URL built from the fresh value looks perfectly correct
// in prod.env and authenticates as nobody on the next restart.
const seededPostgresPassword = "old-generated-postgres-password"

// databaseHostOf is the container-resolved database hostname each host's
// NODES_DATABASE_URL must name. thor reaches the bundled compose service
// `postgres`; orin reaches the same database as `thor`, the name
// compose.orin.yml resolves through its extra_hosts entry from THOR_IP. These
// are NOT the script's ssh targets, and a lane that reused the ssh target
// would give thor a URL pointing at itself.
var databaseHostOf = map[string]string{"thor": "postgres", "orin": "thor"}

// wantDatabaseURL is the URL each host must end up with, given a prod.env
// seeded with seededPostgresPassword and no DATABASE_SSLMODE of its own.
func wantDatabaseURL(host, sslmode string) string {
	return "postgres://nodes:" + seededPostgresPassword + "@" + databaseHostOf[host] + ":5432/nodes?sslmode=" + sslmode
}

// withoutKey drops one KEY= line from an env-file body, so a seed missing one
// key stays derived from accretedProdEnv instead of forking a near-copy of it
// that stops matching when the fixture changes.
func withoutKey(body, key string) string {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, key+"=") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// TestDeploymentSettingsReachAProvisionedHostWithoutRotating is issue #124 in
// executable form, and the reason the whole lane exists.
//
// Both hosts start out as the incident left them: a prod.env with a live
// POSTGRES_PASSWORD from an earlier run and NO NODES_DATABASE_URL. The script
// is run with nothing set — no FORCE_PROD, no confirmation file — because
// "compose says a variable is missing on a host I already installed" must be
// answered by a plain re-run and never by rotating a live credential.
//
// The load-bearing assertion is which password the URL carries. This run
// generated a fresh POSTGRES_PASSWORD in memory and the guarded lane above
// correctly refused to install it; a URL composed locally would therefore
// embed a password the database has never heard of, in a prod.env that reads
// as entirely correct, and the stack would fail auth on its next restart.
// Finding the SEEDED password in the URL is the evidence it was read on the
// host, from the host's own file.
func TestDeploymentSettingsReachAProvisionedHostWithoutRotating(t *testing.T) {
	if strings.Contains(accretedProdEnv, "NODES_DATABASE_URL") {
		t.Fatal("accretedProdEnv now carries NODES_DATABASE_URL; the seed no longer reproduces the #124 shape (a provisioned host MISSING the key) and this test proves nothing")
	}

	c := newFakeCluster(t)
	for _, host := range []string{"thor", "orin"} {
		c.seedProdEnv(t, host, accretedProdEnv)
	}

	out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"})
	if code != 0 {
		t.Fatalf("unforced re-run exited %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "kept existing prod.env") {
		t.Errorf("the guarded lane did not report keeping the existing prod.env — these settings must arrive WITHOUT a rotation, so a run that rotated proves nothing; output:\n%s", out)
	}

	for _, host := range []string{"thor", "orin"} {
		path := c.prodEnvPath(t, host)
		env := readEnvFile(t, path)
		env.assertNoDuplicateKeys(t, path)

		url, present := env.values["NODES_DATABASE_URL"]
		if !present {
			t.Errorf("%s: an unforced re-run left NODES_DATABASE_URL out of prod.env — that is issue #124 itself: the key is reachable only by rotating every generated secret with it", host)
			continue
		}
		if !strings.Contains(url, seededPostgresPassword) {
			t.Errorf("%s: NODES_DATABASE_URL does not carry the password this host's prod.env holds; it was composed from something other than the host's own file, so it will fail auth on the next restart while looking correct", host)
		}
		if want := wantDatabaseURL(host, "disable"); url != want {
			t.Errorf("%s: NODES_DATABASE_URL = %q, want %q", host, url, want)
		}
		if got := env.values["POSTGRES_PASSWORD"]; got != seededPostgresPassword {
			t.Errorf("%s: delivering a deployment setting rotated POSTGRES_PASSWORD to %q; this lane mints nothing and must touch no credential", host, got)
		}
		assertAccretedKeysSurvived(t, host, env)
	}
}

// TestDeploymentSettingsWriteSslmodeAsALiteral pins challenge finding c29.
//
// The obvious way to write the URL is with a `${DATABASE_SSLMODE}` placeholder
// and let compose resolve it. Compose does interpolate env-file values
// recursively — but only BACKWARDS: a placeholder resolves only while the key
// it names happens to sit EARLIER in the file. In the other order compose
// resolves `sslmode=` to the empty string and reports no error whatsoever;
// libpq then falls back to its own default, the stack connects, and nobody
// learns the TLS mode was never applied. An add-if-absent lane appends in
// whatever order a given host's gaps dictate, so it is perfectly capable of
// producing exactly that file.
//
// Resolving on the host removes the ordering dependency instead of documenting
// it, so the URL must contain no `${` at all and must end in a NON-EMPTY
// sslmode — `sslmode=` is the exact string this lane exists never to write.
func TestDeploymentSettingsWriteSslmodeAsALiteral(t *testing.T) {
	c := newFakeCluster(t)
	for _, host := range []string{"thor", "orin"} {
		c.seedProdEnv(t, host, accretedProdEnv)
	}

	out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"})
	if code != 0 {
		t.Fatalf("unforced re-run exited %d; output:\n%s", code, out)
	}

	for _, host := range []string{"thor", "orin"} {
		env := readEnvFile(t, c.prodEnvPath(t, host))
		url, present := env.values["NODES_DATABASE_URL"]
		if !present {
			t.Errorf("%s: no NODES_DATABASE_URL was written", host)
			continue
		}
		if strings.Contains(url, "${") {
			t.Errorf("%s: NODES_DATABASE_URL = %q carries a compose placeholder; it resolves only when the key it names sits earlier in the file, and silently resolves to the empty string when it does not", host, url)
		}
		_, sslmode, found := strings.Cut(url, "?sslmode=")
		if !found {
			t.Errorf("%s: NODES_DATABASE_URL = %q names no sslmode at all; libpq then applies its own default and the TLS mode is whatever nobody chose", host, url)
			continue
		}
		if sslmode == "" {
			t.Errorf("%s: NODES_DATABASE_URL = %q ends in an empty sslmode — the exact quiet wrongness this lane exists to prevent", host, url)
		}
	}
}

// TestDeploymentSettingsHonourTheHostsOwnSslmode is the other half of c29.
//
// Resolving sslmode on the host is only correct if it resolves the HOST's
// value. A lane that wrote its own default into the URL would silently
// downgrade a host an operator had already set to `require` — and, because the
// URL is add-if-absent, that downgrade is permanent until someone removes the
// key by hand. The default (`disable`, the LAN network-trust decision) applies
// only where the host expressed no choice.
func TestDeploymentSettingsHonourTheHostsOwnSslmode(t *testing.T) {
	c := newFakeCluster(t)
	for _, host := range []string{"thor", "orin"} {
		c.seedProdEnv(t, host, accretedProdEnv+"DATABASE_SSLMODE=require\n")
	}

	out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"})
	if code != 0 {
		t.Fatalf("unforced re-run exited %d; output:\n%s", code, out)
	}

	for _, host := range []string{"thor", "orin"} {
		path := c.prodEnvPath(t, host)
		env := readEnvFile(t, path)
		env.assertNoDuplicateKeys(t, path)
		if got := env.values["DATABASE_SSLMODE"]; got != "require" {
			t.Errorf("%s: DATABASE_SSLMODE = %q, want the host's own %q left alone", host, got, "require")
		}
		if want := wantDatabaseURL(host, "require"); env.values["NODES_DATABASE_URL"] != want {
			t.Errorf("%s: NODES_DATABASE_URL = %q, want %q — the URL must carry the TLS mode the host chose, not this lane's default", host, env.values["NODES_DATABASE_URL"], want)
		}
	}
}

// TestDeploymentSettingsNeverReplaceAnOperatorEditedValue pins the deliberate
// asymmetry: a key prod.env does not have is written, a key it HAS is left
// alone however wrong it looks.
//
// deploy/prod/README's "Bundled or external PostgreSQL" section tells an
// operator to point the stack at an external database by hand-editing
// NODES_DATABASE_URL and COMPOSE_PROFILES on the host. A lane that re-asserted
// its own values every run would silently revert that documented choice on the
// next deploy and bring the stack back up against the bundled database having
// reported nothing — the same shape of quiet damage as the wholesale rewrite
// this file was written for, arriving from the opposite direction.
//
// The cost is stated in the README rather than hidden: correcting a wrong
// value is remove-secret.sh followed by a re-run, not a re-run.
func TestDeploymentSettingsNeverReplaceAnOperatorEditedValue(t *testing.T) {
	const externalURL = "postgres://nodes:provider-issued-password@db.example.net:5432/nodes?sslmode=verify-full"
	const externalProfiles = "backup"

	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", accretedProdEnv+
		"NODES_DATABASE_URL="+externalURL+"\n"+
		"COMPOSE_PROFILES="+externalProfiles+"\n")
	c.seedProdEnv(t, "orin", accretedProdEnv+"NODES_DATABASE_URL="+externalURL+"\n")

	out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"})
	if code != 0 {
		t.Fatalf("unforced re-run exited %d; output:\n%s", code, out)
	}

	for _, host := range []string{"thor", "orin"} {
		path := c.prodEnvPath(t, host)
		env := readEnvFile(t, path)
		env.assertNoDuplicateKeys(t, path)
		if got := env.values["NODES_DATABASE_URL"]; got != externalURL {
			t.Errorf("%s: a re-run reverted the operator's external NODES_DATABASE_URL to %q; the documented external-database edit must survive every deploy", host, got)
		}
		if strings.Contains(env.raw, seededPostgresPassword+"@") {
			t.Errorf("%s: prod.env now carries a bundled-database URL alongside the operator's external one:\n%s", host, env.raw)
		}
	}
	if got := readEnvFile(t, c.prodEnvPath(t, "thor")).values["COMPOSE_PROFILES"]; got != externalProfiles {
		t.Errorf("thor: a re-run reverted COMPOSE_PROFILES to %q; re-adding bundled-postgres restarts the very database the operator moved off", got)
	}
}

// TestDeploymentSettingsRefuseAMissingPostgresPasswordByName covers the host
// this lane cannot serve: a prod.env with no POSTGRES_PASSWORD to compose from.
//
// Two things must both hold, and they pull in opposite directions.
//
// It must not write the URL anyway. `postgres://nodes:@postgres:5432/nodes`
// authenticates as nobody while reading, to anything that greps for the key,
// as configured — the same class of quiet wrongness as an empty sslmode.
//
// And it must not abort the run. Aborting before the later lanes (codex bridge
// tokens, the notify token, the claude token relay) is the #124 failure shape
// itself: one unsatisfiable key stopping every subsequent lane from reaching a
// host. So the refusal is announced BY NAME on stderr, the settings that do
// not depend on the password are still delivered, and the script continues.
func TestDeploymentSettingsRefuseAMissingPostgresPasswordByName(t *testing.T) {
	c := newFakeCluster(t)
	seed := withoutKey(accretedProdEnv, "POSTGRES_PASSWORD")
	if strings.Contains(seed, "POSTGRES_PASSWORD") {
		t.Fatal("the seed still carries POSTGRES_PASSWORD; there is nothing to refuse")
	}
	for _, host := range []string{"thor", "orin"} {
		c.seedProdEnv(t, host, seed)
	}

	stdout, stderr, code := c.runSplit(t, installSecretsPath(t), []string{"thor", "orin"})
	if code != 0 {
		t.Fatalf("a refused NODES_DATABASE_URL aborted the run (exit %d); the later lanes must still reach both hosts\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, key := range []string{"POSTGRES_PASSWORD", "NODES_DATABASE_URL"} {
		if !strings.Contains(stderr, key) {
			t.Errorf("the refusal does not name %s on stderr; an unannounced skip is indistinguishable from success in a deploy log\nstderr:\n%s", key, stderr)
		}
	}

	for _, host := range []string{"thor", "orin"} {
		path := c.prodEnvPath(t, host)
		env := readEnvFile(t, path)
		env.assertNoDuplicateKeys(t, path)

		if got, present := env.values["NODES_DATABASE_URL"]; present {
			t.Errorf("%s: NODES_DATABASE_URL = %q was written with no password to compose it from; a URL that authenticates as nobody reads as configured to everything that greps for the key", host, got)
		}
		if strings.Contains(env.raw, "postgres://nodes:@") {
			t.Errorf("%s: prod.env carries a URL with an empty password:\n%s", host, env.raw)
		}
		// The settings that do NOT depend on the password still arrive.
		if got := env.values["DATABASE_SSLMODE"]; got != "disable" {
			t.Errorf("%s: DATABASE_SSLMODE = %q — one unsatisfiable key must not take the rest of the lane's settings with it", host, got)
		}
		// …and so do the LATER lanes, which is why the refusal is not fatal.
		if _, present := env.values["NODES_ACTOR_CODEX_THOR_TOKEN"]; !present {
			t.Errorf("%s: the codex-bridge lane never ran — the refusal stopped the script instead of continuing past it", host)
		}
	}
	if got := readEnvFile(t, c.prodEnvPath(t, "thor")).values["COMPOSE_PROFILES"]; got != "bundled-postgres,backup" {
		t.Errorf("thor: COMPOSE_PROFILES = %q, want the thor-only profile list delivered despite the refused URL", got)
	}
}

// TestDeploymentSettingsAreIdempotent asserts the second run is a no-op and
// SAYS so.
//
// This lane runs unguarded on every invocation of install-secrets.sh, so it is
// the one lane an operator re-runs freely — which makes "a second run changes
// nothing" a property, not a nicety. And because add-if-absent already means a
// second run finds nothing to do, a lane printing the same success line either
// way would be a second place in this script claiming success without
// evidence: the report must distinguish keys actually added from none.
func TestDeploymentSettingsAreIdempotent(t *testing.T) {
	c := newFakeCluster(t)
	for _, host := range []string{"thor", "orin"} {
		c.seedProdEnv(t, host, accretedProdEnv)
	}

	out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"})
	if code != 0 {
		t.Fatalf("first re-run exited %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "added deployment settings to prod.env on thor:") {
		t.Errorf("the first run does not report which settings it added; output:\n%s", out)
	}
	first := map[string]envFile{}
	for _, host := range []string{"thor", "orin"} {
		first[host] = readEnvFile(t, c.prodEnvPath(t, host))
	}

	out, code = c.run(t, installSecretsPath(t), []string{"thor", "orin"})
	if code != 0 {
		t.Fatalf("second re-run exited %d; output:\n%s", code, out)
	}
	for _, host := range []string{"thor", "orin"} {
		if !strings.Contains(out, "no deployment settings to add on "+host) {
			t.Errorf("the second run does not report that it added nothing on %s; a lane that prints the same line either way reports success without evidence; output:\n%s", host, out)
		}
		path := c.prodEnvPath(t, host)
		second := readEnvFile(t, path)
		second.assertNoDuplicateKeys(t, path)
		if second.raw != first[host].raw {
			t.Errorf("%s: a second unforced re-run changed prod.env.\nbefore:\n%s\nafter:\n%s", host, first[host].raw, second.raw)
		}
	}
}

// TestCodeRunnerNameFollowsTheRestOfItsTuple pins the fix for a live outage,
// and the reason the condition cannot live in compose.
//
// cmd/nodes/worker.go refuses a PARTIAL code-runner tuple — one key set while
// another is empty — because that attributes a code operation to a runner
// nobody can identify. Setting NONE of the three is legitimate and means "this
// deployment runs no code nodes at all".
//
// Both compose files used to hardcode NODES_CODE_RUNNER_NAME, which made that
// legitimate state unreachable: the worker always saw exactly one of the three
// set. thor survived only because someone had hand-installed the other two.
// orin had none, ran fine for 46 hours on an older image, and CrashLoopBackOff'd
// (exit 2, 11 restarts) the moment a deploy brought it to a revision carrying
// the check. Compose has no conditionals, so the rule lives in the lane that is
// already reading prod.env.
//
// The lane must never INVENT the other two: a build revision and a registered
// actor row are facts about a deployment, and guessing either would attribute
// evidence to a runner that never produced it.
func TestCodeRunnerNameFollowsTheRestOfItsTuple(t *testing.T) {
	const revision = "68024ac9a00cf3613a93c89ea251bde5b3cdfe32"
	const actorRow = "actor_code_runner_ROW123"

	c := newFakeCluster(t)
	// thor: already carries the other two, as the live host does.
	c.seedProdEnv(t, "thor", accretedProdEnv+
		"NODES_CODE_RUNNER_REVISION="+revision+"\n"+
		"NODES_CODE_RUNNER_ACTOR_ID="+actorRow+"\n")
	// orin: carries neither — the state that crash-looped.
	c.seedProdEnv(t, "orin", accretedProdEnv)

	out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"})
	if code != 0 {
		t.Fatalf("run exited %d; output:\n%s", code, out)
	}

	thor := readEnvFile(t, c.prodEnvPath(t, "thor"))
	if got := thor.values["NODES_CODE_RUNNER_NAME"]; got != "headspace" {
		t.Errorf("thor has the other two runner keys but NODES_CODE_RUNNER_NAME is %q, want \"headspace\"; without it thor's worker loses its code-runner capability the next time it is deployed against a compose file that no longer hardcodes the name", got)
	}
	if thor.values["NODES_CODE_RUNNER_REVISION"] != revision || thor.values["NODES_CODE_RUNNER_ACTOR_ID"] != actorRow {
		t.Errorf("thor's pre-existing runner keys were modified: revision=%q actor=%q", thor.values["NODES_CODE_RUNNER_REVISION"], thor.values["NODES_CODE_RUNNER_ACTOR_ID"])
	}

	orin := readEnvFile(t, c.prodEnvPath(t, "orin"))
	for _, key := range []string{"NODES_CODE_RUNNER_NAME", "NODES_CODE_RUNNER_REVISION", "NODES_CODE_RUNNER_ACTOR_ID"} {
		if got, present := orin.values[key]; present {
			t.Errorf("orin carries none of the runner tuple, so the lane must write NONE of it — but %s = %q. One key alone is exactly what the worker refuses at startup, and inventing the other two would attribute code evidence to a runner that never ran", key, got)
		}
	}
	orin.assertNoDuplicateKeys(t, c.prodEnvPath(t, "orin"))
}
