package actors

import (
	"context"
	"errors"
	"testing"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// keyStore answers ActorKey only; every other CallbackStore method is the
// embedded nil interface and must not be reached by originActorMismatch.
type keyStore struct {
	CallbackStore
	keys  map[string]string
	fail  error
	calls int
}

func (s *keyStore) ActorKey(_ context.Context, id string) (string, error) {
	s.calls++
	if s.fail != nil {
		return "", s.fail
	}
	key, ok := s.keys[id]
	if !ok {
		return "", ErrUnknownActor
	}
	return key, nil
}

func records(origins ...string) []ledger.Record {
	out := make([]ledger.Record, 0, len(origins))
	for _, o := range origins {
		out = append(out, ledger.Record{Origin: ledger.Origin{ActorID: o}})
	}
	return out
}

// Qodo on PR #264, finding 1: a store that cannot answer is not a foreign
// actor. Only an unknown row or a different key is a refusal.
func TestOriginActorMismatchTransientLookupIsAnErrorNotARefusal(t *testing.T) {
	store := &keyStore{fail: errors.New("dial tcp: connection refused")}
	detail, err := originActorMismatch(context.Background(), store, records("row-b"), "row-a")
	if err == nil {
		t.Fatalf("transient lookup failure: detail=%q err=nil, want an error", detail)
	}
	if detail != "" {
		t.Fatalf("transient lookup failure produced a refusal %q; that would set RetryRefusal permanently", detail)
	}
}

func TestOriginActorMismatchUnknownOriginIsRefused(t *testing.T) {
	store := &keyStore{keys: map[string]string{"row-a": "company/dev"}}
	detail, err := originActorMismatch(context.Background(), store, records("row-nope"), "row-a")
	if err != nil {
		t.Fatalf("unknown origin: err=%v, want a refusal without an error", err)
	}
	if detail == "" {
		t.Fatal("unknown origin was accepted")
	}
}

func TestOriginActorMismatchSameKeyIsAccepted(t *testing.T) {
	store := &keyStore{keys: map[string]string{"row-a": "company/dev", "row-b": "company/dev", "row-c": "other/actor"}}
	detail, err := originActorMismatch(context.Background(), store, records("row-b", "row-a"), "row-a")
	if err != nil || detail != "" {
		t.Fatalf("same actor_key: detail=%q err=%v, want accepted", detail, err)
	}
	detail, err = originActorMismatch(context.Background(), store, records("row-b", "row-c"), "row-a")
	if err != nil || detail == "" {
		t.Fatalf("different actor_key: detail=%q err=%v, want refused", detail, err)
	}
}

// Qodo on PR #264, finding 2: one lookup per distinct origin row, not one
// per record — the dispatched row plus one alternate row cost two queries
// however many records the delta carries.
func TestOriginActorMismatchResolvesEachOriginOnce(t *testing.T) {
	store := &keyStore{keys: map[string]string{"row-a": "company/dev", "row-b": "company/dev"}}
	many := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		many = append(many, "row-b")
	}
	detail, err := originActorMismatch(context.Background(), store, records(many...), "row-a")
	if err != nil || detail != "" {
		t.Fatalf("detail=%q err=%v, want accepted", detail, err)
	}
	if store.calls != 2 {
		t.Fatalf("ActorKey called %d times for 500 records of one alternate row, want 2", store.calls)
	}
}
