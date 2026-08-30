// Package deploytest -- lanes/env-backup.sh returns a PATH, and the deploy log
// advertises a restore command built from it (PR #263 review finding 2).
//
// The reported finding: backup_env_file printed the new backup path with
// `printf %s` (no newline) and then ran the retention step into the same
// captured stdout, so anything the retention step printed would be
// concatenated onto the end of the path.
//
// Writing the test the finding asks for turned up a SECOND way to reach the
// same symptom, and this one fires with nothing printed at all. Retention
// ranked the archive with `ls -1t` -- by mtime -- but backups are made with
// `cp -p`, so a backup's mtime is the mtime of the file it copied, not the
// moment it was made. mtime is therefore not creation order, and on a host
// whose grant file was restored from an older copy the backup just taken sorts
// LAST and `tail -n +11` deletes it. The deploy then advertises a restore
// command for a file its own retention step removed.
//
// These tests do not assert the plumbing; they assert the OUTCOME an operator
// depends on at 03:00. The path the log names is stat-able, it has the shape a
// backup has and nothing appended to it, and pasting the advertised command
// back into a shell puts the replaced bytes back.
package deploytest

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// backupPathShape is the whole of what backup_env_file may return: the file's
// own path plus a UTC stamp. Asserting the shape is what catches a returned
// value that has had a log line concatenated onto it, which a bare
// strings.Contains on the base name would not.
var backupPathShape = regexp.MustCompile(`\.bak-\d{8}T\d{6}Z$`)

// backedUpLine and restoreLine are the two lines this lane prints. They are
// the lane's contract with the deploy log, so the tests read them the way an
// operator does -- by their prefix -- rather than by position.
const (
	backedUpPrefix = "==> backed up "
	restorePrefix  = "==> restore it with: "
)

// lineWithPrefix returns the single output line starting with prefix.
func lineWithPrefix(t *testing.T, output, prefix string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one line starting %q, got %d; output was:\n%s", prefix, len(found), output)
	}
	return found[0]
}

// seedFullBackupArchive fills path's archive past the retention ceiling, so
// the retention step of the next backup actually selects files to act on. That
// is the only state in which either defect in this file is reachable at all.
//
// Each backup's mtime is set to match the stamp in its NAME, and the file
// being backed up is given an OLDER one. That is not a contrived arrangement:
// backups are made with `cp -p`, so a backup's mtime is the mtime of the file
// it copied, not the moment it was made -- and any restore that preserves
// times (a `cp -p`, an `rsync -a`, a `tar x`, a host snapshot) moves the live
// file's mtime backwards. So this is the archive of a host whose runner
// secrets were restored from an older copy, and it is the state in which
// ranking backups by mtime ranks the newest one last.
func seedFullBackupArchive(t *testing.T, path string, count int) {
	t.Helper()
	for i := range count {
		stamp := time.Date(2026, 1, i+1, 0, 0, 0, 0, time.UTC)
		backup := fmt.Sprintf("%s.bak-%s", path, stamp.Format("20060102T150405Z"))
		seedFile(t, backup, "older bytes\n")
		if err := os.Chtimes(backup, stamp, stamp); err != nil {
			t.Fatalf("stamp %s: %v", backup, err)
		}
	}
	restored := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, restored, restored); err != nil {
		t.Fatalf("stamp %s: %v", path, err)
	}
}

// runBackupLane sources env-backup.sh and takes one backup of file on host,
// with the two streams kept apart -- the path this lane returns has to be
// separable from the lines it logs, and CombinedOutput would hide exactly the
// mixing under test.
func runBackupLane(t *testing.T, c *fakeCluster, host, file string) (string, string, int) {
	t.Helper()
	snippet := "set -euo pipefail\n. " + envBackupPath(t) + "\nbackup_env_file " + host + " " + file + "\n"
	return runSnippet(t, c, snippet)
}

// TestTheAdvertisedRestoreCommandRestoresTheFile is review finding 2 as the
// operator meets it. The backup is taken with the archive already full, the
// file is then destroyed the way the 2026-08-29 deploy destroyed it, and the
// command the deploy log printed is executed verbatim. If the returned path
// carries so much as one extra byte, the copy fails or restores nothing and
// the original bytes are gone for good.
func TestTheAdvertisedRestoreCommandRestoresTheFile(t *testing.T) {
	c := newFakeCluster(t)
	path := runnerSecretsPath(t, c, thorFake)
	before := fiveKeyRunnerSecrets()
	seedFile(t, path, before)
	seedFullBackupArchive(t, path, 14)

	stdout, stderr, code := runBackupLane(t, c, thorFake, "runner-secrets.env")
	if code != 0 {
		t.Fatalf("backup_env_file exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	restore := strings.TrimPrefix(lineWithPrefix(t, stdout, restorePrefix), restorePrefix)
	// The truncation the seatbelt is behind: install_jira_runner_env reduced
	// this file to two empty values and the runner refused 183 runs.
	seedFile(t, path, "JIRA_ACCOUNT_EMAIL=\nJIRA_API_TOKEN=\n")

	out, errOut, code := runSnippet(t, c, "set -euo pipefail\n"+restore+"\n")
	if code != 0 {
		t.Fatalf("the restore command the deploy log advertised exited %d: %s\nstdout:\n%s\nstderr:\n%s",
			code, restore, out, errOut)
	}
	if got := readFileString(t, path); got != before {
		t.Errorf("the advertised restore command did not put the prior bytes back.\ncommand: %s\ngot:\n%q\nwant:\n%q",
			restore, got, before)
	}
}

// TestTheBackupPathTheLaneReturnsIsTheOneItCreated is the same defect one
// level down, asserted directly: the path named in the log must exist on the
// host and must be a backup path and nothing else. A retention log line folded
// onto the end produces a name that stats as absent, which is precisely the
// symptom the review reported.
func TestTheBackupPathTheLaneReturnsIsTheOneItCreated(t *testing.T) {
	c := newFakeCluster(t)
	path := runnerSecretsPath(t, c, thorFake)
	seedFile(t, path, fiveKeyRunnerSecrets())
	seedFullBackupArchive(t, path, 14)

	stdout, stderr, code := runBackupLane(t, c, thorFake, "runner-secrets.env")
	if code != 0 {
		t.Fatalf("backup_env_file exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	named := strings.TrimPrefix(lineWithPrefix(t, stdout, backedUpPrefix), backedUpPrefix)
	parts := strings.Split(named, " to ")
	if len(parts) != 2 {
		t.Fatalf("the backup log line does not name a destination: %q", named)
	}
	backup := parts[1]
	if !backupPathShape.MatchString(backup) {
		t.Errorf("the lane returned %q, which is not a `<file>.bak-<UTC stamp>` path; something was "+
			"printed into the same stdout the path is captured from", backup)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Errorf("the deploy log names a backup that is not on the host: %v", err)
	}
	if backups := backupsOf(t, path); len(backups) > 10 {
		t.Errorf("%d backups remain (%v); separating the path from the log must not disable retention",
			len(backups), backups)
	}
}
