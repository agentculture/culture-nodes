// Package deploytest -- this file is login-from-anywhere task t13's:
// scripts/bind-identity.sh is the third place of the three-place onboarding
// recipe (spec c46: Access allow policy, registered human actor revision,
// actor_identities binding) and the retire-the-binding half of offboarding.
//
// There is no live Postgres in this test binary, so this file tests at the
// same two levels registeractor_test.go does:
//
//   - static assertions over the script's own text: it INSERTs into
//     actor_identities, its only UPDATE stamps revoked_at, and it never
//     DELETEs -- bindings are append-only history like actor rows.
//   - behavioral assertions: the script is executed for real. The two
//     refusals the task calls out -- an invalid role and an unknown
//     provider -- are checked with PSQL_CMD pointed at a path that does
//     not exist, so any attempt to reach Postgres would surface as a shell
//     "No such file" failure rather than the script's own refusal text.
//     The bind and revoke paths run against a fake psql (a bash script in
//     t.TempDir() that logs every query it is handed and plays back canned
//     rows), the technique newFakePsql already uses next door.
package deploytest

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func bindIdentityScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repo root to find bind-identity.sh")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // tests/deploy -> tests -> repo root
	return filepath.Join(repoRoot, "scripts", "bind-identity.sh")
}

func bindIdentityScriptText(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(bindIdentityScriptPath(t))
	if err != nil {
		t.Fatalf("read bind-identity.sh: %v", err)
	}
	return string(raw)
}

// runBindIdentity executes the script with argv and an environment built
// from scratch (PATH only, plus what the test passes), and returns combined
// output and the exit code.
func runBindIdentity(t *testing.T, env []string, args ...string) (output string, exitCode int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{bindIdentityScriptPath(t)}, args...)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	out, err := cmd.CombinedOutput()
	output = string(out)
	if err == nil {
		return output, 0
	}
	var exitErr *exec.ExitError
	if asExitError(err, &exitErr) {
		return output, exitErr.ExitCode()
	}
	t.Fatalf("run bind-identity.sh: %v (output: %s)", err, output)
	return "", -1
}

// newFakeIdentityPsql stands in for psql the way newFakePsql does, with
// canned answers for the queries bind-identity.sh issues: the actor lookup
// returns actorRow, an INSERT echoes an identity id, an UPDATE echoes
// updateRow (empty means "no live row matched").
func newFakeIdentityPsql(t *testing.T, actorRow, updateRow string) (scriptPath, callsLog string) {
	t.Helper()
	dir := t.TempDir()
	scriptPath = filepath.Join(dir, "fake-psql")
	callsLog = filepath.Join(dir, "calls.log")
	if err := os.WriteFile(callsLog, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"query=\"${@: -1}\"\n" +
		"printf '%s\\n' \"$query\" >> '" + callsLog + "'\n" +
		"case \"$query\" in\n" +
		"  *'INSERT INTO actor_identities'*) printf '%s' 'identity_fake' ;;\n" +
		"  *'UPDATE actor_identities'*) printf '%s' '" + updateRow + "' ;;\n" +
		"  *'FROM actors'*) printf '%s' '" + actorRow + "' ;;\n" +
		"  *) printf '%s' '' ;;\n" +
		"esac\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return scriptPath, callsLog
}

func TestBindIdentityScriptIsBashWithStrictMode(t *testing.T) {
	text := bindIdentityScriptText(t)
	if !strings.HasPrefix(text, "#!/usr/bin/env bash\n") {
		t.Error("bind-identity.sh does not start with a bash shebang")
	}
	if !strings.Contains(text, "set -euo pipefail") {
		t.Error("bind-identity.sh does not set -euo pipefail")
	}
	if out, err := exec.Command("bash", "-n", bindIdentityScriptPath(t)).CombinedOutput(); err != nil {
		t.Fatalf("bash -n bind-identity.sh: %v\n%s", err, out)
	}
}

// TestBindIdentityScriptIsAppendOnly pins the history shape migration 0053
// sets: bindings are inserted, revocation stamps revoked_at, nothing is
// ever deleted. The UPDATE check is precise rather than a blanket ban: the
// one UPDATE the script may issue is the revoked_at stamp, and any other
// column assignment is a violation.
func TestBindIdentityScriptIsAppendOnly(t *testing.T) {
	text := bindIdentityScriptText(t)
	if !strings.Contains(text, "INSERT INTO actor_identities") {
		t.Error("bind-identity.sh does not INSERT INTO actor_identities")
	}
	if strings.Contains(text, "DELETE") {
		t.Error("bind-identity.sh contains a DELETE statement; bindings are append-only history")
	}
	updates := regexp.MustCompile(`UPDATE actor_identities SET ([a-z_]+) = `).FindAllStringSubmatch(text, -1)
	if len(updates) != 1 {
		t.Fatalf("bind-identity.sh has %d UPDATE actor_identities statements, want exactly one (the revoke)", len(updates))
	}
	if updates[0][1] != "revoked_at" {
		t.Errorf("bind-identity.sh's UPDATE assigns %q; the only permitted assignment is revoked_at", updates[0][1])
	}
	if !strings.Contains(text, "AND revoked_at IS NULL RETURNING id") {
		t.Error("bind-identity.sh's revoke must match only a live row and RETURN its id so a no-op revoke is reported, not silently accepted")
	}
}

// TestBindIdentityRefusesInvalidRoleBeforeAnySQL: PSQL_CMD is a path that
// does not exist. If the script reached for Postgres at all, bash would
// report "No such file or directory" instead of the script's own refusal.
func TestBindIdentityRefusesInvalidRoleBeforeAnySQL(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "psql-must-not-run")
	env := []string{"PSQL_CMD=" + missing, "NODES_NAMESPACE_ID=namespace_1"}
	output, code := runBindIdentity(t, env, "bind",
		"--provider", "cloudflare-access", "--subject", "user-123",
		"--actor-key", "company/alice", "--roles", "approver,superuser")
	if code != 1 {
		t.Fatalf("invalid role: exit=%d, want 1; output: %s", code, output)
	}
	if !strings.Contains(output, "unknown role 'superuser'") {
		t.Errorf("invalid role refusal should name the role; got: %s", output)
	}
	if strings.Contains(output, "No such file") {
		t.Errorf("bind-identity.sh reached for psql before refusing the role: %s", output)
	}
}

func TestBindIdentityRefusesUnknownProviderBeforeAnySQL(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "psql-must-not-run")
	env := []string{"PSQL_CMD=" + missing, "NODES_NAMESPACE_ID=namespace_1"}
	output, code := runBindIdentity(t, env, "bind",
		"--provider", "okta", "--subject", "user-123",
		"--actor-key", "company/alice", "--roles", "approver")
	if code != 1 {
		t.Fatalf("unknown provider: exit=%d, want 1; output: %s", code, output)
	}
	if !strings.Contains(output, "unknown provider 'okta'") {
		t.Errorf("unknown provider refusal should name the provider; got: %s", output)
	}
	if strings.Contains(output, "No such file") {
		t.Errorf("bind-identity.sh reached for psql before refusing the provider: %s", output)
	}
}

// TestBindIdentityRefusesUnregisteredActor: the actor lookup answers with no
// row, so the bind must stop before it INSERTs and point at register-actor.
func TestBindIdentityRefusesUnregisteredActor(t *testing.T) {
	fake, calls := newFakeIdentityPsql(t, "", "")
	env := []string{"PSQL_CMD=" + fake, "NODES_NAMESPACE_ID=namespace_1"}
	output, code := runBindIdentity(t, env, "bind",
		"--provider", "cloudflare-access", "--subject", "user-123",
		"--actor-key", "company/alice", "--roles", "approver")
	if code != 1 || !strings.Contains(output, "no actor registered under key 'company/alice'") {
		t.Fatalf("unregistered actor: exit=%d output=%q", code, output)
	}
	log, _ := os.ReadFile(calls)
	if strings.Contains(string(log), "INSERT") {
		t.Errorf("bind INSERTed despite an unregistered actor:\n%s", log)
	}
}

func TestBindIdentityBindInsertsLiveBinding(t *testing.T) {
	fake, calls := newFakeIdentityPsql(t, "actor_alice_1", "")
	env := []string{"PSQL_CMD=" + fake, "NODES_NAMESPACE_ID=namespace_1"}
	output, code := runBindIdentity(t, env, "bind",
		"--provider", "cloudflare-access", "--subject", "7c1d9e2a-user",
		"--actor-key", "company/alice", "--roles", "approver,viewer")
	if code != 0 {
		t.Fatalf("bind exit=%d output=%q", code, output)
	}
	log, _ := os.ReadFile(calls)
	queries := string(log)
	for _, want := range []string{
		"INSERT INTO actor_identities (id, namespace_id, provider, subject, actor_id, roles)",
		"'namespace_1', 'cloudflare-access', '7c1d9e2a-user', 'actor_alice_1', ARRAY['approver','viewer']::TEXT[]",
	} {
		if !strings.Contains(queries, want) {
			t.Errorf("bind INSERT lacks %q; queries were:\n%s", want, queries)
		}
	}
	if !strings.Contains(output, "bound cloudflare-access/7c1d9e2a-user to company/alice (actor actor_alice_1") {
		t.Errorf("bind should report provider, subject, key and actor id; got %q", output)
	}
}

func TestBindIdentityRevokeStampsRevokedAt(t *testing.T) {
	fake, calls := newFakeIdentityPsql(t, "", "identity_1")
	env := []string{"PSQL_CMD=" + fake, "NODES_NAMESPACE_ID=namespace_1"}
	output, code := runBindIdentity(t, env, "revoke", "--identity", "identity_1")
	if code != 0 || !strings.Contains(output, "revoked identity identity_1") {
		t.Fatalf("revoke exit=%d output=%q", code, output)
	}
	log, _ := os.ReadFile(calls)
	if !strings.Contains(string(log), "UPDATE actor_identities SET revoked_at = now() WHERE id = 'identity_1' AND namespace_id = 'namespace_1' AND revoked_at IS NULL RETURNING id") {
		t.Errorf("revoke issued an unexpected statement:\n%s", log)
	}

	// A second revoke of the same id matches no live row and must say so.
	fake2, _ := newFakeIdentityPsql(t, "", "")
	output, code = runBindIdentity(t, []string{"PSQL_CMD=" + fake2, "NODES_NAMESPACE_ID=namespace_1"}, "revoke", "--identity", "identity_1")
	if code != 1 || !strings.Contains(output, "no live binding with id 'identity_1'") {
		t.Fatalf("second revoke exit=%d output=%q, want a named refusal", code, output)
	}
}

// TestBindIdentitySubjectIsNotAnEmailColumn: an email-shaped subject is
// accepted (Cloudflare could in principle issue one) but warned about,
// because spec c37 keys bindings by the provider's stable subject and an
// operator pasting the login email instead of the `sub` claim is the
// likeliest onboarding mistake.
func TestBindIdentityWarnsOnEmailShapedSubject(t *testing.T) {
	fake, _ := newFakeIdentityPsql(t, "actor_alice_1", "")
	env := []string{"PSQL_CMD=" + fake, "NODES_NAMESPACE_ID=namespace_1"}
	output, code := runBindIdentity(t, env, "bind",
		"--provider", "cloudflare-access", "--subject", "alice@example.com",
		"--actor-key", "company/alice", "--roles", "viewer")
	if code != 0 {
		t.Fatalf("email-shaped subject: exit=%d output=%q", code, output)
	}
	if !strings.Contains(output, "looks like an email address") {
		t.Errorf("expected a warning about an email-shaped subject; got %q", output)
	}
}
