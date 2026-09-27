package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/scheduler"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The declaration engine's process configuration (task t38, #328). Both
// `nodes serve` (event deliveries, the switch route) and `nodes scheduler`
// (node deadlines, action.* results, thaw-and-replay, schedule fires) run the
// engine, and both must read the SAME marker key: a marker one process mints
// is verified by whichever process receives the reaction.
const (
	// envDeclarationEngine enables the engine: on/true/1. Unset (or
	// off/false/0) leaves every delivery and tick exactly as before t38.
	envDeclarationEngine = "NODES_DECLARATION_ENGINE"
	// envDeclarationProducerActorID overrides the registered producer
	// identity a firing's derived decision record is written under
	// (declengine.DeclarationEngineActorID, engine_declaration_engine).
	envDeclarationProducerActorID = "NODES_DECLARATION_PRODUCER_ACTOR_ID"
)

// declarationEngineFromEnv builds the engine when it is enabled, reading the
// origin-marker key from declengine.DefaultMarkerKeyEnv. Enabled without a
// usable key is a startup refusal, never a silently disabled engine: a
// control plane an operator believes is evaluating declarations must not be
// quietly evaluating none.
func declarationEngineFromEnv(db *postgres.Store) (*declengine.Engine, *clifmt.CliError) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(envDeclarationEngine)))
	switch raw {
	case "", "off", "false", "0":
		return nil, nil
	case "on", "true", "1":
	default:
		return nil, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("%s=%q is not on or off", envDeclarationEngine, raw),
			Remediation: "set it to on to run the declaration engine, or unset it",
		}
	}
	e, err := declengine.NewPostgres(declengine.Config{MarkerKeyEnv: declengine.DefaultMarkerKeyEnv}, db, os.Getenv(envDeclarationProducerActorID))
	if err != nil {
		return nil, &clifmt.CliError{
			Code:    clifmt.ExitEnvError,
			Message: fmt.Sprintf("%s=on but the declaration engine cannot start: %v", envDeclarationEngine, err),
			Remediation: fmt.Sprintf("set %s to a secret of at least 32 bytes, identical on every process that runs the engine, or unset %s",
				declengine.DefaultMarkerKeyEnv, envDeclarationEngine),
		}
	}
	return e, nil
}

// declarationDriver adapts an enabled engine to the scheduler's option. A nil
// engine must stay a nil interface, not a typed nil the scheduler would call.
func declarationDriver(db *postgres.Store, e *declengine.Engine) scheduler.DeclarationEngine {
	if e == nil {
		return nil
	}
	return declengine.Driver{Engine: e, Store: db}
}
