package notifier

import "testing"

func TestIsLifecycleEventAdmitsExactlyTheFiveRunStates(t *testing.T) {
	want := []string{
		"dev.culture.nodes.run.created",
		"dev.culture.nodes.run.completed",
		"dev.culture.nodes.run.failed",
		"dev.culture.nodes.run.cancelled",
		"dev.culture.nodes.run.bounded",
	}
	for _, eventType := range want {
		if !isLifecycleEvent(eventType) {
			t.Errorf("isLifecycleEvent(%q) = false, want true", eventType)
		}
	}
}

func TestIsLifecycleEventRejectsEverythingElse(t *testing.T) {
	reject := []string{
		"dev.culture.nodes.attempt.completed",
		"dev.culture.nodes.node-run.ready",
		"dev.culture.nodes.ledger.record-appended",
		"dev.culture.nodes.token.transitioned",
		"",
		"run.completed", // missing the required prefix
	}
	for _, eventType := range reject {
		if isLifecycleEvent(eventType) {
			t.Errorf("isLifecycleEvent(%q) = true, want false", eventType)
		}
	}
}

func TestWorkflowSkippedMatchesExactAndStarPrefix(t *testing.T) {
	cases := []struct {
		key  string
		skip []string
		want bool
	}{
		{"pr-upkeep-sweep-cycle", []string{"pr-upkeep-sweep-cycle"}, true},
		{"pr-upkeep-sweep-cycle", []string{"pr-upkeep-sweep"}, false}, // no `*`: exact only
		{"pr-upkeep-sweep", []string{"pr-upkeep-sweep-cycle"}, false},
		{"pr-upkeep-sweep", []string{"pr-upkeep-sweep*"}, true},
		{"pr-upkeep-sweep-cycle", []string{"pr-upkeep-sweep*"}, true},
		{"pr-upkeep-swept", []string{"pr-upkeep-sweep*"}, false},
		{"parallel-live-proof", []string{"other", "parallel-*"}, true},
		{"parallel-live-proof", nil, false},            // empty list: today's behaviour
		{"", []string{"*"}, false},                     // an unresolved name is never skipped (fail-open)
		{"pr-upkeep-sweep-cycle", []string{""}, false}, // a blank entry matches nothing
	}
	for _, c := range cases {
		if got := workflowSkipped(c.key, c.skip); got != c.want {
			t.Errorf("workflowSkipped(%q, %q) = %v, want %v", c.key, c.skip, got, c.want)
		}
	}
}
