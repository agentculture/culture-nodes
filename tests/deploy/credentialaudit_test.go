// Package deploytest -- this file is task t12's (upkeep-actors-jira plan,
// issue #69 item 2): the post-deploy credential audit. deploy.sh used to end
// without ever asking whether the environment it had just shipped was
// complete, so a credential that went missing was discovered by the first
// component that needed it, whenever that happened to be.
//
// The incident this detector answers is t11's:
//
//	Aug 13 13:03  company/developer  succeeded       <- auth working
//	   ... FORCE=1 rotation removed NODES_ACTOR_CLAUDE_TOKEN ...
//	Aug 14 06:42  company/developer  policy_denied   <- 401, token gone
//
// ~18 hours of silence, because the running worker held the token in memory
// until its next restart. t11 fixed the CAUSE (prod.env writes now merge key
// by key, tests/deploy/prodenvmerge_test.go). This is a different mechanism,
// not a second fix for the same one: whatever removes a key next -- a hand
// edit, a restore from an older file, a lane that was never taught to install
// it on this host -- the deploy that follows says so out loud.
//
// These are BEHAVIORAL, in prodenvmerge_test.go's shape: the real script runs
// against a stub `ssh` that executes each remote command locally under a
// per-host HOME, so `~/.culture-nodes/prod.env` is a real file the audit
// really reads. Asserting on the script's source text would prove only that
// somebody wrote the words "required" somewhere.
package deploytest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// auditScriptPath locates deploy/prod/audit-credentials.sh beside
// install-secrets.sh (installSecretsPath comes from codexsecrets_test.go).
func auditScriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(installSecretsPath(t)), "audit-credentials.sh")
}

// auditProdEnvComplete is a thor prod.env carrying every key the audit
// classifies as required, plus two optional ones, plus one key no compose
// file declares. Values are visibly inert placeholders; several tests below
// assert that none of them is ever printed, so they need to be greppable.
const auditProdEnvComplete = `POSTGRES_USER=nodes
POSTGRES_DB=nodes
POSTGRES_PASSWORD=placeholder-postgres-password
NODES_DATABASE_URL=postgres://nodes:placeholder-postgres-password@postgres:5432/nodes?sslmode=disable
MINIO_ROOT_USER=nodesroot
MINIO_ROOT_PASSWORD=placeholder-minio-password
NODES_HUMAN_DECISION_TOKEN_SECRET=placeholder-human-decision-secret
NODES_CALLBACK_TOKEN_SECRET=placeholder-callback-secret
NODES_CALLBACK_BASE_URL=http://thor:18080
NODES_RUNNER_SECRET=placeholder-runner-secret
NODES_NAMESPACE_ID=01JQZZZZZZZZZZZZZZZZZZZZZZ
NODES_ACTOR_CLAUDE_TOKEN=placeholder-claude-bridge-token
NODES_ACTOR_CODEX_THOR_TOKEN=placeholder-codex-thor-token
NODES_ACTOR_CODEX_ORIN_TOKEN=placeholder-codex-orin-token
NODES_ACTOR_NOTIFY_TOKEN=placeholder-notify-token
`

// auditPlaceholderValues are the secret VALUES of the fixture above. None may
// appear in the audit's output or in an ssh argv: the audit reports key NAMES.
var auditPlaceholderValues = []string{
	"placeholder-postgres-password",
	"placeholder-minio-password",
	"placeholder-human-decision-secret",
	"placeholder-callback-secret",
	"placeholder-runner-secret",
	"placeholder-claude-bridge-token",
	"placeholder-codex-thor-token",
	"placeholder-codex-orin-token",
	"placeholder-notify-token",
}

// prodEnvWithout returns the complete fixture minus one key's whole line --
// the shape a FORCE rotation left prod.env in, not a key set to empty (that
// case is its own test below).
func prodEnvWithout(keys ...string) string {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimSuffix(auditProdEnvComplete, "\n"), "\n") {
		name, _, _ := strings.Cut(line, "=")
		if drop[name] {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n") + "\n"
}

// runAudit runs the audit for host under the fake cluster.
func runAudit(t *testing.T, c *fakeCluster, host string, extraEnv ...string) (string, int) {
	t.Helper()
	return c.run(t, auditScriptPath(t), []string{host}, extraEnv...)
}

// logSSHArgv replaces the fake cluster's ssh stub with one that appends its
// own argv to a log file before doing the same thing the original does. The
// log is how "a secret never reaches an argv" is checked against what the
// script really passed, rather than against a regexp over its source.
func logSSHArgv(t *testing.T, c *fakeCluster) string {
	t.Helper()
	logPath := filepath.Join(c.root, "..", "ssh-argv.log")
	stub := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> " + logPath + "\n" +
		"host=$1; shift\n" +
		"export HOME=\"$FAKE_SSH_HOME_ROOT/$host\"\n" +
		"mkdir -p \"$HOME\"\n" +
		"exec bash -c \"$*\"\n"
	if err := os.WriteFile(filepath.Join(c.binDir, "ssh"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write logging ssh stub: %v", err)
	}
	return logPath
}

// syntheticComposeDir writes compose files the audit is pointed at with
// AUDIT_COMPOSE_DIR. Tests that use it are proving the declared set is READ
// from compose: no hand-maintained list in the script can know about a
// variable invented inside a test's temp directory.
func syntheticComposeDir(t *testing.T, thorBody, orinBody string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"compose.thor.yml": thorBody, "compose.orin.yml": orinBody} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// --- acceptance criterion 2: a missing required key fails the deploy ------

// TestAuditFailsOnMissingRequiredKey is the incident in executable form: the
// one key a FORCE rotation actually destroyed is gone from prod.env, and the
// audit must exit non-zero and name it.
func TestAuditFailsOnMissingRequiredKey(t *testing.T) {
	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", prodEnvWithout("NODES_ACTOR_CLAUDE_TOKEN"))

	out, code := runAudit(t, c, "thor")
	if code == 0 {
		t.Fatalf("audit passed with NODES_ACTOR_CLAUDE_TOKEN missing; it must fail. Output:\n%s", out)
	}
	if !strings.Contains(out, "missing (required)") {
		t.Errorf("audit output does not carry the `missing (required)` section; an operator has to be able to see WHICH class failed. Output:\n%s", out)
	}
	if !strings.Contains(out, "NODES_ACTOR_CLAUDE_TOKEN") {
		t.Errorf("audit failed without naming NODES_ACTOR_CLAUDE_TOKEN. Output:\n%s", out)
	}
}

// TestAuditFailsOnRequiredKeyPresentButEmpty -- `KEY=` is not a credential.
// A key whose line survived with its value stripped fails exactly like an
// absent one, and says which of the two it was.
func TestAuditFailsOnRequiredKeyPresentButEmpty(t *testing.T) {
	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", prodEnvWithout("NODES_CALLBACK_TOKEN_SECRET")+"NODES_CALLBACK_TOKEN_SECRET=\n")

	out, code := runAudit(t, c, "thor")
	if code == 0 {
		t.Fatalf("audit passed with an empty NODES_CALLBACK_TOKEN_SECRET. Output:\n%s", out)
	}
	if !strings.Contains(out, "empty (required)") || !strings.Contains(out, "NODES_CALLBACK_TOKEN_SECRET") {
		t.Errorf("an empty required value must be reported as `empty (required)` and named. Output:\n%s", out)
	}
}

// TestAuditFailsLoudlyWhenProdEnvIsUnreadable -- no prod.env at all is an
// environment failure, distinct from a missing key, and names the path.
func TestAuditFailsLoudlyWhenProdEnvIsUnreadable(t *testing.T) {
	c := newFakeCluster(t)
	c.hostHome(t, "thor") // HOME exists, prod.env does not

	out, code := runAudit(t, c, "thor")
	if code == 0 {
		t.Fatalf("audit passed against a host with no prod.env. Output:\n%s", out)
	}
	if !strings.Contains(out, "prod.env") {
		t.Errorf("the failure does not name ~/.culture-nodes/prod.env. Output:\n%s", out)
	}
}

// --- acceptance criterion 1: three classes, none of them collapsed --------

// TestAuditPassesWhenOnlyOptionalKeysAreAbsent is the class that matters most
// for the audit being read at all. DISCORD_WEBHOOK_URL and its siblings are
// absent by legitimate choice -- their absence closes a feature rather than
// breaking one -- and reporting them as failures is how an operator learns to
// ignore the whole report.
func TestAuditPassesWhenOnlyOptionalKeysAreAbsent(t *testing.T) {
	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", auditProdEnvComplete)

	out, code := runAudit(t, c, "thor")
	if code != 0 {
		t.Fatalf("audit exited %d on a prod.env holding every required key. Output:\n%s", code, out)
	}
	for _, key := range []string{
		"DISCORD_WEBHOOK_URL",                   // named by the task as the shape
		"CULTURE_NODES_WEBHOOK_URL",             // its sibling; either one enables delivery
		"NODES_ADHOC_RUN_TOKEN_SECRET",          // compose says "closed-by-default" in as many words
		"NODES_ACTOR_REGISTRATION_TOKEN_SECRET", // same
		"NODES_EVENT_TOKEN_SECRET",              // same
	} {
		if !strings.Contains(out, key) {
			t.Errorf("absent optional key %s is not reported at all; the audit must classify it, not drop it. Output:\n%s", key, out)
		}
	}
	if !strings.Contains(out, "optional") {
		t.Errorf("audit output has no `optional` section. Output:\n%s", out)
	}
	if strings.Contains(out, "missing (required)") {
		t.Errorf("audit reported a required key missing when only optional ones were absent. Output:\n%s", out)
	}
}

// TestAuditReportsUnknownKeysWithoutFailingOrRemovingThem -- prod.env
// legitimately carries keys compose never mentions (NODES_RUNNER_SECRET is
// one on both real hosts today). They are reported and left alone: deleting a
// key nobody could explain is how the incident happened in the first place.
func TestAuditReportsUnknownKeysWithoutFailingOrRemovingThem(t *testing.T) {
	c := newFakeCluster(t)
	seeded := auditProdEnvComplete + "OPERATOR_ADDED_KEY=placeholder-operator-value\n"
	c.seedProdEnv(t, "thor", seeded)

	out, code := runAudit(t, c, "thor")
	if code != 0 {
		t.Fatalf("an unknown key made the audit fail (exit %d); unknown is reported, not fatal. Output:\n%s", code, out)
	}
	if !strings.Contains(out, "unknown") {
		t.Errorf("audit output has no `unknown` section. Output:\n%s", out)
	}
	for _, key := range []string{"OPERATOR_ADDED_KEY", "NODES_RUNNER_SECRET"} {
		if !strings.Contains(out, key) {
			t.Errorf("unknown key %s is not reported. Output:\n%s", key, out)
		}
	}

	after, err := os.ReadFile(c.prodEnvPath(t, "thor"))
	if err != nil {
		t.Fatalf("read prod.env after audit: %v", err)
	}
	if string(after) != seeded {
		t.Errorf("the audit modified prod.env; it is a read-only detector.\n--- before ---\n%s\n--- after ---\n%s", seeded, after)
	}
}

// TestAuditClassifiesEveryDeclaredKeyOfTheRealComposeFiles is the anti-drift
// gate on the hand-classified half. Every key the shipped compose files
// declare without deciding for themselves (`${KEY:-}` and bare `${KEY}` say
// nothing about whether the deployment works without it) must have an entry
// in the script's one classification list; a new one that nobody classified
// is reported as `unclassified`.
func TestAuditClassifiesEveryDeclaredKeyOfTheRealComposeFiles(t *testing.T) {
	for _, host := range []string{"thor", "orin"} {
		c := newFakeCluster(t)
		// Every key both real compose files declare, set to a placeholder,
		// so nothing fails for absence and the only thing under test is
		// whether each declared key got a class.
		c.seedProdEnv(t, host, allDeclaredKeysSeeded(t))
		out, code := runAudit(t, c, host)
		if code != 0 {
			t.Fatalf("%s: audit exited %d with every compose-declared key set. Output:\n%s", host, code, out)
		}
		if strings.Contains(out, "unclassified") {
			t.Errorf("%s: the audit reports an unclassified compose-declared key -- add it to the script's classification list with a comment saying why it is where it is. Output:\n%s", host, out)
		}
	}
}

// allDeclaredKeysSeeded builds a prod.env holding every `${KEY...}` the two
// shipped compose files reference, each set to a placeholder value.
func allDeclaredKeysSeeded(t *testing.T) string {
	t.Helper()
	dir := filepath.Dir(installSecretsPath(t))
	ref := regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)`)
	seen := map[string]bool{}
	var lines []string
	for _, name := range []string{"compose.thor.yml", "compose.orin.yml"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// $$ is compose's own escape: container-side, never an env-file key.
		body := strings.ReplaceAll(string(raw), "$$", "@@")
		for _, m := range ref.FindAllStringSubmatch(body, -1) {
			if seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			lines = append(lines, m[1]+"=placeholder-declared-value")
		}
	}
	if len(lines) < 10 {
		t.Fatalf("only %d declared keys found in the compose files; the extraction is wrong", len(lines))
	}
	return strings.Join(lines, "\n") + "\n"
}

// --- the declared set is read, not remembered -----------------------------

// TestAuditReadsTheDeclaredSetFromComposeFiles points the audit at compose
// files invented in a temp directory. A key named there and nowhere else must
// still be demanded, which no hand-maintained list in the script could do.
func TestAuditReadsTheDeclaredSetFromComposeFiles(t *testing.T) {
	dir := syntheticComposeDir(t,
		"services:\n  api:\n    environment:\n"+
			"      A: ${SYNTHETIC_REQUIRED_KEY:?install secrets first}\n"+
			"      B: ${SYNTHETIC_DEFAULTED_KEY:-a-working-default}\n",
		"services:\n  worker:\n    environment:\n      C: ${SYNTHETIC_REQUIRED_KEY:?}\n")
	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", "UNRELATED_KEY=placeholder\n")

	out, code := runAudit(t, c, "thor", "AUDIT_COMPOSE_DIR="+dir)
	if code == 0 {
		t.Fatalf("audit passed without SYNTHETIC_REQUIRED_KEY, which the compose file it was pointed at refuses to start without. Output:\n%s", out)
	}
	if !strings.Contains(out, "SYNTHETIC_REQUIRED_KEY") {
		t.Errorf("audit never names SYNTHETIC_REQUIRED_KEY, so it is not reading the compose file. Output:\n%s", out)
	}
	// A compose-supplied default is a working deployment choice, not a
	// missing credential: it may be reported, never as a required failure.
	if idx := strings.Index(out, "missing (required)"); idx >= 0 {
		line := out[idx:]
		if end := strings.Index(line, "\n"); end >= 0 {
			line = line[:end]
		}
		if strings.Contains(line, "SYNTHETIC_DEFAULTED_KEY") {
			t.Errorf("a compose-defaulted key was reported as a missing required credential: %q", line)
		}
	}
}

// TestAuditIgnoresComposeEscapedVariables -- `$${VAR}` in a compose file is
// an escape: the container's own shell expands it, and it is not an env-file
// key at all. compose.thor.yml's backup service is full of them.
func TestAuditIgnoresComposeEscapedVariables(t *testing.T) {
	dir := syntheticComposeDir(t,
		"services:\n  backup:\n    command:\n      - |\n"+
			"        pg_dump -U $${CONTAINER_SIDE_ONLY:-nodes} -f /b/$$(date +%s).dump\n"+
			"    environment:\n      A: ${SYNTHETIC_DEFAULTED_KEY:-fine}\n",
		"services:\n  worker:\n    environment:\n      B: ${SYNTHETIC_DEFAULTED_KEY:-fine}\n")
	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", "SYNTHETIC_DEFAULTED_KEY=placeholder\n")

	out, code := runAudit(t, c, "thor", "AUDIT_COMPOSE_DIR="+dir)
	if code != 0 {
		t.Fatalf("audit exited %d over a compose-escaped variable. Output:\n%s", code, out)
	}
	if strings.Contains(out, "CONTAINER_SIDE_ONLY") {
		t.Errorf("`$${CONTAINER_SIDE_ONLY}` is compose's escape for the container's own shell, not a declared env-file key, but the audit treats it as one. Output:\n%s", out)
	}
}

// TestAuditDoesNotCallAThorOnlyKeyUnknownOnOrin -- the two hosts share one
// generated secret block, so orin's prod.env holds keys only thor's compose
// declares. `unknown` means declared by NO compose file; scoping it to one
// host's file would report half of a correct prod.env as mystery keys.
func TestAuditDoesNotCallAThorOnlyKeyUnknownOnOrin(t *testing.T) {
	c := newFakeCluster(t)
	c.seedProdEnv(t, "orin", auditProdEnvComplete+
		"THOR_IP=192.0.2.10\nTHOR_HOST=thor\nOPERATOR_ADDED_KEY=placeholder-operator-value\n")

	out, code := runAudit(t, c, "orin")
	if code != 0 {
		t.Fatalf("orin audit exited %d on a complete prod.env. Output:\n%s", code, out)
	}
	idx := strings.Index(out, "unknown")
	if idx < 0 {
		t.Fatalf("orin audit reports no unknown section, but OPERATOR_ADDED_KEY is one. Output:\n%s", out)
	}
	unknownSection := out[idx:]
	// NODES_HUMAN_DECISION_TOKEN_SECRET is declared by compose.thor.yml only,
	// and install-secrets.sh puts it on both hosts.
	if strings.Contains(unknownSection, "NODES_HUMAN_DECISION_TOKEN_SECRET") {
		t.Errorf("a key compose.thor.yml declares is reported unknown on orin. Output:\n%s", out)
	}
	if !strings.Contains(unknownSection, "OPERATOR_ADDED_KEY") {
		t.Errorf("orin audit does not report OPERATOR_ADDED_KEY as unknown. Output:\n%s", out)
	}
}

// --- the discipline every deploy/prod script is held to -------------------

// TestAuditNeverPrintsOrArgvsASecretValue. The audit reads a file full of
// live credentials; what it reports is key NAMES. Both halves are checked
// against what actually happened: the script's whole output, and the argv of
// every ssh it invoked.
func TestAuditNeverPrintsOrArgvsASecretValue(t *testing.T) {
	c := newFakeCluster(t)
	argvLog := logSSHArgv(t, c)
	c.seedProdEnv(t, "thor", auditProdEnvComplete)

	out, code := runAudit(t, c, "thor")
	if code != 0 {
		t.Fatalf("audit exited %d. Output:\n%s", code, out)
	}
	for _, value := range auditPlaceholderValues {
		if strings.Contains(out, value) {
			t.Errorf("the audit printed the value of a credential (%s); it reports key names only. Output:\n%s", value, out)
		}
	}
	logged, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read ssh argv log: %v", err)
	}
	for _, value := range auditPlaceholderValues {
		if strings.Contains(string(logged), value) {
			t.Errorf("a credential value (%s) reached an ssh argv:\n%s", value, logged)
		}
	}
}

// --- wiring: it runs at the end of a deploy --------------------------------

// TestDeployRunsTheCredentialAuditAtTheEndOfBothLanes. An audit nobody calls
// detects nothing, and calling it early would audit the environment the
// deploy was about to change rather than the one it shipped.
func TestDeployRunsTheCredentialAuditAtTheEndOfBothLanes(t *testing.T) {
	script := deployScriptText(t)
	thorStart := strings.Index(script, "  thor*)")
	orinStart := strings.Index(script, "  orin*)")
	if thorStart < 0 || orinStart < 0 || orinStart < thorStart {
		t.Fatalf("cannot locate the thor*/orin* host lanes in deploy.sh")
	}
	lanes := map[string]string{
		"thor": script[thorStart:orinStart],
		"orin": script[orinStart:],
	}
	for host, lane := range lanes {
		call := strings.Index(lane, "audit-credentials.sh")
		if call < 0 {
			t.Errorf("the %s lane of deploy.sh never runs audit-credentials.sh", host)
			continue
		}
		up := strings.LastIndex(lane[:call], "docker compose")
		if up < 0 {
			t.Errorf("the %s lane runs audit-credentials.sh before it brings the stack up; the audit reports on what was shipped, so it goes last", host)
		}
	}
}
