// Command nodes-harvest stages a published actor handover on a candidate of
// the feature branch.
//
// Exit status is a routing contract, not a generic success/failure bit:
//
//	0 candidate_staged   package applied and eligible for verdicts
//	1 merge_conflict     package conflicts with the feature tip (domain edge)
//	2 staging_failure    the staging machinery could not produce an answer
//	3 routes_to_human    candidate touches .github/; no verdict may bypass it
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/agentculture/culture-nodes/internal/handover"
)

func main() {
	var req handover.Request
	var featureRef string
	flag.StringVar(&req.Repository, "repository", "", "integration Git repository")
	flag.StringVar(&req.Remote, "remote", "", "control-plane-configured Git remote name")
	flag.StringVar(&req.Ref, "ref", "", "published refs/culture-nodes/ ref")
	flag.StringVar(&req.Commit, "commit", "", "pinned 40-character commit object id")
	flag.StringVar(&req.RunID, "run-id", "", "Culture Nodes run id")
	flag.StringVar(&req.Worktree, "worktree", "", "new integration worktree path")
	flag.StringVar(&featureRef, "feature-ref", "", "feature branch or pinned feature commit")
	flag.Parse()

	result, err := handover.StageCandidate(context.Background(), req, featureRef)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harvest:", err)
		os.Exit(2)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "harvest: encode result:", err)
		os.Exit(2)
	}
	os.Exit(candidateExitCode(result.Outcome))
}

func candidateExitCode(outcome handover.CandidateOutcome) int {
	switch outcome {
	case handover.CandidateConflict:
		return 1
	case handover.CandidateRoutesHuman:
		return 3
	case handover.CandidateStaged:
		return 0
	default:
		return 2
	}
}
