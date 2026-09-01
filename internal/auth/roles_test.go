package auth

import "testing"

func TestParseRoleAcceptsKnownRoles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  Role
	}{
		{"viewer", RoleViewer},
		{"approver", RoleApprover},
		{"namespace_administrator", RoleNamespaceAdministrator},
	}
	for _, test := range tests {
		test := test
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()
			got, err := ParseRole(test.input)
			if err != nil {
				t.Fatalf("ParseRole(%q): %v", test.input, err)
			}
			if got != test.want {
				t.Errorf("ParseRole(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestParseRoleRejectsUnknownRoles(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "admin", "Viewer", "namespace-admin"} {
		if got, err := ParseRole(input); err == nil {
			t.Errorf("ParseRole(%q) = %q, nil; want an error", input, got)
		}
	}
}
