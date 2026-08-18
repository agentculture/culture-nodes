// Command nodes-merge consumes a gate-reports response on stdin, advances the
// gated feature branch to that exact --no-ff candidate, and pushes it through
// the #90 worker credential seam. Credential values are never command flags.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/agentculture/culture-nodes/internal/handover"
)

func main() {
	var req handover.MergeRequest
	flag.StringVar(&req.Repository, "repository", "", "integration Git repository")
	flag.StringVar(&req.Remote, "remote", "", "configured Git remote name")
	flag.StringVar(&req.Branch, "feature-branch", "", "feature branch to advance and push")
	flag.StringVar(&req.Candidate, "candidate", "", "gated two-parent candidate commit")
	flag.Parse()
	report, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	if err != nil {
		fmt.Fprintln(os.Stderr, "merge: read gate report:", err)
		os.Exit(2)
	}
	req.GateReport = report
	if err := handover.MergeCandidate(context.Background(), req); err != nil {
		fmt.Fprintln(os.Stderr, "merge:", err)
		os.Exit(2)
	}
	fmt.Println("feature branch pushed at gated candidate", req.Candidate)
}
