package kinds

import "testing"

func TestExternalTimerAndScheduleEmitterReserved(t *testing.T) {
	for _, tc := range []struct{ name, emitter string }{
		{"timer", "external"},
		{"ordinary", "schedule:forged"},
	} {
		if err := CheckExternalEvent(tc.name, tc.emitter); err == nil {
			t.Errorf("accepted external %s from %s", tc.name, tc.emitter)
		}
	}
	if err := CheckExternalEvent("ordinary", "external"); err != nil {
		t.Fatal(err)
	}
}
