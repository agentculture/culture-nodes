package decl_test

import (
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
)

func TestAgentCannotRedefineBlocked(t *testing.T) {
	base := `{"name":"sample","trigger":{"kind":"timer"},"action":{"kind":"agent.work","with":{"uses":"actor://test/worker@sha256:aaaaaa","graph_config":{"contract":{"outcomes":%s}}}},"start_node":{"name":"root","deadline":"none"},"landing_node":{"name":"waiting","deadline":"1h"}}`
	for _, tc := range []struct {
		outcomes string
		refused  bool
	}{
		{`{"blocked":{"schema":{"type":"string"}}}`, true},
		{`{"completed":{"schema":{"type":"object"}}}`, false},
	} {
		_, err := decl.Parse([]byte(strings.Replace(base, "%s", tc.outcomes, 1)), decl.FormatJSON)
		if (err != nil) != tc.refused {
			t.Fatalf("outcomes %s: error %v, refused %v", tc.outcomes, err, tc.refused)
		}
		if tc.refused && !strings.Contains(err.Error(), "blocked is a reserved") {
			t.Fatal(err)
		}
	}
}
