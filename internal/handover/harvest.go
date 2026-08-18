// Package handover moves portable git-ref handoffs between node workspaces.
package handover

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// Request describes a harvest from a control-plane-configured Git remote.
// Remote is a configured remote name, deliberately not a URL or credential.
type Request struct {
	Repository       string
	Remote           string
	Ref              string
	Commit           string
	RunID            string
	Worktree         string
	AllowFileFixture bool
}

// Result names both the staged tree and its durable recovery handle.
type Result struct {
	Commit      string `json:"commit"`
	RecoveryRef string `json:"recovery_ref"`
	Worktree    string `json:"worktree"`
}

// CandidateOutcome is the closed set of domain answers candidate staging can
// produce. Errors are reserved for failures to run the staging machinery;
// merge conflicts and protected CI changes are expected routing outcomes.
type CandidateOutcome string

const (
	CandidateStaged      CandidateOutcome = "candidate_staged"
	CandidateConflict    CandidateOutcome = "merge_conflict"
	CandidateRoutesHuman CandidateOutcome = "routes_to_human"
)

// CandidateResult describes the feature-based worktree after the harvested
// package has been applied to it. ChangedPaths is measured against the pinned
// feature tip, not taken from the package's report.
type CandidateResult struct {
	Result
	FeatureCommit   string           `json:"feature_commit"`
	CandidateCommit string           `json:"candidate_commit,omitempty"`
	Outcome         CandidateOutcome `json:"outcome"`
	ChangedPaths    []string         `json:"changed_paths,omitempty"`
	GuardedPaths    []string         `json:"guarded_paths,omitempty"`
}

// Harvest fetches the named handover into a durable, run-scoped ref before it
// creates the integration worktree. Git updates the ref atomically only after
// receiving its objects, so process termination can never leave a ref that
// names an incomplete object graph. Once that ref exists, the run ID alone
// identifies the recoverable commit even if worktree staging later fails.
func Harvest(ctx context.Context, req Request) (Result, error) {
	if err := validate(req); err != nil {
		return Result{}, err
	}
	remoteURL, err := output(ctx, req.Repository, "remote", "get-url", req.Remote)
	if err != nil {
		return Result{}, fmt.Errorf("resolve configured remote %q: %w", req.Remote, err)
	}
	if err := validateTransport(remoteURL, req.AllowFileFixture); err != nil {
		return Result{}, err
	}

	recovery := "refs/culture-nodes/harvested/" + req.RunID
	refspec := "+" + req.Ref + ":" + recovery
	if err := run(ctx, req.Repository, "fetch", "--no-tags", "--", req.Remote, refspec); err != nil {
		return Result{}, fmt.Errorf("fetch handover %s: %w", req.Ref, err)
	}
	got, err := output(ctx, req.Repository, "rev-parse", "--verify", recovery+"^{commit}")
	if err != nil {
		return Result{}, fmt.Errorf("resolve recovery ref %s: %w", recovery, err)
	}
	if got != req.Commit {
		return Result{}, fmt.Errorf("fetched commit %s does not match pinned commit %s; preserved at %s", got, req.Commit, recovery)
	}

	if err := run(ctx, req.Repository, "worktree", "add", "--detach", req.Worktree, recovery); err != nil {
		return Result{}, fmt.Errorf("stage worktree (fetched commit preserved at %s): %w", recovery, err)
	}
	return Result{Commit: got, RecoveryRef: recovery, Worktree: req.Worktree}, nil
}

// StageCandidate harvests the package, then applies it on a detached worktree
// pinned to featureRef. An eligible combination is materialized as a detached
// merge commit so the suite verdict and later branch update name the same tree.
// This phase precedes suite verdicts:
// a green verdict therefore cannot turn a protected .github change into an
// ordinary merge candidate.
func StageCandidate(ctx context.Context, req Request, featureRef string) (CandidateResult, error) {
	if strings.TrimSpace(featureRef) == "" || strings.HasPrefix(featureRef, "-") {
		return CandidateResult{}, errors.New("feature ref is required and must not begin with '-'")
	}
	harvested, err := Harvest(ctx, req)
	if err != nil {
		return CandidateResult{}, err
	}
	featureCommit, err := output(ctx, req.Repository, "rev-parse", "--verify", featureRef+"^{commit}")
	if err != nil {
		return CandidateResult{}, fmt.Errorf("resolve feature ref %q: %w", featureRef, err)
	}
	result := CandidateResult{Result: harvested, FeatureCommit: featureCommit}
	if err := run(ctx, req.Worktree, "reset", "--hard", featureCommit); err != nil {
		return CandidateResult{}, fmt.Errorf("pin candidate to feature commit %s: %w", featureCommit, err)
	}

	mergeErr := run(ctx, req.Worktree, "merge", "--no-commit", "--no-ff", harvested.Commit)
	if mergeErr != nil {
		unmerged, readErr := output(ctx, req.Worktree, "diff", "--name-only", "--diff-filter=U", "-z")
		if readErr != nil {
			return CandidateResult{}, fmt.Errorf("merge package and inspect conflict: %v; %w", mergeErr, readErr)
		}
		if paths := splitZeroPaths(unmerged); len(paths) > 0 {
			result.Outcome = CandidateConflict
			result.ChangedPaths = paths
			return result, nil
		}
		return CandidateResult{}, fmt.Errorf("merge harvested package: %w", mergeErr)
	}

	changed, err := output(ctx, req.Worktree, "diff", "--name-only", "-z", featureCommit)
	if err != nil {
		return CandidateResult{}, fmt.Errorf("measure candidate diff: %w", err)
	}
	result.ChangedPaths = splitZeroPaths(changed)
	for _, path := range result.ChangedPaths {
		if path == ".github" || strings.HasPrefix(path, ".github/") {
			result.GuardedPaths = append(result.GuardedPaths, path)
		}
	}
	if len(result.GuardedPaths) > 0 {
		result.Outcome = CandidateRoutesHuman
	} else {
		// Materialize the exact tree the gate will measure.  This is a detached
		// merge commit: it moves no branch, but gives the combination a durable
		// object id which the gate report and the later merge step can both name.
		if err := run(ctx, req.Worktree,
			"-c", "user.name=Culture Nodes Candidate",
			"-c", "user.email=candidate@culture-nodes.invalid",
			"commit", "--no-edit"); err != nil {
			return CandidateResult{}, fmt.Errorf("materialize candidate commit: %w", err)
		}
		result.CandidateCommit, err = output(ctx, req.Worktree, "rev-parse", "HEAD")
		if err != nil {
			return CandidateResult{}, fmt.Errorf("resolve candidate commit: %w", err)
		}
		result.Outcome = CandidateStaged
	}
	return result, nil
}

func splitZeroPaths(raw string) []string {
	var paths []string
	for _, path := range strings.Split(raw, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func validate(req Request) error {
	if req.Repository == "" || req.Worktree == "" {
		return errors.New("repository and worktree are required")
	}
	if !namePattern.MatchString(req.Remote) {
		return errors.New("remote must be a configured remote name")
	}
	if !strings.HasPrefix(req.Ref, "refs/culture-nodes/") || strings.ContainsAny(req.Ref, " ~^:?*[\\") {
		return errors.New("ref must be a valid refs/culture-nodes/ ref")
	}
	if !commitPattern.MatchString(req.Commit) {
		return errors.New("commit must be a lowercase 40-character object id")
	}
	if !namePattern.MatchString(req.RunID) {
		return errors.New("run id contains unsafe characters")
	}
	repo, err := filepath.Abs(req.Repository)
	if err != nil {
		return fmt.Errorf("resolve repository: %w", err)
	}
	worktree, err := filepath.Abs(req.Worktree)
	if err != nil {
		return fmt.Errorf("resolve worktree: %w", err)
	}
	if worktree == repo {
		return errors.New("worktree must differ from repository")
	}
	return nil
}

func validateTransport(raw string, allowFileFixture bool) error {
	if allowFileFixture {
		if filepath.IsAbs(raw) || strings.HasPrefix(raw, "file://") {
			return nil
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("configured remote must use HTTPS")
	}
	if u.User != nil {
		return errors.New("configured remote URL must not contain user information")
	}
	return nil
}

func run(ctx context.Context, repo string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func output(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
