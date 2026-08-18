package handover

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// WorkerPushCredential is the #90 credential seam installed on the actor
// host in ~/.culture-nodes/bridge-push.env. Its value is environment-only: it
// is never accepted as a flag and never included in git argv.
const WorkerPushCredential = "GITHUB_TOKEN_WORKER"

var branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// GateAuthorization is the small part of POST gate-reports' response that is
// authoritative for merging. Data is decoded separately so the executor must
// compare both the aggregate outcome and its measured subject.
type GateAuthorization struct {
	Outcome   string `json:"outcome"`
	Aggregate struct {
		Data map[string]any `json:"data"`
	} `json:"aggregate"`
}

// MergeRequest pins every non-secret input to the merge. GateReport is read
// from stdin by cmd/nodes-merge, keeping even non-secret authorization data
// out of a long-lived process argv.
type MergeRequest struct {
	Repository string
	Remote     string
	Branch     string
	Candidate  string
	GateReport []byte
	GitBinary  string // tests only; empty selects git from PATH
}

// MergeCandidate advances and pushes a feature branch only when the supplied
// aggregate is gates_passed for the exact two-parent --no-ff candidate. The
// expected-old update-ref closes the gap between inspecting the feature tip
// and advancing it; a concurrent feature update is refused, never overwritten.
func MergeCandidate(ctx context.Context, req MergeRequest) error {
	if err := validateMergeRequest(req); err != nil {
		return err
	}
	var gate GateAuthorization
	if err := json.Unmarshal(req.GateReport, &gate); err != nil {
		return fmt.Errorf("decode gate report: %w", err)
	}
	if gate.Outcome != OutcomeGatesPassed {
		return fmt.Errorf("gate outcome is %q, want %q", gate.Outcome, OutcomeGatesPassed)
	}
	measured, _ := gate.Aggregate.Data["commit_sha"].(string)
	if measured != req.Candidate {
		return fmt.Errorf("gate measured candidate %q, about-to-merge candidate is %q", measured, req.Candidate)
	}
	credential := os.Getenv(WorkerPushCredential)
	if credential == "" {
		return fmt.Errorf("%s is unset; refusing a push that would prompt for operator credentials", WorkerPushCredential)
	}

	git := req.GitBinary
	if git == "" {
		git = "git"
	}
	branchRef := "refs/heads/" + req.Branch
	candidate, err := mergeOutput(ctx, git, req.Repository, nil, "rev-parse", "--verify", req.Candidate+"^{commit}")
	if err != nil {
		return err
	}
	if candidate != req.Candidate {
		return fmt.Errorf("candidate resolved to %s, want %s", candidate, req.Candidate)
	}
	parents, err := mergeOutput(ctx, git, req.Repository, nil, "show", "-s", "--format=%P", req.Candidate)
	if err != nil {
		return err
	}
	parent := strings.Fields(parents)
	if len(parent) != 2 {
		return fmt.Errorf("candidate %s has %d parent(s), want the two-parent commit produced by merge --no-ff", req.Candidate, len(parent))
	}
	featureTip, err := mergeOutput(ctx, git, req.Repository, nil, "rev-parse", "--verify", branchRef+"^{commit}")
	if err != nil {
		return err
	}
	if featureTip != parent[0] {
		return fmt.Errorf("feature branch moved: %s is %s, candidate was built on %s", branchRef, featureTip, parent[0])
	}
	if err := mergeRun(ctx, git, req.Repository, nil, "update-ref", branchRef, req.Candidate, featureTip); err != nil {
		return fmt.Errorf("advance feature branch: %w", err)
	}

	askpassDir, err := os.MkdirTemp("", "culture-nodes-askpass-")
	if err != nil {
		return fmt.Errorf("prepare push credential helper: %w", err)
	}
	defer os.RemoveAll(askpassDir)
	askpass := filepath.Join(askpassDir, "askpass")
	const helper = "#!/bin/sh\ncase \"$1\" in *Username*) printf '%s\\n' x-access-token;; *) printf '%s\\n' \"$GITHUB_TOKEN_WORKER\";; esac\n"
	if err := os.WriteFile(askpass, []byte(helper), 0o700); err != nil {
		return fmt.Errorf("write push credential helper: %w", err)
	}
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_ASKPASS="+askpass)
	// A configured credential helper SILENTLY outranks GIT_ASKPASS on push —
	// on any host where `gh auth setup-git` ran, the push would use the
	// helper's identity instead of WorkerPushCredential and nothing would
	// say so (measured on this fleet; see the operator memory that #90's
	// correction comment records). The two empty -c flags reset the helper
	// list for this one invocation so the askpass identity is the ONLY one.
	if err := mergeRun(ctx, git, req.Repository, env,
		"-c", "credential.helper=",
		"-c", "credential.https://github.com.helper=",
		"push", "--porcelain", "--", req.Remote, branchRef+":"+branchRef); err != nil {
		return fmt.Errorf("push feature branch: %w", err)
	}
	return nil
}

func validateMergeRequest(req MergeRequest) error {
	if req.Repository == "" || len(req.GateReport) == 0 {
		return errors.New("repository and gate report are required")
	}
	if !namePattern.MatchString(req.Remote) {
		return errors.New("remote must be a configured remote name")
	}
	if !branchPattern.MatchString(req.Branch) || strings.Contains(req.Branch, "..") || strings.HasSuffix(req.Branch, "/") {
		return errors.New("branch is not a safe feature branch name")
	}
	if !commitPattern.MatchString(req.Candidate) {
		return errors.New("candidate must be a lowercase 40-character object id")
	}
	return nil
}

func mergeOutput(ctx context.Context, git, repo string, env []string, args ...string) (string, error) {
	var stdout bytes.Buffer
	if err := mergeCommand(ctx, git, repo, env, &stdout, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

func mergeRun(ctx context.Context, git, repo string, env []string, args ...string) error {
	return mergeCommand(ctx, git, repo, env, nil, args...)
}

func mergeCommand(ctx context.Context, git, repo string, env []string, stdout *bytes.Buffer, args ...string) error {
	cmd := exec.CommandContext(ctx, git, append([]string{"-C", repo}, args...)...) // #nosec G204 -- binary is test-injectable; production uses fixed git
	if env == nil {
		env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	}
	cmd.Env = env
	if stdout != nil {
		cmd.Stdout = stdout
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if secret := os.Getenv(WorkerPushCredential); secret != "" {
			detail = strings.ReplaceAll(detail, secret, "<redacted>")
		}
		return fmt.Errorf("git %s: %w: %s", args[0], err, detail)
	}
	return nil
}
