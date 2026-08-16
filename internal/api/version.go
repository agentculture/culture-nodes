package api

import (
	"net/http"
	"runtime/debug"
	"strings"
)

// Which revision of the control plane is answering (task t32, issue #104).
//
// # What this replaces
//
// A probe, one route at a time:
//
//	$ curl -o /dev/null -w '%{http_code}' -X POST \
//	    http://192.168.1.146:18080/v1alpha1/attempts/att_probe/artifacts
//	405
//
// 401 would have meant the route exists and is rejecting the token. 405 meant
// it was not there — which is how issue #104 established that a whole batch of
// work was not running in production, fifteen hours after it merged. Nothing
// reported it; somebody went looking. A live test against that control plane
// would have produced a green measuring the previous release, which is the
// same "green that measured nothing" the merge gate exists to prevent, one
// layer up.
//
// So the route exists to make a live test able to ASSERT which code it is
// testing rather than assume it. That is why it is unauthenticated, like
// healthz and readyz: a live test that had to hold a decision secret just to
// learn what it was testing is a live test nobody runs.
//
// # Why it refuses a partial answer
//
// The revision must be a full 40-hex commit id or it is not reported at all,
// and the same three refusals internal/handover's validateFullSHA makes apply
// for the same reason — `HEAD`, a branch name and an abbreviation each mean
// something different tomorrow, and this value exists to be compared later.
// An unstamped build reports no revision and says why, rather than an empty
// string a reader has to interpret.

// RevisionFromBuildFlag and RevisionFromVCSStamp name how the revision was
// learned, the same distinction the bridges' deployment.py draws for the same
// reason: the two carry different guarantees.
//
// A `-ldflags -X` value is what the DEPLOY said it was building — the
// container image is built from a `git archive` with no .git in it, so this is
// the only thing that can answer there. The Go toolchain's own vcs stamp is
// what the BUILD MACHINE observed, available only when the build ran inside a
// checkout, and it comes with `vcs.modified`, which a ldflags value cannot.
const (
	RevisionFromBuildFlag = "build_flag"
	RevisionFromVCSStamp  = "go_vcs_stamp"
)

// versionOut is components.schemas.Version.
type versionOut struct {
	Version        string `json:"version"`
	Revision       string `json:"revision,omitempty"`
	RevisionSource string `json:"revision_source,omitempty"`
	// RevisionIsDirty is only ever true for a build the Go toolchain stamped
	// from a modified checkout. A `-X` build flag cannot know it, so its
	// absence here is not a claim that the tree was clean.
	RevisionIsDirty bool `json:"revision_is_dirty,omitempty"`
	// Staleness is one sentence a reader needs and cannot derive: what this
	// answer does and does not establish.
	Staleness string `json:"staleness"`
}

// WithBuildInfo tells the server which version and revision it was built as.
//
// Both come from cmd/nodes's `-ldflags -X` values, because the container is
// built from a source tree with no .git in it (see the Dockerfile) and the Go
// toolchain therefore stamps no vcs information into it. A binary built
// straight from a checkout has that stamp instead, and handleVersion falls
// back to it — so `go build ./cmd/nodes && ./nodes serve` answers correctly
// with no flags at all, which is the case a developer actually hits.
func WithBuildInfo(version, revision string) Option {
	return func(s *Server) {
		if version != "" {
			s.buildVersion = version
		}
		s.buildRevision = revision
	}
}

// handleVersion is GET /v1alpha1/version.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, s.versionOut())
	return nil
}

func (s *Server) versionOut() versionOut {
	out := versionOut{Version: s.buildVersion}

	if revision := fullCommitSHA(s.buildRevision); revision != "" {
		out.Revision = revision
		out.RevisionSource = RevisionFromBuildFlag
		out.Staleness = "this binary was stamped at build time with the revision the deploy shipped; " +
			"it names the commit the SOURCE came from and cannot say whether that tree was clean"
		return out
	}

	if revision, dirty, ok := vcsStamp(); ok {
		out.Revision = revision
		out.RevisionSource = RevisionFromVCSStamp
		out.RevisionIsDirty = dirty
		out.Staleness = "this binary was built inside a checkout and the Go toolchain stamped the " +
			"revision it saw"
		if dirty {
			out.Staleness += "; that checkout had UNCOMMITTED changes, so the revision names the " +
				"last commit and not the code that is running"
		}
		return out
	}

	out.Staleness = "this binary's revision CANNOT BE ESTABLISHED: it carries no build-time stamp and " +
		"was not built inside a git checkout. A live test against it can say what it measured but " +
		"not which code it measured — build with -ldflags \"-X main.revision=$(git rev-parse HEAD)\""
	return out
}

// vcsStamp reads the revision the Go toolchain recorded, which it does only
// for a build that ran inside a version-controlled directory.
func vcsStamp() (revision string, dirty bool, ok bool) {
	info, available := debug.ReadBuildInfo()
	if !available {
		return "", false, false
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = fullCommitSHA(setting.Value)
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	return revision, dirty, revision != ""
}

// fullCommitSHA is internal/handover's validateFullSHA as a predicate: value
// if it is an unambiguous 40-character lowercase hex commit id, else "".
//
// Mixed case is refused along with the rest. The same commit spelled two ways
// compares unequal, and comparing is the entire purpose of this field — a
// live test asserting it is testing revision X against a control plane
// reporting X in capitals would fail for no reason a reader could see.
func fullCommitSHA(value string) string {
	value = strings.TrimSpace(value)
	if len(value) != 40 {
		return ""
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return ""
		}
	}
	return value
}
