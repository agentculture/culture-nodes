package main

import (
	"testing"

	"github.com/agentculture/culture-nodes/internal/handover"
)

func TestCandidateExitCodesAreDistinctRoutingEdges(t *testing.T) {
	cases := []struct {
		outcome handover.CandidateOutcome
		want    int
	}{
		{handover.CandidateStaged, 0},
		{handover.CandidateConflict, 1},
		{handover.CandidateRoutesHuman, 3},
		{handover.CandidateOutcome("unknown"), 2},
	}
	for _, tc := range cases {
		if got := candidateExitCode(tc.outcome); got != tc.want {
			t.Errorf("candidateExitCode(%q) = %d, want %d", tc.outcome, got, tc.want)
		}
	}
}
