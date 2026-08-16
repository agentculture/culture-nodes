package api_test

import (
	"net/http"
	"strings"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
)

// versionOut mirrors components.schemas.Version.
type versionOut struct {
	Version        string `json:"version"`
	Revision       string `json:"revision"`
	RevisionSource string `json:"revision_source"`
	Dirty          bool   `json:"revision_is_dirty"`
	Staleness      string `json:"staleness"`
}

const buildRevision = "774d5153c32a2e2fdb86f699d814977d111f1408"

func getVersion(t *testing.T, f *fixture) versionOut {
	t.Helper()
	var out versionOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/version"), nil, &out)
	requireStatus(t, resp, body, http.StatusOK)
	return out
}

// The point of the route (issue #104): "what revision is the control plane
// running" stops being a question you answer by probing feature routes one at
// a time and hoping to infer the answer from a 405.
func TestVersionReportsTheRevisionTheBuildWasStampedWith(t *testing.T) {
	f := newFixtureWithBuild(t, "1.2.3", buildRevision)

	out := getVersion(t, f)

	if out.Version != "1.2.3" {
		t.Errorf("version = %q, want 1.2.3", out.Version)
	}
	if out.Revision != buildRevision {
		t.Errorf("revision = %q, want %q", out.Revision, buildRevision)
	}
	if out.RevisionSource == "" {
		t.Error("revision_source is empty: a revision with no account of how it was learned cannot " +
			"be weighed against a work tree that might have moved")
	}
}

// A build that was never stamped must say so rather than report a revision it
// does not have. 405-probing a route is how #104 was found; a route that
// answered with an empty string would have been no better.
func TestAnUnstampedBuildSaysItsRevisionIsUnknown(t *testing.T) {
	f := newFixtureWithBuild(t, "0.1.0-dev", "")

	out := getVersion(t, f)

	if out.Revision != "" {
		t.Errorf("revision = %q, want empty when nothing stamped one", out.Revision)
	}
	if !strings.Contains(strings.ToLower(out.Staleness), "cannot") {
		t.Errorf("staleness = %q, want it to state plainly that the revision cannot be established",
			out.Staleness)
	}
}

// The route is authless, exactly as healthz and readyz are. A live test that
// had to hold a decision secret just to learn which code it was testing would
// be a live test nobody runs.
func TestVersionNeedsNoCredential(t *testing.T) {
	f := newFixtureWithBuild(t, "1.2.3", buildRevision)

	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/version"), nil, &versionOut{})

	requireStatus(t, resp, body, http.StatusOK)
}

// A build stamped with something that is not a full commit id is refused the
// same way the bridges refuse one: the value exists to be compared later, and
// "main" or an abbreviation compares to nothing.
func TestAVersionRouteRefusesARevisionThatIsNotAFullCommit(t *testing.T) {
	for _, bad := range []string{"main", "HEAD", "774d515", strings.ToUpper(buildRevision)} {
		f := newFixtureWithBuild(t, "1.2.3", bad)
		if out := getVersion(t, f); out.Revision != "" {
			t.Errorf("%q was reported as a revision; want it refused", bad)
		}
	}
}

func newFixtureWithBuild(t *testing.T, version, revision string) *fixture {
	t.Helper()
	return newFixtureWithDecisionAuth(t, decisionAuthSecret, apipkg.WithBuildInfo(version, revision))
}
