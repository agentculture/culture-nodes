package main

import (
	"strings"
	"testing"
)

// The deployment surface for handover evidence (task t10, issue #13).
//
// Two properties, and the second is the one that matters: an unconfigured
// deployment records nothing (and says nothing), while a HALF-configured one
// refuses to start. An operator who set a remote has stated an intent for
// handed-over refs to be measured; a control plane that shrugged and recorded
// nothing would leave them believing evidence exists when none does — which
// is the same class of silence this task was written to end.
//
// A fully-configured observer needs a database handle, so it is exercised
// where a database exists: internal/worker/handover_test.go builds the same
// Observer against a real PostgreSQL and a real git remote, on both terminal
// paths.

func TestHandoverObserverIsAbsentByDefault(t *testing.T) {
	t.Setenv(envHandoverRemote, "")
	t.Setenv(envHandoverActorID, "")

	observer, err := handoverObserver(nil, "ns")
	if err != nil {
		t.Fatalf("handoverObserver with nothing set: %v", err)
	}
	if observer != nil {
		t.Fatal("an unconfigured deployment built an observer; it must record nothing at all")
	}
}

func TestARemoteWithNoMeasuringActorRefusesToStart(t *testing.T) {
	t.Setenv(envHandoverRemote, "/srv/repos/culture-nodes.git")
	t.Setenv(envHandoverActorID, "")

	observer, err := handoverObserver(nil, "ns")
	if err == nil {
		t.Fatal("a half-configured deployment must refuse rather than silently record nothing")
	}
	if observer != nil {
		t.Fatal("a refused configuration must not also return an observer")
	}
	if !strings.Contains(err.Message, envHandoverActorID) {
		t.Errorf("the refusal does not name the missing variable: %s", err.Message)
	}
	if err.Remediation == "" {
		t.Error("the refusal carries no remediation")
	}
}

func TestAMeasuringActorWithNoRemoteRefusesToStart(t *testing.T) {
	t.Setenv(envHandoverRemote, "")
	t.Setenv(envHandoverActorID, "handover-fetch")

	if _, err := handoverObserver(nil, "ns"); err == nil {
		t.Fatal("an actor id with nowhere to fetch from must refuse rather than start")
	} else if !strings.Contains(err.Message, envHandoverRemote) {
		t.Errorf("the refusal does not name the missing variable: %s", err.Message)
	}
}
