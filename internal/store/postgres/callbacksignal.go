package postgres

import (
	"context"
	"errors"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// Mid-execution emission (issue #43 task t21, parallel-tokens design D11):
// the store half of the `signal` callback event kind.
//
// An actor node that wants to speak to the rest of the system WHILE STILL
// WORKING — the design's example is an agent calling a human node for a reply
// and carrying on — POSTs a non-terminal `signal` event to the callback route
// it already has, authenticated by the attempt-scoped HMAC token
// (internal/actors/token.go). internal/actors' ingest derives the emitter from
// that verified attempt and lands here.
//
// Why this is the same delivery an external POST /v1alpha1/events performs,
// and not a narrower "append only" path: an emission nobody can hear is not a
// feature. The fact must fire pending subscriptions and active event routes
// exactly as an external delivery does, or a human node parked on the name
// would never wake — which is the whole scenario the design names.
//
// Why it does not touch the emitting attempt: DeliverSignalEvent takes the
// advisory locks of the runs it RESUMES, and the emitting attempt is parked
// (waiting_external) holding no lock and no lease. Its own completion,
// whenever it comes, is a separate transaction that queues behind this one.
// There is therefore no path by which emitting can complete, block, or fail
// the session that emitted.

// EmitSignalEvent implements actors.CallbackStore. The namespace comes from
// the verified invocation record, falling back to the store's own binding: a
// caller never names it.
func (cs *CallbackStore) EmitSignalEvent(
	ctx context.Context, inv actors.PendingInvocation, in actors.EmitSignalInput,
) (actors.EmitSignalResult, error) {
	switch {
	case in.Name == "":
		return actors.EmitSignalResult{}, errors.New("postgres: EmitSignalEvent: name is required")
	case in.Emitter == "":
		// The ingest derives this; an empty one means the derivation broke,
		// and an unattributed fact is worse than a refused emission.
		return actors.EmitSignalResult{}, errors.New("postgres: EmitSignalEvent: emitter is required")
	}

	namespaceID := inv.NamespaceID
	if namespaceID == "" {
		namespaceID = cs.namespaceID
	}

	ev, fired, err := cs.store.DeliverSignalEvent(ctx, DeliverSignalEventInput{
		NamespaceID: namespaceID,
		Name:        in.Name,
		Payload:     in.Payload,
		Emitter:     in.Emitter,
		RunID:       in.RunID,
	})
	if err != nil {
		return actors.EmitSignalResult{}, err
	}
	return actors.EmitSignalResult{EventID: ev.ID, Resumed: len(fired)}, nil
}
