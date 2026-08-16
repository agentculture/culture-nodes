// Package deploytest -- the deployment-settings lane (issue #124).
//
// Split out of prodenvmerge_test.go when that file crossed the repo's
// 1000-line hard limit. Same package, so the fakeCluster harness, the
// accretedProdEnv/accretedKeys fixtures and readEnvFile are all shared --
// these tests deliberately reuse the harness that pins the merge itself,
// because the lane's whole contract is expressed through that merge.
package deploytest

import (
	"strings"
	"testing"
)

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
