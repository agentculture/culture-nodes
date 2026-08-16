package headspace_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// TestExecute_RunIdentityCrossesTheBoundary is task t16's enabling fact: a
// code node can name the run it is executing for.
//
// Without it a merge gate expressed as a code node can measure everything and
// record nothing — every derived record it writes has to name its subject, and
// the operation was the only thing that knew what the subject was. The names
// are granted the same way a secret is (`--env NAME`, value in envp, never in
// argv), so nothing here weakens the argv discipline the bridge already holds.
func TestExecute_RunIdentityCrossesTheBoundary(t *testing.T) {
	record := recordFile(t)
	t.Setenv("NODES_FAKE_RUN_EXIT", "0")

	b := newTestBridge(t, nil)
	op := baseOperation(t, b, []string{"python3", "-c", "import os; print(os.environ['NODES_RUN_ID'])"})
	op.Context = &runners.Context{
		RunID:     "01M05ZGNT86MAFDHATB6W5VYPN",
		NodeRunID: "01M05ZGNT8QW2W1M5PAPXQ8N3C",
		AttemptID: "01M05ZGNT8SJ2K9GS5Y7HQ0F1B",
	}

	if _, err := b.Execute(context.Background(), op); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	raw, err := os.ReadFile(record) //nolint:gosec // test-controlled path.
	if err != nil {
		t.Fatalf("read record file: %v", err)
	}
	for _, name := range []string{runners.EnvRunID, runners.EnvNodeRunID, runners.EnvAttemptID} {
		if !strings.Contains(string(raw), name) {
			t.Errorf("expected --env %s in recorded argv:\n%s", name, raw)
		}
	}
	// The ids are identities, not secrets, but they still travel as env
	// values: argv is the digest-addressed half of an operation and must not
	// vary run to run.
	if strings.Contains(string(raw), "01M05ZGNT86MAFDHATB6W5VYPN") {
		t.Errorf("the run id reached argv; it must cross only as an environment value:\n%s", raw)
	}
}
