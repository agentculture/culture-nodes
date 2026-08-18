package handover_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentculture/culture-nodes/internal/handover"
)

func fixture(t *testing.T) (repo, remote, ref, commit string) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "integration")
	remote = filepath.Join(root, "fixture.git")
	producer := filepath.Join(root, "producer")
	git(t, root, "init", "--bare", remote)
	git(t, root, "init", repo)
	git(t, root, "init", producer)
	if err := os.WriteFile(filepath.Join(producer, "harvested.txt"), []byte("from actor\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, producer, "add", "harvested.txt")
	git(t, producer, "commit", "-m", "fixture handover")
	commit = git(t, producer, "rev-parse", "HEAD")
	ref = "refs/culture-nodes/run-fixture"
	git(t, producer, "push", remote, "HEAD:"+ref)
	git(t, repo, "remote", "add", "handover", remote)
	return repo, remote, ref, commit
}

func TestHarvestFetchesFixtureRefAndStagesWorktree(t *testing.T) {
	repo, _, ref, commit := fixture(t)
	worktree := filepath.Join(t.TempDir(), "staged")

	result, err := handover.Harvest(context.Background(), handover.Request{
		Repository: repo, Remote: "handover", Ref: ref, Commit: commit,
		RunID: "run-fixture", Worktree: worktree, AllowFileFixture: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Commit != commit || result.RecoveryRef != "refs/culture-nodes/harvested/run-fixture" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if got := git(t, worktree, "rev-parse", "HEAD"); got != commit {
		t.Fatalf("worktree HEAD = %s, want %s", got, commit)
	}
	if got, err := os.ReadFile(filepath.Join(worktree, "harvested.txt")); err != nil || string(got) != "from actor\n" {
		t.Fatalf("staged content = %q, %v", got, err)
	}
}

func TestFetchIsRecoverableBeforeWorktreeStage(t *testing.T) {
	repo, _, ref, commit := fixture(t)
	badWorktree := filepath.Join(t.TempDir(), "non-empty")
	if err := os.Mkdir(badWorktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badWorktree, "occupied"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := handover.Harvest(context.Background(), handover.Request{
		Repository: repo, Remote: "handover", Ref: ref, Commit: commit,
		RunID: "run-preserved", Worktree: badWorktree, AllowFileFixture: true,
	})
	if err == nil {
		t.Fatal("Harvest succeeded, want worktree staging failure")
	}
	if got := git(t, repo, "rev-parse", "refs/culture-nodes/harvested/run-preserved"); got != commit {
		t.Fatalf("recovery ref = %s, want %s (harvest error: %v)", got, commit, err)
	}
}

func TestHarvestRefusesUnsafeInputs(t *testing.T) {
	repo, remote, ref, commit := fixture(t)
	cases := []struct {
		name   string
		mutate func(*handover.Request)
	}{
		{"ref outside namespace", func(r *handover.Request) { r.Ref = "refs/heads/main" }},
		{"run id with slash", func(r *handover.Request) { r.RunID = "run/escape" }},
		{"filesystem transport in production", func(r *handover.Request) { r.AllowFileFixture = false }},
		{"remote url as input", func(r *handover.Request) { r.Remote = remote }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := handover.Request{Repository: repo, Remote: "handover", Ref: ref, Commit: commit,
				RunID: "safe-run", Worktree: filepath.Join(t.TempDir(), "worktree"), AllowFileFixture: true}
			tc.mutate(&r)
			if _, err := handover.Harvest(context.Background(), r); err == nil {
				t.Fatal("Harvest succeeded, want refusal")
			}
		})
	}
}

func candidateFixture(t *testing.T, featureBody, packagePath, packageBody string) (repo, ref, commit string) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "integration")
	producer := filepath.Join(root, "producer")
	remote := filepath.Join(root, "fixture.git")
	git(t, root, "init", "--bare", remote)
	git(t, root, "init", "--initial-branch=feature", repo)
	git(t, repo, "config", "user.email", "candidate@example.test")
	git(t, repo, "config", "user.name", "candidate fixture")
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "shared.txt")
	git(t, repo, "commit", "-m", "base")
	git(t, root, "clone", repo, producer)
	git(t, producer, "config", "user.email", "package@example.test")
	git(t, producer, "config", "user.name", "package fixture")

	if featureBody != "" {
		if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte(featureBody), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", "shared.txt")
		git(t, repo, "commit", "-m", "feature advances")
	}
	path := filepath.Join(producer, filepath.FromSlash(packagePath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(packageBody), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, producer, "add", "-A")
	git(t, producer, "commit", "-m", "fixture package")
	commit = git(t, producer, "rev-parse", "HEAD")
	ref = "refs/culture-nodes/candidate-fixture"
	git(t, producer, "push", remote, "HEAD:"+ref)
	git(t, repo, "remote", "add", "handover", remote)
	return repo, ref, commit
}

func TestStageCandidateReturnsConflictDomainOutcome(t *testing.T) {
	repo, ref, commit := candidateFixture(t, "feature\n", "shared.txt", "package\n")
	result, err := handover.StageCandidate(context.Background(), handover.Request{
		Repository: repo, Remote: "handover", Ref: ref, Commit: commit,
		RunID: "conflict-fixture", Worktree: filepath.Join(t.TempDir(), "candidate"), AllowFileFixture: true,
	}, "feature")
	if err != nil {
		t.Fatalf("StageCandidate returned engine error for a merge conflict: %v", err)
	}
	if result.Outcome != handover.CandidateConflict {
		t.Fatalf("outcome = %q, want %q", result.Outcome, handover.CandidateConflict)
	}
	if len(result.ChangedPaths) != 1 || result.ChangedPaths[0] != "shared.txt" {
		t.Fatalf("conflicting paths = %q, want shared.txt", result.ChangedPaths)
	}
}

func TestStageCandidateRoutesGitHubChangeToHumanBeforeVerdict(t *testing.T) {
	repo, ref, commit := candidateFixture(t, "", ".github/workflows/package.yml", "name: package\n")
	result, err := handover.StageCandidate(context.Background(), handover.Request{
		Repository: repo, Remote: "handover", Ref: ref, Commit: commit,
		RunID: "github-fixture", Worktree: filepath.Join(t.TempDir(), "candidate"), AllowFileFixture: true,
	}, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != handover.CandidateRoutesHuman {
		t.Fatalf("outcome = %q, want %q", result.Outcome, handover.CandidateRoutesHuman)
	}
	if len(result.GuardedPaths) != 1 || result.GuardedPaths[0] != ".github/workflows/package.yml" {
		t.Fatalf("guarded paths = %q", result.GuardedPaths)
	}
	// StageCandidate has no verdict input: routing is computed from the
	// candidate diff before a green suite result can enter the lane.
}
