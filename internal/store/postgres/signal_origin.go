package postgres

import (
	"strings"

	"github.com/agentculture/culture-nodes/internal/decl/kinds"
)

// checkSignalOrigin admits the schedule emitter only from FireSchedule's
// locked-row path. External callers cannot set the unexported scheduleFire
// flag. Timer is the sole reserved name that path may assert.
func checkSignalOrigin(in DeliverSignalEventInput) error {
	if in.scheduleFire && strings.HasPrefix(in.Emitter, "schedule:") &&
		(in.Name == "timer" || !kinds.ReservedEvent(in.Name)) {
		return nil
	}
	return kinds.CheckExternalEvent(in.Name, in.Emitter)
}
