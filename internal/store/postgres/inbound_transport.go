package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/jackc/pgx/v5"
)

// InboundEnvelope is durable work claimed by a dialled-in bridge.
type InboundEnvelope struct {
	ID        string          `json:"id"`
	ActorKey  string          `json:"actor_key"`
	AttemptID string          `json:"attempt_id"`
	Request   json.RawMessage `json:"request"`
}

func (s *Store) TouchInboundActor(ctx context.Context, namespaceID, actorKey string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO inbound_actor_presence(namespace_id, actor_key, last_seen_at)
		VALUES ($1,$2,now()) ON CONFLICT(namespace_id,actor_key)
		DO UPDATE SET last_seen_at=excluded.last_seen_at`, namespaceID, actorKey)
	return err
}

func (s *Store) InboundActorAvailable(ctx context.Context, namespaceID, actorKey string, since time.Time) (bool, error) {
	var available bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM inbound_actor_presence
		WHERE namespace_id=$1 AND actor_key=$2 AND last_seen_at >= $3)`, namespaceID, actorKey, since).Scan(&available)
	return available, err
}

func (s *Store) ClaimInbound(ctx context.Context, namespaceID, actorKey string) (*InboundEnvelope, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var e InboundEnvelope
	err = tx.QueryRow(ctx, `SELECT id, actor_key, attempt_id, request FROM inbound_actor_mailbox
		WHERE namespace_id=$1 AND actor_key=$2 AND completed_at IS NULL
		  AND (claimed_at IS NULL OR claim_until <= now())
		ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`, namespaceID, actorKey).
		Scan(&e.ID, &e.ActorKey, &e.AttemptID, &e.Request)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE inbound_actor_mailbox SET claimed_at=now(), claim_until=now()+interval '90 seconds' WHERE id=$1`, e.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) CompleteInbound(ctx context.Context, namespaceID, actorKey, id string, status int, response json.RawMessage) error {
	tag, err := s.pool.Exec(ctx, `UPDATE inbound_actor_mailbox SET response=$1,response_status=$2,completed_at=now()
		WHERE id=$3 AND namespace_id=$4 AND actor_key=$5 AND completed_at IS NULL`, response, status, id, namespaceID, actorKey)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("postgres: inbound envelope is absent or already completed")
	}
	return nil
}

// InvokeInbound implements actors.DialInInvoker using the durable mailbox.
func (s *Store) InvokeInbound(ctx context.Context, namespaceID, actorKey string, req actors.InvocationRequest) (actors.InvocationResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return actors.InvocationResponse{}, err
	}
	var id string
	err = s.pool.QueryRow(ctx, `INSERT INTO inbound_actor_mailbox(namespace_id,actor_key,attempt_id,request)
		VALUES($1,$2,$3,$4) ON CONFLICT(namespace_id,attempt_id) DO UPDATE SET actor_key=excluded.actor_key
		RETURNING id`, namespaceID, actorKey, req.AttemptID, body).Scan(&id)
	if err != nil {
		return actors.InvocationResponse{}, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var response []byte
		var status int
		err = s.pool.QueryRow(ctx, `SELECT response,response_status FROM inbound_actor_mailbox WHERE id=$1 AND completed_at IS NOT NULL`, id).Scan(&response, &status)
		if err == nil {
			return actors.ParseInvocationResponse(status, response)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return actors.InvocationResponse{}, err
		}
		select {
		case <-ctx.Done():
			return actors.InvocationResponse{}, fmt.Errorf("dial-in invocation: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
