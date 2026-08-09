package api

import "net/http"

// handleHealthz is GET /v1alpha1/healthz: a pure liveness probe. It never
// touches the database — a database outage is a readiness concern
// (handleReadyz), not a liveness one; conflating the two would make a
// transient DB blip restart an otherwise-healthy process under a
// liveness-probe-triggers-restart policy.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, HealthOut{Status: "ok"})
	return nil
}

// handleReadyz is GET /v1alpha1/readyz: pings the database and reports
// whether this process is ready to serve traffic.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) error {
	if err := s.Store.Pool().Ping(r.Context()); err != nil {
		return unavailable("wait for the database to become reachable and retry", "database ping failed: %v", err)
	}
	writeJSON(w, http.StatusOK, HealthOut{Status: "ok"})
	return nil
}
