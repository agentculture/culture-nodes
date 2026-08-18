package handover_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/handover"
)

const mergeSecret = "worker-secret-must-never-appear"

func TestMergeCandidatePushesExactGatedNoFFCommit(t *testing.T) {
	repo, remote, base, candidate := mergeFixture(t)
	t.Setenv(handover.WorkerPushCredential, mergeSecret)
	req := handover.MergeRequest{
		Repository: repo,
		Remote:     "origin",
		Branch:     "feature/loop",
		Candidate:  candidate,
		GateReport: gateReport(t, "gates_passed", candidate),
	}
	if err := handover.MergeCandidate(context.Background(), req); err != nil {
		t.Fatalf("merge candidate: %v", err)
	}
	if got := gitMerge(t, repo, "rev-parse", "refs/heads/feature/loop"); got != candidate {
		t.Fatalf("local feature = %s, want gated candidate %s", got, candidate)
	}
	if got := gitMerge(t, remote, "rev-parse", "refs/heads/feature/loop"); got != candidate {
		t.Fatalf("pushed feature = %s, want gated candidate %s", got, candidate)
	}
	if parents := strings.Fields(gitMerge(t, repo, "show", "-s", "--format=%P", candidate)); len(parents) != 2 || parents[0] != base {
		t.Fatalf("candidate parents = %v, want two-parent --no-ff merge based on %s", parents, base)
	}
}

func TestMergeCandidateRefusesVerdictOrCandidateMismatchBeforeBranchMoves(t *testing.T) {
	repo, _, base, candidate := mergeFixture(t)
	t.Setenv(handover.WorkerPushCredential, mergeSecret)
	for _, tc := range []struct {
		name, outcome, measured string
	}{
		{"not passed", "changes_required", candidate},
		{"different candidate", "gates_passed", strings.Repeat("a", 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := handover.MergeCandidate(context.Background(), handover.MergeRequest{
				Repository: repo, Remote: "origin", Branch: "feature/loop", Candidate: candidate,
				GateReport: gateReport(t, tc.outcome, tc.measured),
			})
			if err == nil {
				t.Fatal("merge succeeded without an exact gates_passed authorization")
			}
			if got := gitMerge(t, repo, "rev-parse", "refs/heads/feature/loop"); got != base {
				t.Fatalf("feature moved to %s after refusal, want %s", got, base)
			}
		})
	}
}

func TestPushCredentialReachesNeitherArgvNorDiagnostic(t *testing.T) {
	repo, _, _, candidate := mergeFixture(t)
	t.Setenv(handover.WorkerPushCredential, mergeSecret)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv")
	wrapper := filepath.Join(dir, "git-wrapper")
	script := `#!/bin/sh
case " $* " in *" push "*) is_push=1;; *) is_push=0;; esac
if [ "$is_push" = 1 ]; then
  printf '%s\n' "$@" > "$MERGE_ARGV_LOG"
  printf 'hostile remote echoed %s\n' "$GITHUB_TOKEN_WORKER" >&2
  exit 1
fi
exec "$REAL_GIT" "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("MERGE_ARGV_LOG", argvLog)
	err = handover.MergeCandidate(context.Background(), handover.MergeRequest{
		Repository: repo, Remote: "origin", Branch: "feature/loop", Candidate: candidate,
		GateReport: gateReport(t, "gates_passed", candidate), GitBinary: wrapper,
	})
	if err == nil {
		t.Fatal("fake push succeeded")
	}
	if strings.Contains(err.Error(), mergeSecret) {
		t.Fatalf("credential reached diagnostic: %v", err)
	}
	argv, readErr := os.ReadFile(argvLog)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(argv), mergeSecret) {
		t.Fatalf("credential reached argv: %s", argv)
	}
}

func mergeFixture(t *testing.T) (repo, remote, base, candidate string) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	remote = filepath.Join(root, "remote.git")
	runMerge(t, root, "git", "init", "-q", "--bare", remote)
	runMerge(t, root, "git", "init", "-q", repo)
	gitMerge(t, repo, "config", "user.name", "Test")
	gitMerge(t, repo, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "base"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitMerge(t, repo, "add", "base")
	gitMerge(t, repo, "commit", "-q", "-m", "base")
	base = gitMerge(t, repo, "rev-parse", "HEAD")
	gitMerge(t, repo, "branch", "feature/loop", base)
	gitMerge(t, repo, "checkout", "-q", "-b", "package")
	if err := os.WriteFile(filepath.Join(repo, "package"), []byte("package\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitMerge(t, repo, "add", "package")
	gitMerge(t, repo, "commit", "-q", "-m", "package")
	gitMerge(t, repo, "checkout", "-q", "--detach", base)
	gitMerge(t, repo, "merge", "-q", "--no-ff", "package", "-m", "candidate")
	candidate = gitMerge(t, repo, "rev-parse", "HEAD")
	gitMerge(t, repo, "remote", "add", "origin", remote)
	return repo, remote, base, candidate
}

func gateReport(t *testing.T, outcome, candidate string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"outcome":   outcome,
		"aggregate": map[string]any{"data": map[string]any{"commit_sha": candidate}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func gitMerge(t *testing.T, repo string, args ...string) string {
	t.Helper()
	all := append([]string{"-C", repo}, args...)
	cmd := exec.Command("git", all...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func runMerge(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", name, err, out)
	}
}
