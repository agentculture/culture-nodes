package kinds

import (
	"errors"
	"testing"
)

// Task t38g (review A2): every trigger kind the control plane emits is
// reserved -- the three reactions, node.expired, and every action.* result
// registered now or later -- and the outside facts are not.
func TestReservedEventsAreTheControlPlanesOwn(t *testing.T) {
	for _, k := range Triggers() {
		engineOwn := k.Name == "timer" || k.Name == "human.decision" || k.Name == "code.result" || k.Name == "agent.result"
		for _, c := range k.Consumes {
			if c == ArtifactActionResult || c == ArtifactNode {
				engineOwn = true
			}
		}
		if got := ReservedEvent(k.Name); got != engineOwn {
			t.Errorf("ReservedEvent(%q) = %v, want %v", k.Name, got, engineOwn)
		}
	}
	if !ReservedEvent("action.some_future_result") {
		t.Error("an unregistered action.* name is not reserved")
	}
}

func TestCheckExternalEvent(t *testing.T) {
	var refusal *ReservedEventError
	if err := CheckExternalEvent("human.decision", "bridge"); !errors.As(err, &refusal) || refusal.Name != "human.decision" {
		t.Fatalf("reserved name: err = %v", err)
	}
	if err := CheckExternalEvent("ordinary", ControlPlaneEmitter); !errors.As(err, &refusal) || refusal.Emitter != ControlPlaneEmitter {
		t.Fatalf("reserved emitter: err = %v", err)
	}
	if err := CheckExternalEvent("ordinary", "external"); err != nil {
		t.Fatalf("ordinary event refused: %v", err)
	}
}
