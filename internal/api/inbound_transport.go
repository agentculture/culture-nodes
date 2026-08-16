package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type inboundCompletion struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func bearerValue(r *http.Request) string {
	scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

func (s *Server) authenticateInbound(w http.ResponseWriter, r *http.Request) (string, bool) {
	actorKey := r.Header.Get("X-Culture-Nodes-Actor-Key")
	if actorKey == "" || s.inboundAuthenticator == nil {
		http.Error(w, "dial-in authentication is not configured", http.StatusUnauthorized)
		return "", false
	}
	decision, err := s.inboundAuthenticator.Authenticate(r.Context(), "actor", actorKey, bearerValue(r))
	if err != nil {
		s.writeAPIError(w, r, err)
		return "", false
	}
	if !decision.Allowed {
		http.Error(w, "dial-in refused: "+string(decision.Reason), http.StatusUnauthorized)
		return "", false
	}
	return actorKey, true
}

func (s *Server) handleInboundPoll(w http.ResponseWriter, r *http.Request) {
	actorKey, ok := s.authenticateInbound(w, r)
	if !ok {
		return
	}
	if err := s.Store.TouchInboundActor(r.Context(), s.NamespaceID, actorKey); err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		envelope, err := s.Store.ClaimInbound(r.Context(), s.NamespaceID, actorKey)
		if err != nil {
			s.writeAPIError(w, r, err)
			return
		}
		if envelope != nil {
			writeJSON(w, http.StatusOK, envelope)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) handleInboundComplete(w http.ResponseWriter, r *http.Request) {
	actorKey, ok := s.authenticateInbound(w, r)
	if !ok {
		return
	}
	var completion inboundCompletion
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&completion); err != nil {
		http.Error(w, "invalid completion", http.StatusBadRequest)
		return
	}
	if completion.Status < 100 || completion.Status > 599 || len(completion.Body) == 0 {
		http.Error(w, "status and body are required", http.StatusBadRequest)
		return
	}
	if err := s.Store.CompleteInbound(r.Context(), s.NamespaceID, actorKey, r.PathValue("id"), completion.Status, completion.Body); err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
