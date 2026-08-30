package actors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// originActorMismatch enforces callback custody before the completion can
// reach a ledger write. The durable pending invocation is the dispatched
// identity; the delta is only the actor's claim. Task t24 deliberately
// refuses disagreement instead of stamping the trusted identity over it —
// server-side custody remains #111's question.
//
// The two return values are different answers, and collapsing them is the
// bug this signature exists to prevent. The string is a REFUSAL — custody
// was decided and the delta lost, which commits contract_rejected and
// refuses redispatch, permanently. The error is UNDECIDED — the store could
// not say — and must fail the delivery instead, so the at-least-once
// redelivery can decide it once the database is back.
func originActorMismatch(
	ctx context.Context, store CallbackStore, records []ledger.Record, dispatchedActorID string,
) (string, error) {
	refusal := func(origin string) string {
		return fmt.Sprintf("origin_actor_id %s is not the dispatched actor %s", origin, dispatchedActorID)
	}
	var dispatchedKey string
	// One lookup per distinct origin row, not per record: a delta is bounded
	// by the 4 MiB body, not by a record count, and the engine's record
	// budget is checked after custody (Qodo on PR #264).
	seen := map[string]bool{}
	for _, record := range records {
		origin := record.Origin.ActorID
		if origin == dispatchedActorID || seen[origin] {
			continue
		}
		// Different row: the same actor when both rows share one actor_key
		// (a bridge's issued identity row versus the worker's registration
		// row — see #117/#183). Anything else, including an unknown origin,
		// is refused.
		if dispatchedKey == "" {
			key, err := store.ActorKey(ctx, dispatchedActorID)
			switch {
			case errors.Is(err, ErrUnknownActor):
				// Not transient and not waiting on anything: the dispatched
				// identity has no row to share a key with, so no origin can
				// ever match it.
				return fmt.Sprintf("%s (dispatched actor unresolvable: %v)", refusal(origin), err), nil
			case err != nil:
				return "", fmt.Errorf("actors: callback custody could not resolve dispatched actor %s: %w",
					dispatchedActorID, err)
			}
			dispatchedKey = key
		}
		originKey, err := store.ActorKey(ctx, origin)
		switch {
		case errors.Is(err, ErrUnknownActor):
			return refusal(origin), nil
		case err != nil:
			return "", fmt.Errorf("actors: callback custody could not resolve origin actor %s: %w", origin, err)
		}
		if originKey != dispatchedKey {
			return refusal(origin), nil
		}
		seen[origin] = true
	}
	return "", nil
}

func identityDiagnostic(detail string) json.RawMessage {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{"class": "identity", "detail": detail},
	})
	return body
}
