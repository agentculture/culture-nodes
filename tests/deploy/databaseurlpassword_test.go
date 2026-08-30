// Package deploytest -- task t25, issue #133: NODES_DATABASE_URL and
// POSTGRES_PASSWORD are two copies of one fact, and nothing noticed when they
// stopped agreeing.
//
// The URL is composed ON THE HOST from that host's POSTGRES_PASSWORD line and
// is then add-if-absent, so it is written once and never revisited. A
// FORCE_PROD=1 rotation replaces POSTGRES_PASSWORD and leaves the URL holding
// the value the database no longer accepts. Both keys are present, both are
// non-empty, the deploy log is clean, and the audit -- whose whole subject is
// prod.env being complete -- reports both as `required (present)`. The stack
// then fails authentication on its next restart, which may be hours later:
// the same latency that made the NODES_ACTOR_CLAUDE_TOKEN incident an
// 18-hour outage rather than a deploy failure.
//
// Two behaviours are pinned here, one per direction:
//
//   - the AUDIT reports the divergence by name (and by name only -- the
//     comparison happens on the host and only a verdict word crosses the ssh
//     channel, so neither password is ever printed or argv'd);
//   - the ROTATION refreshes the URL it can prove it composed, and refuses by
//     name the one it cannot.
//
// Behavioural, in this package's established shape: the real scripts run
// against the stub ssh, and the assertions read the prod.env that resulted.
package deploytest

import (
	"strings"
	"testing"
)

// The two passwords a divergence is made of. Both are visibly inert and
// greppable: several assertions below check that NEITHER reaches the audit's
// output, which is only meaningful if they would be findable if they had.
const (
	livePostgresPassword  = "live-bundled-postgres-password"
	stalePostgresPassword = "stale-pre-rotation-postgres-password"
)

// auditProdEnvWithDatabaseURL is auditProdEnvComplete with a REAL database URL
// in place of its inert placeholder, plus any extra lines a test needs.
//
// auditProdEnvComplete deliberately sets NODES_DATABASE_URL to a value that is
// not a URL at all, which is the right fixture for "is the key present" and
// the wrong one for "do the two copies agree" -- a string with no userinfo has
// no password to compare. This builds the shape a deployed host really has.
func auditProdEnvWithDatabaseURL(postgresPassword, urlPassword, extra string) string {
	body := withoutKey(withoutKey(auditProdEnvComplete, "POSTGRES_PASSWORD"), "NODES_DATABASE_URL")
	return body +
		"POSTGRES_PASSWORD=" + postgresPassword + "\n" +
		"NODES_DATABASE_URL=postgres://nodes:" + urlPassword + "@postgres:5432/nodes?sslmode=disable\n" +
		extra
}

// --- #133, the detector half ----------------------------------------------

// TestAuditReportsADatabaseURLPasswordThatDivergesFromPostgresPassword is the
// defect in executable form: prod.env carries both keys, both non-empty, and
// they disagree. Every check the audit had before this task passes on that
// file, which is exactly why the divergence was silent.
func TestAuditReportsADatabaseURLPasswordThatDivergesFromPostgresPassword(t *testing.T) {
	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", auditProdEnvWithDatabaseURL(livePostgresPassword, stalePostgresPassword, ""))

	stdout, stderr, code := c.runSplit(t, auditScriptPath(t), []string{"thor"})
	if code == 0 {
		t.Fatalf("the audit passed a prod.env whose NODES_DATABASE_URL carries a password POSTGRES_PASSWORD does not; the stack fails auth on its next restart and nothing said so\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	for _, key := range []string{"NODES_DATABASE_URL", "POSTGRES_PASSWORD"} {
		if !strings.Contains(stderr, key) {
			t.Errorf("the divergence report does not name %s; a finding that does not name both copies does not say what to fix\nstderr:\n%s", key, stderr)
		}
	}
	// The whole point of comparing on the host: the verdict travels, the
	// values do not.
	for _, value := range []string{livePostgresPassword, stalePostgresPassword} {
		if strings.Contains(stdout+stderr, value) {
			t.Errorf("the audit printed a database password (%s) while reporting the divergence; it reports key NAMES\nstdout:\n%s\nstderr:\n%s", value, stdout, stderr)
		}
	}
}

// TestAuditNeverArgvsTheComparedPasswords is the other half of the secrets
// discipline: the comparison must happen on the far side of the ssh, so
// neither value may appear in a command line either.
func TestAuditNeverArgvsTheComparedPasswords(t *testing.T) {
	c := newFakeCluster(t)
	argvLog := logSSHArgv(t, c)
	c.seedProdEnv(t, "thor", auditProdEnvWithDatabaseURL(livePostgresPassword, stalePostgresPassword, ""))

	_, _, _ = c.runSplit(t, auditScriptPath(t), []string{"thor"})
	logged := readFileString(t, argvLog)
	for _, value := range []string{livePostgresPassword, stalePostgresPassword} {
		if strings.Contains(logged, value) {
			t.Errorf("a database password (%s) reached an ssh argv:\n%s", value, logged)
		}
	}
}

// TestAuditPassesWhenTheDatabaseURLCarriesThePostgresPassword is the negative
// control. A comparison that fails on the correct file too is a comparison
// nobody will keep.
func TestAuditPassesWhenTheDatabaseURLCarriesThePostgresPassword(t *testing.T) {
	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", auditProdEnvWithDatabaseURL(livePostgresPassword, livePostgresPassword, ""))

	stdout, stderr, code := c.runSplit(t, auditScriptPath(t), []string{"thor"})
	if code != 0 {
		t.Fatalf("the audit failed a prod.env whose two copies agree (exit %d)\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// TestAuditDoesNotCompareAnExternalDatabaseURL keeps the detector honest about
// the one topology where the two keys are SUPPOSED to differ.
//
// deploy/prod/README's "Bundled or external PostgreSQL" tells an operator to
// point the stack at a provider by setting NODES_DATABASE_URL to the provider
// URL and removing `bundled-postgres` from COMPOSE_PROFILES. On such a host
// POSTGRES_PASSWORD is the bundled database's password and nothing reads it,
// so a difference is the documented state rather than a defect -- and an audit
// that failed it every run would be an audit an operator learns to ignore,
// which is the failure mode the classification list already guards against.
func TestAuditDoesNotCompareAnExternalDatabaseURL(t *testing.T) {
	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", auditProdEnvWithDatabaseURL(livePostgresPassword, stalePostgresPassword, "COMPOSE_PROFILES=backup\n"))

	stdout, stderr, code := c.runSplit(t, auditScriptPath(t), []string{"thor"})
	if code != 0 {
		t.Fatalf("the audit failed a documented external-database host (exit %d); COMPOSE_PROFILES without bundled-postgres is the README's own switch\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "NODES_DATABASE_URL") {
		t.Errorf("the audit says nothing about why it did not compare the two copies; a check that silently does not run is indistinguishable from one that passed\nstdout:\n%s", stdout)
	}
}

// --- #133, the rotation half ----------------------------------------------

// rotationSeed is a provisioned host as a rotation really finds it: the
// accreted prod.env plus the NODES_DATABASE_URL the settings lane composed
// from that file's own POSTGRES_PASSWORD on some earlier run.
func rotationSeed(host, urlPassword string) string {
	return accretedProdEnv +
		"NODES_DATABASE_URL=postgres://nodes:" + urlPassword + "@" + databaseHostOf[host] + ":5432/nodes?sslmode=disable\n"
}

// TestProdEnvRotationRefreshesTheDatabaseURLPassword is #133 itself.
//
// A confirmed FORCE_PROD=1 rotation mints a new POSTGRES_PASSWORD. The URL
// beside it was composed from the OLD one and is add-if-absent, so nothing
// else in the script will ever revisit it. Leaving it is how the two copies
// diverge; the rotation must carry the new value into the URL it can prove it
// composed -- proof being that the URL's own password is the value being
// rotated away.
func TestProdEnvRotationRefreshesTheDatabaseURLPassword(t *testing.T) {
	c := newFakeCluster(t)
	for _, host := range []string{"thor", "orin"} {
		c.seedProdEnv(t, host, rotationSeed(host, seededPostgresPassword))
		c.confirmRotation(t, host)
	}

	stdout, stderr, code := c.runSplit(t, installSecretsPath(t), []string{"thor", "orin"}, "FORCE_PROD=1")
	if code != 0 {
		t.Fatalf("rotation exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	for _, host := range []string{"thor", "orin"} {
		path := c.prodEnvPath(t, host)
		env := readEnvFile(t, path)
		env.assertNoDuplicateKeys(t, path)

		rotated := env.values["POSTGRES_PASSWORD"]
		if !hexSecret.MatchString(rotated) {
			t.Fatalf("%s: POSTGRES_PASSWORD = %q, want a freshly generated secret", host, rotated)
		}
		url := env.values["NODES_DATABASE_URL"]
		if strings.Contains(url, seededPostgresPassword) {
			t.Errorf("%s: NODES_DATABASE_URL still carries the pre-rotation password; both keys are present and non-empty, the deploy log is clean, and the stack fails auth on its next restart -- that is issue #133", host)
		}
		if want := "postgres://nodes:" + rotated + "@" + databaseHostOf[host] + ":5432/nodes?sslmode=disable"; url != want {
			t.Errorf("%s: NODES_DATABASE_URL = %q, want %q -- only the password changes; the host, the database and the TLS mode are the host's own", host, url, want)
		}
		// A refresh that announced the new value would put a live password in
		// every deploy log.
		if strings.Contains(stdout+stderr, rotated) {
			t.Errorf("%s: the rotation printed the new database password; every lane in this script reports key names only", host)
		}
	}
}

// TestProdEnvRotationRefusesToRefreshAURLItDidNotCompose is the other side of
// the same rule, and the reason the refresh is conditional rather than
// unconditional.
//
// On an external-database host the URL carries a password the provider issued
// and POSTGRES_PASSWORD is a bundled-database credential nothing reads.
// Rewriting the URL's password there would point the whole stack at a database
// with a credential no database has ever accepted -- turning a rotation of an
// unused key into an outage. The rotation therefore refreshes only a URL whose
// password is the value being rotated away, and names what it left alone.
func TestProdEnvRotationRefusesToRefreshAURLItDidNotCompose(t *testing.T) {
	const externalURL = "postgres://nodes:provider-issued-password@db.example.net:5432/nodes?sslmode=verify-full"

	c := newFakeCluster(t)
	for _, host := range []string{"thor", "orin"} {
		c.seedProdEnv(t, host, accretedProdEnv+"NODES_DATABASE_URL="+externalURL+"\n")
		c.confirmRotation(t, host)
	}

	stdout, stderr, code := c.runSplit(t, installSecretsPath(t), []string{"thor", "orin"}, "FORCE_PROD=1")
	if code != 0 {
		t.Fatalf("rotation exited %d; a URL it cannot refresh must be announced, not fatal\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	for _, host := range []string{"thor", "orin"} {
		env := readEnvFile(t, c.prodEnvPath(t, host))
		if got := env.values["NODES_DATABASE_URL"]; got != externalURL {
			t.Errorf("%s: the rotation rewrote a URL it did not compose to %q; the provider's password is not this script's to replace", host, got)
		}
	}
	for _, key := range []string{"NODES_DATABASE_URL", "POSTGRES_PASSWORD"} {
		if !strings.Contains(stderr, key) {
			t.Errorf("the rotation does not name %s when it declines to refresh the URL; an unannounced skip is indistinguishable from success in a deploy log\nstderr:\n%s", key, stderr)
		}
	}
}
