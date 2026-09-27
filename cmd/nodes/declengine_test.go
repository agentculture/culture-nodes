package main

import (
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Task t38, acceptance 6: the declaration engine's marker key is read at
// startup through Config.MarkerKeyEnv, and an enabled engine without a usable
// key refuses to start rather than running with none.
func TestDeclarationEngineFromEnvFailsLoudWithoutTheMarkerKey(t *testing.T) {
	db := &postgres.Store{} // NewPostgres only wires it; nothing here queries
	for _, tc := range []struct {
		name, enabled, key string
		wantEngine         bool
		wantErr            bool
	}{
		{name: "unset is off", enabled: "", wantEngine: false},
		{name: "off is off, key or not", enabled: "off", key: strings.Repeat("k", 32), wantEngine: false},
		{name: "on without a key refuses", enabled: "on", key: "", wantErr: true},
		{name: "on with a short key refuses", enabled: "on", key: "too-short", wantErr: true},
		{name: "on with a key starts", enabled: "on", key: strings.Repeat("k", 32), wantEngine: true},
		{name: "a typo is refused, not read as off", enabled: "yes please", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envDeclarationEngine, tc.enabled)
			t.Setenv(declengine.DefaultMarkerKeyEnv, tc.key)
			e, cliErr := declarationEngineFromEnv(db)
			if tc.wantErr {
				if cliErr == nil {
					t.Fatalf("started (engine=%v), want a refusal", e != nil)
				}
				if cliErr.Remediation == "" {
					t.Error("a refusal with no hint: line is not this CLI's error contract")
				}
				return
			}
			if cliErr != nil {
				t.Fatalf("refused: %v", cliErr)
			}
			if (e != nil) != tc.wantEngine {
				t.Fatalf("engine=%v, want %v", e != nil, tc.wantEngine)
			}
			if d := declarationDriver(db, e); (d != nil) != tc.wantEngine {
				t.Fatalf("scheduler option non-nil=%v, want %v (a disabled engine must be a nil interface)", d != nil, tc.wantEngine)
			}
		})
	}
}
