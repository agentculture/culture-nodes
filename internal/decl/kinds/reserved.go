package kinds

import (
	"fmt"
	"strings"
)

// ControlPlaneEmitter is the emitter the declaration engine writes on every
// signal event it emits itself (task t38g, #328, cortex review finding A2):
// the reactions (human.decision, code.result, agent.result), node.expired,
// and the action.* results. It is also the engine's ledger producer
// identity (declengine.DeclarationEngineActorID is this constant).
const ControlPlaneEmitter = "engine_declaration_engine"

// controlPlaneEvents are the trigger kinds only the control plane emits.
// A reaction's origin marker proves which firing it continues, not what
// the reaction says, so an outside delivery of one of these names could
// carry a copied marker and a forged outcome. ReservedEvent also reserves
// every "action." name: the action.* results are the engine's reports on
// its own dispatches.
var controlPlaneEvents = map[string]bool{
	"timer":                       true,
	string(ArtifactHumanDecision): true,
	string(ArtifactCodeResult):    true,
	"agent.result":                true,
	"node.expired":                true,
}

const controlPlaneEventPrefix = "action."

// ReservedEvent reports whether name is a signal event only the control
// plane may emit.
func ReservedEvent(name string) bool {
	return controlPlaneEvents[name] || strings.HasPrefix(name, controlPlaneEventPrefix)
}

// ReservedEventError is CheckExternalEvent's refusal. Name is set when the
// event name is reserved; Emitter when the emitter claims to be the
// control plane.
type ReservedEventError struct {
	Name, Emitter string
}

func (e *ReservedEventError) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("event name %q is reserved: only the control plane emits it", e.Name)
	}
	return fmt.Sprintf("emitter %q is reserved: it names an internal event source", e.Emitter)
}

// CheckExternalEvent is the one check every ingress that appends a signal
// event on someone else's behalf applies before it appends: it refuses a
// control-plane event name and the control plane's own emitter. The
// engine's emitters append their events directly and never pass through it.
func CheckExternalEvent(name, emitter string) error {
	if ReservedEvent(name) {
		return &ReservedEventError{Name: name}
	}
	if emitter == ControlPlaneEmitter || strings.HasPrefix(emitter, "schedule:") {
		return &ReservedEventError{Emitter: emitter}
	}
	return nil
}
