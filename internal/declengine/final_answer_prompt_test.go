package declengine

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every agent dispatch tells the agent its domain outcomes and the JSON
// object its final answer must end with; a single-outcome node also names
// that outcome as input.success_outcome, so a prose-only answer maps to it.
// Found live on 2026-09-28 (#328): the developer stamped PR #329 with the
// GitHub App token and answered in prose, the bridge reported `completed`,
// and stamp-pr (outcome `stamped`) was contract_rejected.
func TestAgentDispatchNamesItsOutcomes(t *testing.T) {
	for _, tc := range []struct {
		lane, file     string
		outcomes       []string
		successOutcome string
		requires       string
	}{
		{"pr-upkeep", "stamp-pr", []string{"stamped"}, "stamped", "- stamped (output requires: summary)"},
		{"pr-upkeep", "retry-stamp-pr", []string{"stamped"}, "stamped", "- stamped (output requires: summary)"},
		{"pr-upkeep", "analyse", []string{"no_fix", "packaged"}, "", "- packaged"},
		{"pr-upkeep", "fix", []string{"completed", "no_change"}, "", "- no_change"},
		{"jira-intake", "intake", []string{"intake_drafted"}, "intake_drafted", "- intake_drafted"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			d := blockedFile(t, tc.lane, tc.file)
			_, raw, err := workerEnvelope(DispatchRequest{Action: d.Action, Declaration: d})
			if err != nil {
				t.Fatal(err)
			}
			var input map[string]any
			if err := json.Unmarshal(raw, &input); err != nil {
				t.Fatal(err)
			}
			instruction, _ := input["instruction"].(string)
			if !strings.Contains(instruction, `end your final answer with a JSON object {"outcome":"<outcome>","output":{...}}`) {
				t.Fatalf("instruction lacks the final-answer format:\n%s", instruction)
			}
			for _, o := range tc.outcomes {
				if !strings.Contains(instruction, "\n- "+o) {
					t.Errorf("instruction does not offer outcome %q", o)
				}
			}
			if !strings.Contains(instruction, tc.requires) {
				t.Errorf("instruction lacks %q", tc.requires)
			}
			// blocked stays last and still overrides the format.
			if strings.Index(instruction, "end your final answer") > strings.Index(instruction, "answer with outcome blocked") {
				t.Error("the blocked override must follow the success format")
			}
			if got, _ := input["success_outcome"].(string); got != tc.successOutcome {
				t.Errorf("success_outcome = %q, want %q", got, tc.successOutcome)
			}
		})
	}
}
