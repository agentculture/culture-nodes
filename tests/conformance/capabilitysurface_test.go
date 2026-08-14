package conformance_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/tests/conformance"
)

// Issue #67, task t15, acceptance criterion 2, as a protocol property: a
// bridge that does not advertise the capability surface is unaffected.
//
// The sibling `TestReferenceActorPassesTheConformanceKit` runs the whole kit
// against an actor that DOES advertise, so the shape check is exercised
// there. This runs the same kit against the same actor with the surface
// muted, and requires it to pass all the same. A capability check that only
// ever ran against an advertising actor could have been quietly mandatory
// and nobody would have found out until a conformant fourth adapter, or
// adapters/human-inbox, failed a suite it should never have been failing.
func TestAnActorThatAdvertisesNoCapabilitySurfaceIsStillConformant(t *testing.T) {
	actor := conformance.NewReferenceActor("reference-workload-token").WithoutCapabilitySurface()
	defer actor.Close()

	// The fixture really is silent there — otherwise the run below would be
	// exercising the advertising path under a name that says it does not.
	req, err := http.NewRequest(http.MethodGet, actor.URL()+actors.CapabilitiesPath, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer reference-workload-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("probe the muted capability route: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the muted actor answered %d at %s; the fixture is not silent, so this test "+
			"would not be measuring what it says", resp.StatusCode, actors.CapabilitiesPath)
	}

	conformance.Run(t, conformance.Config{
		Endpoint:  actor.URL(),
		AuthToken: "reference-workload-token",

		WorkflowName:   "conformance-reference",
		WorkflowDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		NodeID:         "reference",
		ContractDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",

		Input:      json.RawMessage(`{"subject":"synchronous"}`),
		AsyncInput: json.RawMessage(`{"async":true,"delay_ms":50}`),
		BadInput:   json.RawMessage(`{"reject":true}`),

		ExpectCallbackRetry: true,
		RequireCancellation: true,
	})
}
