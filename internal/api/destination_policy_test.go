package api

import (
	"net/http"
	"testing"
)

func TestDestinationAudienceRejectsAgentPolicy(t *testing.T) {
	p, protected := principalPolicy(http.MethodPost, destinationAudiencesPath)
	if !protected || p.agents || p.secret != "" {
		t.Fatalf("destination write policy = %+v, protected=%v", p, protected)
	}
}
