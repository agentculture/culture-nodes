package actors

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// originActorMismatch enforces callback custody before the completion can
// reach a ledger write. The durable pending invocation is the dispatched
// identity; the delta is only the actor's claim. Task t24 deliberately
// refuses disagreement instead of stamping the trusted identity over it —
// server-side custody remains #111's question.
func originActorMismatch(ctx context.Context, store CallbackStore, records []ledger.Record, dispatchedActorID string) string {
	var dispatchedKey string
	for _, record := range records {
		origin := record.Origin.ActorID
		if origin == dispatchedActorID {
			continue
		}
		// Different row: the same actor when both rows share one actor_key
		// (a bridge's issued identity row versus the worker's registration
		// row — see #117/#183). Anything else, including an unknown origin,
		// is refused.
		if dispatchedKey == "" {
			key, err := store.ActorKey(ctx, dispatchedActorID)
			if err != nil {
				return fmt.Sprintf("origin_actor_id %s is not the dispatched actor %s (dispatched actor unresolvable: %v)", origin, dispatchedActorID, err)
			}
			dispatchedKey = key
		}
		originKey, err := store.ActorKey(ctx, origin)
		if err != nil || originKey != dispatchedKey {
			return fmt.Sprintf("origin_actor_id %s is not the dispatched actor %s", origin, dispatchedActorID)
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
