// Command nodes-harvest stages a published actor handover for integration.
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
	flag.StringVar(&req.Repository, "repository", "", "integration Git repository")
	flag.StringVar(&req.Remote, "remote", "", "control-plane-configured Git remote name")
	flag.StringVar(&req.Ref, "ref", "", "published refs/culture-nodes/ ref")
	flag.StringVar(&req.Commit, "commit", "", "pinned 40-character commit object id")
	flag.StringVar(&req.RunID, "run-id", "", "Culture Nodes run id")
	flag.StringVar(&req.Worktree, "worktree", "", "new integration worktree path")
	flag.Parse()

	result, err := handover.Harvest(context.Background(), req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harvest:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "harvest: encode result:", err)
		os.Exit(1)
	}
}
