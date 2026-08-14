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
