package engine_test

import (
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
)

func TestRemintTaxonomyExcludesEveryDomainOutcome(t *testing.T) {
	tests := []struct {
		name    string
		status  engine.TechStatus
		outcome string
		want    bool
	}{
		{"transport failure", engine.StatusFailed, "", true},
		{"timeout", engine.StatusTimedOut, "", true},
		{"contract refusal", engine.StatusContractRejected, "", true},
		{"completed", engine.StatusSucceeded, "completed", false},
		{"changes required", engine.StatusSucceeded, "changes_required", false},
		{"rejected", engine.StatusSucceeded, "rejected", false},
		{"needs human", engine.StatusSucceeded, "needs_human", false},
		{"inconsistent answer is still an answer", engine.StatusFailed, "changes_required", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := engine.RemintableTechnicalFailure(tc.status, tc.outcome); got != tc.want {
				t.Fatalf("RemintableTechnicalFailure(%q, %q) = %v, want %v", tc.status, tc.outcome, got, tc.want)
			}
		})
	}
}
