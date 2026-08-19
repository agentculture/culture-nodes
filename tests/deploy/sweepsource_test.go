package deploytest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSweepSourceDefaultsFollowShippedRevisionAndRemainOverridable(t *testing.T) {
	script := deployScriptText(t)
	for _, want := range []string{
		`REVISION=$(git rev-parse "$BRANCH")`,
		`PR_UPKEEP_SWEEP_SOURCE_URL=${PR_UPKEEP_SWEEP_SOURCE_URL:-`,
		`PR_UPKEEP_SWEEP_SOURCE_SHA256=${PR_UPKEEP_SWEEP_SOURCE_SHA256:-`,
		`git show "$REVISION:examples/pr-upkeep/sweep.py"`,
		`PR_UPKEEP_SWEEP_JIRA_SOURCE_URL=${PR_UPKEEP_SWEEP_JIRA_SOURCE_URL:-`,
		`PR_UPKEEP_SWEEP_JIRA_SOURCE_SHA256=${PR_UPKEEP_SWEEP_JIRA_SOURCE_SHA256:-`,
		`git show "$REVISION:examples/pr-upkeep/pr_upkeep_jira.py"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("deploy.sh does not derive the sweep grant from its shipped revision; missing %q", want)
		}
	}
	if strings.Contains(script, "0abf042") {
		t.Error("deploy.sh still pins the orphaned pre-squash revision")
	}
}

func TestSweepSourceDerivationWorksAfterSquashMerge(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "test")
	run("config", "user.email", "test@example.invalid")
	path := filepath.Join(repo, "examples", "pr-upkeep", "sweep.py")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("print('base')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "feature")
	if err := os.WriteFile(path, []byte("print('squashed')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-qam", "feature")
	orphaned := run("rev-parse", "HEAD")
	run("checkout", "-q", "main")
	if err := os.WriteFile(path, []byte("print('squashed')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-qam", "squash merge feature")
	revision := run("rev-parse", "HEAD")
	run("branch", "-D", "feature")
	if refs := run("for-each-ref", "--contains="+orphaned, "--format=%(refname)", "refs/heads"); refs != "" {
		t.Fatalf("pre-squash commit remains branch-reachable through %s", refs)
	}
	content := run("show", revision+":examples/pr-upkeep/sweep.py")
	if content != "print('squashed')" {
		t.Fatalf("sweep at shipped squash revision = %q", content)
	}
	url := "https://raw.githubusercontent.com/agentculture/culture-nodes/" + revision + "/examples/pr-upkeep/sweep.py"
	if !strings.Contains(url, revision) || strings.Contains(url, orphaned) {
		t.Fatalf("derived URL %q does not name the shipped squash revision", url)
	}
}
