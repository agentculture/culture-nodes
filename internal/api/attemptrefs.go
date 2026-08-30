package api

import (
	"context"
	"encoding/json"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// recordedAttemptID resolves a protocol attempt reference to the ledger's
// foreign-key-backed envelope field. Graph-dispatched attempts are not written
// to attempts until completion, so an unknown reference is provenance, not a
// valid attempt_id yet.
func (s *Server) recordedAttemptID(ctx context.Context, nodeRunID, attemptRef string) (string, error) {
	if nodeRunID == "" || attemptRef == "" {
		return "", nil
	}
	attempts, err := s.engineStore.Attempts(ctx, nodeRunID)
	if err != nil {
		return "", err
	}
	for _, attempt := range attempts {
		if attempt.ID == attemptRef {
			return attemptRef, nil
		}
	}
	return "", nil
}

func withAttemptRef(record ledger.Record, attemptRef string) (ledger.Record, error) {
	if attemptRef == "" {
		return record, nil
	}
	var data map[string]any
	if err := json.Unmarshal(record.Data, &data); err != nil {
		return ledger.Record{}, err
	}
	data["attempt_ref"] = attemptRef
	payload, err := json.Marshal(data)
	if err != nil {
		return ledger.Record{}, err
	}
	record.Data = payload
	return record, nil
}
