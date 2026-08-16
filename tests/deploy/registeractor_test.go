// Task t6 (codex-bridges-thor-orin): register-actor.sh is the idempotent,
// IP-only actor-registration helper operators run against the thor+orin
// production pair (see deploy/prod/README.md's "runner registry" section
// and deploy/prod/deploy.sh's own compose-exec psql pattern, which this
// script reuses by default).
//
// There is no live Postgres in this test binary, so this file tests
// honestly at two levels, the same split compose_test.go and helm_test.go
// use for their own manifest-as-Go-test checks:
//
//   - static assertions over the script's own text: it contains an INSERT
//     and never an UPDATE or DELETE statement, and it contains the IPv4
//     refusal check.
//   - behavioral assertions: the script is actually executed with PSQL_CMD
//     pointed at a fake psql -- a tiny bash script this file writes to
//     t.TempDir() that logs every query it receives and plays back a
//     canned response -- covering the three behaviors task t6 calls out:
//     a hostname endpoint is refused before any SQL runs, an unchanged row
//     produces no INSERT, and a changed endpoint INSERTs at revision+1.
//
// This mirrors the fake-executable technique used elsewhere in this repo's
// test suites (e.g. tests/fault/runnerasync_fault_test.go builds a real
// sampler binary under t.TempDir() to stand in for a dependency the test
// does not want to run for real) -- here the "binary" is a tiny shell
// script instead of a compiled Go program, because the thing under test
// only ever needs psql's argv and stdout.
package deploytest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// registerActorScriptPath locates deploy/prod/register-actor.sh from this
// test file's own path, the same runtime.Caller(0) technique
// composeFilePath uses in compose_test.go to stay independent of the
// working directory `go test` is invoked from.
func registerActorScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repo root to find register-actor.sh")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // tests/deploy -> tests -> repo root
	return filepath.Join(repoRoot, "deploy", "prod", "register-actor.sh")
}

func registerActorScriptText(t *testing.T) string {
	t.Helper()
	path := registerActorScriptPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// TestRegisterActorScriptContainsInsertOnly is the static half of the h-
// level guarantee task t6 sets: the script writes new actor revisions with
// INSERT and never touches an existing row with UPDATE or DELETE. Actor
// rows are append-only (migrations/0001_namespaces_and_identity.sql), so a
// register helper that ever issued an UPDATE or DELETE would violate that
// contract outright -- this test makes "it never does" a checked fact
// rather than a claim in a comment.
//
// The check is case-sensitive on purpose: this repo's own SQL statements
// are written in uppercase (SELECT/FROM/WHERE/INSERT INTO, see
// internal/worker/registry.go and the INSERT statements throughout
// tests/), so a real UPDATE or DELETE *statement* would appear uppercase
// too. register-actor.sh's own prose comments use lowercase "update" when
// describing the append-only policy in English, and that prose is exactly
// what this case-sensitive check is designed to leave alone.
func TestRegisterActorScriptContainsInsertOnly(t *testing.T) {
	text := registerActorScriptText(t)

	if !strings.Contains(text, "INSERT INTO actors") {
		t.Error("register-actor.sh does not contain an INSERT INTO actors statement; it should insert new actor revisions")
	}
	if strings.Contains(text, "UPDATE") {
		t.Error("register-actor.sh contains an UPDATE statement; actor rows are append-only and must never be updated")
	}
	if strings.Contains(text, "DELETE") {
		t.Error("register-actor.sh contains a DELETE statement; actor rows are append-only and must never be deleted")
	}
}

// TestRegisterActorScriptRefusesNonIPv4Hosts is the static half of the
// IP-only refusal requirement: the script must actually contain the check,
// not just claim to in its usage text. Worker containers cannot resolve
// LAN hostnames (deploy/prod/README.md's THOR_IP note), so an endpoint
// that names a hostname would silently fail at dispatch time far from
// where this script ran -- register-actor.sh is supposed to catch that
// up front instead.
func TestRegisterActorScriptRefusesNonIPv4Hosts(t *testing.T) {
	text := registerActorScriptText(t)

	if !strings.Contains(text, "numeric IPv4 address") {
		t.Error("register-actor.sh's refusal message does not mention a numeric IPv4 address requirement")
	}
	if !strings.Contains(text, "ipv4_regex") {
		t.Error("register-actor.sh does not appear to contain an IPv4-shaped host validation")
	}
}

// TestRegisterActorScriptIsBashWithStrictMode guards the shebang and
// set -euo pipefail this task's spec calls for explicitly.
func TestRegisterActorScriptIsBashWithStrictMode(t *testing.T) {
	text := registerActorScriptText(t)

	if !strings.HasPrefix(text, "#!/usr/bin/env bash\n") {
		t.Error("register-actor.sh does not start with a bash shebang")
	}
	if !strings.Contains(text, "set -euo pipefail") {
		t.Error("register-actor.sh does not set -euo pipefail")
	}
}

// --- behavioral tests: fake psql --------------------------------------

// newFakePsql writes a tiny bash script to dir that stands in for psql.
// It logs the last argv element (register-actor.sh always calls
// `$PSQL_CMD -Atc "<query>"`, so the last argument is always the query
// text) to callsLog, and dispatches a canned response by matching a
// substring of the query:
//
//   - a query against namespaces prints namespaceRow
//   - an INSERT INTO actors query is appended to insertLog verbatim,
//     rather than answered (register-actor.sh discards INSERT's output)
//   - any other query against actors prints currentRow
//
// This mirrors how a real psql -Atc invocation behaves: unaligned,
// tuples-only, pipe-delimited columns, no trailing newline requirement.
func newFakePsql(t *testing.T, dir, namespaceRow, currentRow string) (scriptPath, callsLog, insertLog string) {
	t.Helper()

	scriptPath = filepath.Join(dir, "fake-psql")
	callsLog = filepath.Join(dir, "calls.log")
	insertLog = filepath.Join(dir, "inserts.log")

	if err := os.WriteFile(callsLog, nil, 0o644); err != nil {
		t.Fatalf("seed calls log: %v", err)
	}
	if err := os.WriteFile(insertLog, nil, 0o644); err != nil {
		t.Fatalf("seed insert log: %v", err)
	}

	script := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"query=\"${@: -1}\"\n" +
		"printf '%s\\n' \"$query\" >> '" + callsLog + "'\n" +
		"case \"$query\" in\n" +
		"  *'FROM namespaces'*)\n" +
		"    printf '%s' '" + namespaceRow + "'\n" +
		"    ;;\n" +
		"  *'INSERT INTO actors'*)\n" +
		"    printf '%s\\n' \"$query\" >> '" + insertLog + "'\n" +
		"    ;;\n" +
		"  *'FROM actors'*)\n" +
		"    printf '%s' '" + currentRow + "'\n" +
		"    ;;\n" +
		"esac\n"

	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake psql: %v", err)
	}
	return scriptPath, callsLog, insertLog
}

// runRegisterActor executes register-actor.sh with the given env on top of
// a minimal PATH, and returns combined stdout+stderr and the exit code (0
// on success, matching exec.ExitError.ExitCode() otherwise).
func runRegisterActor(t *testing.T, env []string) (output string, exitCode int) {
	t.Helper()

	cmd := exec.Command("bash", registerActorScriptPath(t))
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
	t.Fatalf("run register-actor.sh: %v (output: %s)", err, output)
	return "", -1
}

// runRegisterActorArgs is runRegisterActor with argv. The flag-taking form
// exists because --metadata has no environment-variable equivalent: it is
// repeatable, and a single env var could not express two keys.
func runRegisterActorArgs(t *testing.T, env []string, args ...string) (output string, exitCode int) {
	t.Helper()

	cmd := exec.Command("bash", append([]string{registerActorScriptPath(t)}, args...)...)
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
	t.Fatalf("run register-actor.sh: %v (output: %s)", err, output)
	return "", -1
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// TestRegisterActorRefusesHostnameBeforeAnySQL is task t6's first
// behavioral case: a hostname endpoint must be refused before the script
// ever shells out to Postgres. PSQL_CMD points at a path that does not
// exist, so if register-actor.sh tried to run it at all, the failure mode
// would be "no such file or directory" from the shell, not this script's
// own refusal message -- proving the refusal really does happen first.
func TestRegisterActorRefusesHostnameBeforeAnySQL(t *testing.T) {
	env := []string{
		"PSQL_CMD=/nonexistent/should-not-run/psql",
		"ACTOR_KEY=company/codex-thor",
		"ENDPOINT_URL=http://thor:17070",
		"AUTH_TOKEN_ENV=CODEX_THOR_TOKEN",
	}
	output, exitCode := runRegisterActor(t, env)

	if exitCode != 1 {
		t.Errorf("exit code = %d, want 1; output: %s", exitCode, output)
	}
	if !strings.Contains(output, "numeric IPv4 address") {
		t.Errorf("output does not name the IPv4 refusal rule; output: %s", output)
	}
	if strings.Contains(output, "No such file or directory") || strings.Contains(output, "not found") {
		t.Errorf("output suggests PSQL_CMD was actually invoked before refusal; output: %s", output)
	}
}

// TestRegisterActorRefusesSchemelessEndpoint covers the PR #20 review
// finding: an endpoint like "192.168.1.5:8086" parses as IPv4 but would be
// persisted scheme-less and then fail when the worker builds HTTP requests
// from it. The script must require an explicit http(s):// scheme, refusing
// before any SQL runs (same nonexistent-PSQL_CMD technique as the hostname
// case).
func TestRegisterActorRefusesSchemelessEndpoint(t *testing.T) {
	env := []string{
		"PSQL_CMD=/nonexistent/should-not-run/psql",
		"ACTOR_KEY=company/codex-thor",
		"ENDPOINT_URL=192.168.1.5:8086",
		"AUTH_TOKEN_ENV=CODEX_THOR_TOKEN",
	}
	output, exitCode := runRegisterActor(t, env)

	if exitCode != 1 {
		t.Errorf("exit code = %d, want 1; output: %s", exitCode, output)
	}
	if !strings.Contains(output, "http:// or https://") {
		t.Errorf("output does not name the explicit-scheme rule; output: %s", output)
	}
	if strings.Contains(output, "No such file or directory") || strings.Contains(output, "not found") {
		t.Errorf("output suggests PSQL_CMD was invoked before refusal; output: %s", output)
	}
}

// TestRegisterActorRefusesSQLMetacharacters covers the PR #20 review
// finding on SQL interpolation: every value that reaches a SQL string is
// confined to a strict allowlist, so inputs carrying quotes or statement
// metacharacters are refused before any Postgres access -- allowlist
// validation as the shell-native equivalent of parameterization.
func TestRegisterActorRefusesSQLMetacharacters(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{
			name: "quote in actor key",
			env: []string{
				"ACTOR_KEY=company/x'; DROP TABLE actors; --",
				"ENDPOINT_URL=http://192.168.1.5:8086",
				"AUTH_TOKEN_ENV=CODEX_THOR_TOKEN",
			},
			want: "refusing actor key",
		},
		{
			name: "metacharacters in auth token env name",
			env: []string{
				"ACTOR_KEY=company/codex-thor",
				"ENDPOINT_URL=http://192.168.1.5:8086",
				"AUTH_TOKEN_ENV=X'); DELETE FROM actors; --",
			},
			want: "refusing auth token env name",
		},
		{
			name: "quote smuggled into endpoint path",
			env: []string{
				"ACTOR_KEY=company/codex-thor",
				"ENDPOINT_URL=http://192.168.1.5:8086/x',''); --",
				"AUTH_TOKEN_ENV=CODEX_THOR_TOKEN",
			},
			want: "refusing endpoint",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := append([]string{"PSQL_CMD=/nonexistent/should-not-run/psql"}, tc.env...)
			output, exitCode := runRegisterActor(t, env)
			if exitCode != 1 {
				t.Errorf("exit code = %d, want 1; output: %s", exitCode, output)
			}
			if !strings.Contains(output, tc.want) {
				t.Errorf("output does not carry %q; output: %s", tc.want, output)
			}
			if strings.Contains(output, "No such file or directory") || strings.Contains(output, "not found") {
				t.Errorf("output suggests PSQL_CMD was invoked before refusal; output: %s", output)
			}
		})
	}
}

// TestRegisterActorUnchangedRowIssuesNoInsert is task t6's second
// behavioral case: when the newest revision's endpoint_ref and
// metadata.auth_token_env already match what was asked for, the script
// must report "unchanged" and must not write anything.
func TestRegisterActorUnchangedRowIssuesNoInsert(t *testing.T) {
	dir := t.TempDir()
	psqlPath, _, insertLog := newFakePsql(t, dir,
		"ns-1",
		"3|http://192.168.1.5:17070|t",
	)

	env := []string{
		"PSQL_CMD=" + psqlPath,
		"NODES_NAMESPACE_ID=ns-1",
		"ACTOR_KEY=company/codex-thor",
		"ENDPOINT_URL=http://192.168.1.5:17070",
		"AUTH_TOKEN_ENV=CODEX_THOR_TOKEN",
	}
	output, exitCode := runRegisterActor(t, env)

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", exitCode, output)
	}
	if !strings.Contains(output, "unchanged (revision 3)") {
		t.Errorf("output = %q, want it to report unchanged at revision 3", output)
	}

	inserted, err := os.ReadFile(insertLog)
	if err != nil {
		t.Fatalf("read insert log: %v", err)
	}
	if len(inserted) != 0 {
		t.Errorf("an unchanged row must issue no INSERT, but the fake psql recorded: %s", inserted)
	}
}

// TestRegisterActorChangedEndpointInsertsNextRevision is task t6's third
// behavioral case: a genuinely different endpoint must produce exactly one
// INSERT at revision+1, carrying the new endpoint and auth_token_env.
func TestRegisterActorChangedEndpointInsertsNextRevision(t *testing.T) {
	dir := t.TempDir()
	psqlPath, _, insertLog := newFakePsql(t, dir,
		"ns-1",
		"3|http://192.168.1.5:17070|t",
	)

	env := []string{
		"PSQL_CMD=" + psqlPath,
		"NODES_NAMESPACE_ID=ns-1",
		"ACTOR_KEY=company/codex-thor",
		"ENDPOINT_URL=http://192.168.1.9:17070",
		"AUTH_TOKEN_ENV=CODEX_THOR_TOKEN",
	}
	output, exitCode := runRegisterActor(t, env)

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", exitCode, output)
	}
	if !strings.Contains(output, "revision 4") {
		t.Errorf("output = %q, want it to report the new revision 4", output)
	}

	inserted, err := os.ReadFile(insertLog)
	if err != nil {
		t.Fatalf("read insert log: %v", err)
	}
	insertedText := string(inserted)
	if !strings.Contains(insertedText, "INSERT INTO actors") {
		t.Fatalf("no INSERT was recorded for a changed endpoint; insert log: %q", insertedText)
	}
	if !strings.Contains(insertedText, ", 4,") {
		t.Errorf("INSERT does not carry revision 4; insert log: %q", insertedText)
	}
	if !strings.Contains(insertedText, "'http://192.168.1.9:17070'") {
		t.Errorf("INSERT does not carry the new endpoint; insert log: %q", insertedText)
	}
	if !strings.Contains(insertedText, "company/codex-thor") {
		t.Errorf("INSERT does not carry the actor key; insert log: %q", insertedText)
	}

	calls, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		t.Fatalf("read calls log: %v", err)
	}
	if strings.Contains(string(calls), "UPDATE") || strings.Contains(string(calls), "DELETE") {
		t.Errorf("a changed endpoint issued an UPDATE or DELETE against Postgres; calls: %q", calls)
	}
}

// TestRegisterActorMergesMetadataInsteadOfReplacingIt is the guard for the
// hazard found while registering handover_remote across the fleet (task t9).
//
// Every registration writes a NEW ROW. The previous implementation built that
// row's metadata from a hardcoded `{"auth_token_env": ...}` literal, so any
// later registration silently dropped every other key -- and once an actor
// carries handover_remote, dropping it makes scripts/collect-handover.py fall
// back to a template or fail outright, with nothing pointing at the cause.
//
// The fix is to carry the prior revision's metadata forward inside Postgres
// (INSERT ... SELECT ... metadata || overlay) rather than through the shell,
// which also keeps stored JSON from ever being re-interpolated into the
// statement. This test pins the merge, not the particular keys.
func TestRegisterActorMergesMetadataInsteadOfReplacingIt(t *testing.T) {
	dir := t.TempDir()
	psqlPath, _, insertLog := newFakePsql(t, dir,
		"ns-1",
		"3|http://192.168.1.5:17070|f", // the overlay is NOT already present
	)

	env := []string{
		"PSQL_CMD=" + psqlPath,
		"NODES_NAMESPACE_ID=ns-1",
		"ACTOR_KEY=company/codex-thor",
		"ENDPOINT_URL=http://192.168.1.5:17070",
		"AUTH_TOKEN_ENV=CODEX_THOR_TOKEN",
	}
	output, exitCode := runRegisterActorArgs(t, env,
		"--metadata", "handover_remote=ssh://thor/~/git/culture-nodes-agent")

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", exitCode, output)
	}

	inserted, err := os.ReadFile(insertLog)
	if err != nil {
		t.Fatalf("read insert log: %v", err)
	}
	insertedText := string(inserted)
	if !strings.Contains(insertedText, "metadata || ") {
		t.Errorf("INSERT must MERGE the previous revision's metadata, but no `metadata || ` appears; insert log: %q", insertedText)
	}
	if !strings.Contains(insertedText, "INSERT INTO actors") || !strings.Contains(insertedText, "SELECT") {
		t.Errorf("a merge needs INSERT ... SELECT so the prior row supplies the carried metadata; insert log: %q", insertedText)
	}
	if !strings.Contains(insertedText, "handover_remote") {
		t.Errorf("INSERT does not carry the requested metadata key; insert log: %q", insertedText)
	}
	// kind and protocol must be carried forward, not re-asserted: hardcoding
	// 'agent'/'http' could not register the human-inbox actor at all, and would
	// silently rewrite a human or runner row into an agent one.
	if strings.Contains(insertedText, "'agent', 'http'") {
		t.Errorf("INSERT re-asserts kind/protocol instead of carrying them forward; insert log: %q", insertedText)
	}
}

// TestRegisterActorMetadataOnlyChangeIsNotReportedUnchanged pins the other
// half of the same fix. The idempotency check used to compare only endpoint
// and auth_token_env, so a registration whose entire purpose was to ADD a
// metadata key reported "unchanged" and wrote nothing.
func TestRegisterActorMetadataOnlyChangeIsNotReportedUnchanged(t *testing.T) {
	dir := t.TempDir()
	psqlPath, _, insertLog := newFakePsql(t, dir,
		"ns-1",
		"3|http://192.168.1.5:17070|f", // same endpoint, overlay not yet present
	)

	env := []string{
		"PSQL_CMD=" + psqlPath,
		"NODES_NAMESPACE_ID=ns-1",
		"ACTOR_KEY=company/codex-thor",
		"ENDPOINT_URL=http://192.168.1.5:17070",
		"AUTH_TOKEN_ENV=CODEX_THOR_TOKEN",
	}
	output, exitCode := runRegisterActorArgs(t, env,
		"--metadata", "handover_remote=ssh://thor/~/git/culture-nodes-agent")

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", exitCode, output)
	}
	if strings.Contains(output, "unchanged") {
		t.Errorf("a metadata-only change must not report unchanged; output: %q", output)
	}
	inserted, err := os.ReadFile(insertLog)
	if err != nil {
		t.Fatalf("read insert log: %v", err)
	}
	if len(inserted) == 0 {
		t.Error("a metadata-only change wrote no INSERT, so the new key would never reach the registry")
	}
}

// TestRegisterActorRefusesUnsafeMetadata: metadata values are interpolated
// into a JSON literal which is itself interpolated into SQL, so a value that
// needs escaping is refused rather than escaped -- the same shell-native
// parameterization the endpoint and actor-key checks use, one layer deeper.
func TestRegisterActorRefusesUnsafeMetadata(t *testing.T) {
	for _, bad := range []string{
		"handover_remote=ssh://thor/'; DROP TABLE actors; --",
		`handover_remote=a"b`,
		`handover_remote=a\b`,
		"bad key=value",
		"handover_remote=",
	} {
		t.Run(bad, func(t *testing.T) {
			env := []string{
				"PSQL_CMD=/nonexistent/should-not-run/psql",
				"NODES_NAMESPACE_ID=ns-1",
				"ACTOR_KEY=company/codex-thor",
				"ENDPOINT_URL=http://192.168.1.5:17070",
			}
			output, exitCode := runRegisterActorArgs(t, env, "--metadata", bad)
			if exitCode == 0 {
				t.Fatalf("register-actor.sh accepted unsafe metadata %q; output: %s", bad, output)
			}
			if !strings.Contains(output, "refusing") || !strings.Contains(output, "metadata") {
				t.Errorf("refusal for %q did not name metadata as the cause; output: %q", bad, output)
			}
			// The refusal must happen before any Postgres access, exactly as
			// the hostname refusal does -- PSQL_CMD points nowhere, so a
			// "no such file" message would prove the check ran too late.
			if strings.Contains(output, "No such file") || strings.Contains(output, "not found") {
				t.Errorf("metadata was validated only after shelling out to psql; output: %q", output)
			}
		})
	}
}

// TestRegisterActorAbsentRowInsertsRevisionOne covers the third case named
// in register-actor.sh's own spec ("or 1 when absent"): a key with no
// existing row at all must be inserted at revision 1, not treated as an
// error.
func TestRegisterActorAbsentRowInsertsRevisionOne(t *testing.T) {
	dir := t.TempDir()
	psqlPath, _, insertLog := newFakePsql(t, dir,
		"ns-1",
		"", // no current row for this actor key
	)

	env := []string{
		"PSQL_CMD=" + psqlPath,
		"NODES_NAMESPACE_ID=ns-1",
		"ACTOR_KEY=company/codex-orin",
		"ENDPOINT_URL=http://192.168.1.9:17070",
		"AUTH_TOKEN_ENV=CODEX_ORIN_TOKEN",
	}
	output, exitCode := runRegisterActor(t, env)

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", exitCode, output)
	}
	if !strings.Contains(output, "revision 1") {
		t.Errorf("output = %q, want it to report revision 1 for a first-time registration", output)
	}

	inserted, err := os.ReadFile(insertLog)
	if err != nil {
		t.Fatalf("read insert log: %v", err)
	}
	if !strings.Contains(string(inserted), ", 1,") {
		t.Errorf("INSERT does not carry revision 1; insert log: %q", inserted)
	}
}

// TestRegisterActorResolvesNamespaceWhenUnset covers deploy.sh's own
// namespace-resolution convention: when NODES_NAMESPACE_ID is not set, the
// script must query for it (the oldest namespaces row) rather than fail.
func TestRegisterActorResolvesNamespaceWhenUnset(t *testing.T) {
	dir := t.TempDir()
	psqlPath, callsLog, insertLog := newFakePsql(t, dir,
		"ns-resolved",
		"",
	)

	env := []string{
		"PSQL_CMD=" + psqlPath,
		"ACTOR_KEY=company/codex-thor",
		"ENDPOINT_URL=http://192.168.1.5:17070",
		"AUTH_TOKEN_ENV=CODEX_THOR_TOKEN",
	}
	output, exitCode := runRegisterActor(t, env)

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", exitCode, output)
	}

	calls, err := os.ReadFile(callsLog)
	if err != nil {
		t.Fatalf("read calls log: %v", err)
	}
	if !strings.Contains(string(calls), "FROM namespaces") {
		t.Errorf("register-actor.sh did not query namespaces when NODES_NAMESPACE_ID was unset; calls: %q", calls)
	}

	inserted, err := os.ReadFile(insertLog)
	if err != nil {
		t.Fatalf("read insert log: %v", err)
	}
	if !strings.Contains(string(inserted), "'ns-resolved'") {
		t.Errorf("INSERT does not use the resolved namespace id; insert log: %q, output: %s", inserted, output)
	}
}
