package actors

import (
	"encoding/json"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// originActorMismatch enforces callback custody before the completion can
// reach a ledger write. The durable pending invocation is the dispatched
// identity; the delta is only the actor's claim. Task t24 deliberately
// refuses disagreement instead of stamping the trusted identity over it —
// server-side custody remains #111's question.
func originActorMismatch(records []ledger.Record, dispatchedActorID string) string {
	for _, record := range records {
		if record.Origin.ActorID != dispatchedActorID {
			return fmt.Sprintf("origin_actor_id %s is not the dispatched actor %s", record.Origin.ActorID, dispatchedActorID)
		}
	}
	return ""
}

func identityDiagnostic(detail string) json.RawMessage {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{"class": "identity", "detail": detail},
	})
	return body
}
