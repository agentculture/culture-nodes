package auth

import "fmt"

// Role is an authorization role carried by an actor identity binding.
type Role string

const (
	RoleViewer                 Role = "viewer"
	RoleApprover               Role = "approver"
	RoleNamespaceAdministrator Role = "namespace_administrator"
)

// ParseRole returns the role represented by s, rejecting values outside the
// deliberately closed authorization vocabulary.
func ParseRole(s string) (Role, error) {
	role := Role(s)
	switch role {
	case RoleViewer, RoleApprover, RoleNamespaceAdministrator:
		return role, nil
	default:
		return "", fmt.Errorf("auth: unknown role %q", s)
	}
}
