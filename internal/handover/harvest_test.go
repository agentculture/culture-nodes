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
