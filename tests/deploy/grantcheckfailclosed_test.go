// Package deploytest -- the grant preflight fails CLOSED on every way it can
// fail to complete the diff, not just on the malformed payloads PR #263 review
// finding 3 named (those are grantsafety_test.go's).
//
// The finding was about the reader coercing what it could not parse to an
// empty list. The same fail-open shape lives one layer out, in the shell around
// the reader, and it is reachable without the control plane misbehaving at all:
//
//   - the ssh probe for runner.env fails, its output is empty, empty is not
//     "no", and the lane proceeds as though the host were fine;
//   - reading NODES_API_URL off the host fails, the URL comes back empty, and
//     the lane announces UNVERIFIED and proceeds;
//   - reading the granted key NAMES off the host fails, `granted` is empty, and
//     every declared ref is reported missing -- a refusal, but for a reason
//     that is not true;
//   - the workspace cannot be created, so curl cannot write its body, and the
//     failure is reported as "the control plane did not answer";
//   - there is no python3, so nothing is diffed, and the lane says UNVERIFIED
//     and proceeds.
//
// The last one used to be a documented decline. It is not any more. A gate has
// exactly two honest answers -- "I diffed it and it is granted" or "I could not
// diff it" -- and only the second of those may let a deploy through, which it
// does by refusing. The ONE surviving exception is an unreachable control
// plane, because that is a state a deploy is often the fix for; it is pinned by
// TestGrantCheckSaysSoWhenItCannotReadTheControlPlane next door, and every test
// here asserts it stayed the exception rather than the rule.
package deploytest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// selectivelyFailingSSH replaces the fake cluster's ssh stub with one that
// refuses every remote command matching $FAKE_SSH_FAIL_PATTERN and behaves
// exactly like the original otherwise.
//
// Per-command rather than all-or-nothing on purpose: this lane makes three
// separate ssh calls for three separate facts, and a stub that broke all of
// them could only ever prove the FIRST one fails closed. Each test below
// breaks exactly one and leaves the others working, which is the only way to
// reach the later calls at all.
func selectivelyFailingSSH(t *testing.T, c *fakeCluster) {
	t.Helper()
	stub := "#!/usr/bin/env bash\n" +
		"host=$1; shift\n" +
		"if [ -n \"${FAKE_SSH_FAIL_PATTERN:-}\" ] && printf '%s' \"$*\" | grep -Eq -- \"$FAKE_SSH_FAIL_PATTERN\"; then\n" +
		"  echo \"ssh: connect to host $host port 22: Connection refused\" >&2\n" +
		"  exit 255\n" +
		"fi\n" +
		"export HOME=\"$FAKE_SSH_HOME_ROOT/$host\"\n" +
		"mkdir -p \"$HOME\"\n" +
		"exec bash -c \"$*\"\n"
	if err := os.WriteFile(filepath.Join(c.binDir, "ssh"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write selectively failing ssh: %v", err)
	}
}

// assertRefusedWithoutClaimingSuccess is the shape every case in this file
// shares: a non-zero exit, a reason on stderr naming the step that failed, and
// -- the part that matters -- no green line anywhere. A gate that refuses and
// still prints "every environment ref is granted" has taught its operator to
// read past the refusal.
func assertRefusedWithoutClaimingSuccess(t *testing.T, stdout, stderr string, code int, wants ...string) {
	t.Helper()
	if code == 0 {
		t.Fatalf("the grant check exited 0 without completing the diff\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if strings.Contains(stdout+stderr, grantCheckGreenLine) {
		t.Errorf("the check claimed the grants were diffed on a path that never diffed them\nstdout:\n%s\nstderr:\n%s",
			stdout, stderr)
	}
	for _, want := range wants {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not mention %q, so it does not name the step that failed; stderr was:\n%s",
				want, stderr)
		}
	}
}

// grantCheckWorkflows is one startable workflow whose refs the fixture host
// genuinely holds -- so every failure below is the failure under test and not
// a missing grant leaking in.
func grantCheckWorkflows(t *testing.T) []workflowVersion {
	t.Helper()
	return []workflowVersion{{WorkflowKey: "pr-upkeep-sweep-cycle", Version: 2,
		NormalizedIR: ir(t, "pr-upkeep.sweep.due", sweepRefs...)}}
}

// TestGrantCheckRefusesWhenItCannotReachTheHostAtAll -- the first ssh call, and
// the one whose fail-open is invisible. `[ "$(ssh …)" = no ]` compares the
// empty string a failed ssh produces against "no", finds them different, and
// carries on into a diff of a host it never reached.
func TestGrantCheckRefusesWhenItCannotReachTheHostAtAll(t *testing.T) {
	c := newFakeCluster(t)
	grantedHost(t, c, thorFake, fiveKeyRunnerSecrets())
	selectivelyFailingSSH(t, c)
	url := fakeControlPlane(t, grantCheckWorkflows(t), nil)

	stdout, stderr, code := runGrantCheck(t, c, thorFake, url, "FAKE_SSH_FAIL_PATTERN=runner[.]env")
	assertRefusedWithoutClaimingSuccess(t, stdout, stderr, code, "preflight failed on "+thorFake, "runner.env")
}

// TestGrantCheckRefusesWhenItCannotReadTheGrantedNamesOffTheHost -- the ssh
// call that produces one half of the diff. A failure here left `granted` empty,
// which does refuse the deploy, but for a reason that is FALSE: it names every
// key the workflow declares as missing from a host that may hold all of them.
// An operator who acts on that goes and re-grants five keys that were never
// gone.
func TestGrantCheckRefusesWhenItCannotReadTheGrantedNamesOffTheHost(t *testing.T) {
	c := newFakeCluster(t)
	grantedHost(t, c, thorFake, fiveKeyRunnerSecrets())
	selectivelyFailingSSH(t, c)
	url := fakeControlPlane(t, grantCheckWorkflows(t), nil)

	stdout, stderr, code := runGrantCheck(t, c, thorFake, url, "FAKE_SSH_FAIL_PATTERN=runner-secrets[.]env")
	assertRefusedWithoutClaimingSuccess(t, stdout, stderr, code, "preflight failed on "+thorFake, "granted key")
	// The refusal must not blame the host for grants it was never able to look
	// at. GITHUB_TOKEN is in the fixture's runner-secrets.env.
	if strings.Contains(stderr, "missing: GITHUB_TOKEN") {
		t.Errorf("the refusal reports a granted key as missing because the read failed; that sends an "+
			"operator to re-grant keys the host already has. stderr was:\n%s", stderr)
	}
}

// TestGrantCheckRefusesWhenItCannotReadTheAPIURLOffTheHost -- the third ssh
// call, reached only when the operator's shell names no control plane. A
// failure came back as an empty URL, and an empty URL was announced as
// "no control-plane URL … UNVERIFIED" and proceeded.
func TestGrantCheckRefusesWhenItCannotReadTheAPIURLOffTheHost(t *testing.T) {
	c := newFakeCluster(t)
	grantedHost(t, c, thorFake, fiveKeyRunnerSecrets())
	selectivelyFailingSSH(t, c)

	// An empty NODES_API_URL is what sends the lane to the host for one.
	stdout, stderr, code := runGrantCheck(t, c, thorFake, "", "FAKE_SSH_FAIL_PATTERN=NODES_API_URL")
	assertRefusedWithoutClaimingSuccess(t, stdout, stderr, code, "preflight failed on "+thorFake, "NODES_API_URL")
}

// TestGrantCheckRefusesWhenNothingNamesAControlPlane -- the same step
// succeeding and finding nothing. This is not an unreachable control plane: it
// is a runner.env missing the key lanes/runner-env-write.sh refuses a deploy
// over two lanes later, so refusing here reports the same host defect at the
// moment the operator is still looking at the screen.
func TestGrantCheckRefusesWhenNothingNamesAControlPlane(t *testing.T) {
	c := newFakeCluster(t)
	grantedHost(t, c, thorFake, fiveKeyRunnerSecrets())
	// A runner.env that exists (so this is not a first deploy) and names no
	// control plane.
	seedFile(t, runnerEnvPath(t, c, thorFake), "PR_UPKEEP_SWEEP_SOURCE_URL=https://example.invalid/sweep.py\n")

	stdout, stderr, code := runGrantCheck(t, c, thorFake, "")
	assertRefusedWithoutClaimingSuccess(t, stdout, stderr, code, "preflight failed on "+thorFake, "NODES_API_URL")
}

// TestGrantCheckRefusesWhenItCannotCreateItsWorkspace -- the answers land in
// files, so the directory they land in is part of the check. Without it curl
// has nowhere to write, and curl failing is the ONE documented decline: the
// lane announced "the control plane did not answer" and proceeded, over a
// control plane that had answered perfectly well.
func TestGrantCheckRefusesWhenItCannotCreateItsWorkspace(t *testing.T) {
	c := newFakeCluster(t)
	grantedHost(t, c, thorFake, fiveKeyRunnerSecrets())
	url := fakeControlPlane(t, grantCheckWorkflows(t), nil)

	absent := filepath.Join(t.TempDir(), "no-such-directory")
	stdout, stderr, code := runGrantCheck(t, c, thorFake, url, "TMPDIR="+absent)
	assertRefusedWithoutClaimingSuccess(t, stdout, stderr, code, "preflight failed on "+thorFake, "TMPDIR")
	if strings.Contains(stderr, "did not answer") {
		t.Errorf("a workspace this deploy could not create was reported as an unreachable control plane, "+
			"which is the one failure that is allowed to proceed; stderr was:\n%s", stderr)
	}
}

// TestGrantCheckRefusesWithNoPythonToReadTheAnswerWith is a deliberate change
// of behaviour, and the one place in this file where something that used to be
// a documented decline becomes a refusal.
//
// Without python3 nothing is compared. That is not "I could not reach the
// control plane" -- it is "I could not do the check", which is the state a
// safety gate exists to make loud. It shipped as a WARNING, and a WARNING in a
// deploy log is read by nobody at 03:00.
func TestGrantCheckRefusesWithNoPythonToReadTheAnswerWith(t *testing.T) {
	c := newFakeCluster(t)
	grantedHost(t, c, thorFake, fiveKeyRunnerSecrets())
	url := fakeControlPlane(t, grantCheckWorkflows(t), nil)

	path := pathWithoutPython3(t, c)
	stdout, stderr, code := runGrantCheck(t, c, thorFake, url, "PATH="+path)
	assertRefusedWithoutClaimingSuccess(t, stdout, stderr, code, "preflight failed on "+thorFake, "python3")
}

// pathWithoutPython3 builds a PATH holding the fake ssh plus symlinks to the
// exact tools this lane and the fake host need, and no python3.
//
// Shadowing python3 with a failing stub would not do: `command -v python3`
// asks PATH whether the name resolves, so only a PATH that genuinely lacks it
// reproduces an operator machine without python3. The returned PATH is
// verified to lack it before any test relies on it -- a PATH that still
// resolved python3 would make the test pass for the wrong reason.
func pathWithoutPython3(t *testing.T, c *fakeCluster) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{
		"bash", "env", "curl", "mktemp", "rm", "sed", "grep", "cat", "tail", "sort",
		"ls", "cp", "chmod", "date", "mkdir", "mv", "touch", "printf", "uname",
	} {
		src, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s is not on this machine's PATH, so a python3-less PATH cannot be built here: %v", tool, err)
		}
		if err := os.Symlink(src, filepath.Join(dir, tool)); err != nil {
			t.Fatalf("link %s: %v", tool, err)
		}
	}
	path := c.binDir + string(os.PathListSeparator) + dir
	probe := exec.Command("bash", "-c", "command -v python3")
	probe.Env = []string{"PATH=" + path}
	if out, err := probe.Output(); err == nil {
		t.Fatalf("the fixture PATH still resolves python3 to %q, so this test would pass without proving anything",
			strings.TrimSpace(string(out)))
	}
	return path
}
