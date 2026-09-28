package decl_test

import (
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
)

func TestHumanAskDeclaredOutcomes(t *testing.T) {
	base := `{"name":"ask","trigger":{"kind":"timer"},"action":{"kind":"human.ask","with":{"approver_ref":"group/reviewers","outcomes":%s}},"start_node":{"name":"root","deadline":"none"},"landing_node":{"name":"asked","deadline":"1h"}}`
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{`["retry","abandon","acknowledged"]`, true},
		{`[]`, false}, {`["retry","retry"]`, false}, {`["expired"]`, false},
		{`["bad name"]`, false}, {`["1bad"]`, false}, {`["retry",3]`, false},
	} {
		_, err := decl.Parse([]byte(strings.Replace(base, "%s", tc.value, 1)), decl.FormatJSON)
		if (err == nil) != tc.valid {
			t.Errorf("outcomes %s: err %v, valid=%v", tc.value, err, tc.valid)
		}
	}
}
