package api

import (
	"net/http"
)

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) error {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		return unauthorized("authenticate through the Access listener", "no_principal")
	}
	principal := map[string]any{"provider": p.Provider, "subject": p.Subject}
	if p.Email != "" {
		principal["email"] = p.Email
	}
	if p.CommonName != "" {
		principal["common_name"] = p.CommonName
	}
	out := map[string]any{"principal": principal}
	if p.ActorID == "" {
		out["unbound"] = true
	} else {
		out["actor_id"] = p.ActorID
		out["roles"] = p.Roles
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
