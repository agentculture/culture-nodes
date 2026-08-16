package testslint

// The reverse transport is one module, not five copies (task t18).
//
// `dialin.py` is the third file in this repo that every bridge carries
// verbatim, alongside preflight.py/deployment.py (preflightsurface_test.go)
// and reap.py/reclaim.py (workspacereaper_test.go) -- and until t18 it was
// the only one of the three that NOTHING checked. CLAUDE.md already told
// readers that tests/lint enforced byte-identity for the shared bridge
// modules "preflight.py, dialin.py, deployment.py, and the workspace
// reaper"; three of those four were true.
//
// That gap mattered on this cycle's schedule specifically: a later task
// rewrites this exact file in all five bridges at once. A rewrite applied
// four times and fumbled the fifth is the divergence this package exists to
// refuse -- and the fifth bridge here is human-inbox, which ships NO
// capability surface, so it is invisible to the guard next door. It is the
// copy most likely to be forgotten, which is precisely why the check below
// asks every discovered package rather than a list of the ones that
// advertise.
//
// Byte-identity is the only property asserted here. Whether the transport
// stays stdlib-only is the other half of t18 and is asserted where Python's
// own parser can be used on it -- an AST walk, not a text match, because
// these modules are full of prose that a grep would read as an import:
// tests/test_adapter_zero_dependencies.py.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// dialInModule is the reverse transport every bridge dials home with.
const dialInModule = "dialin.py"

// TestEveryBridgeShipsTheSameDialInTransport is the guard.
//
// It asserts two things in one pass, because either alone passes over a
// broken repo: every bridge HAS the module (a bridge that lost it cannot be
// reached at all on a host with no inbound route), and every copy is the
// SAME BYTES.
func TestEveryBridgeShipsTheSameDialInTransport(t *testing.T) {
	packages := discoverAdapterPackages(t)

	var missing []string
	digests := map[string][]string{}
	for _, pkg := range packages {
		if !pkg.has(t, dialInModule) {
			missing = append(missing, pkg.adapter)
			continue
		}
		sum := sha256.Sum256([]byte(pkg.read(t, dialInModule)))
		digest := hex.EncodeToString(sum[:])
		digests[digest] = append(digests[digest], pkg.adapter)
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("adapters/%s ship no %s: a bridge without the reverse transport is reachable "+
			"only from a control plane that can open a connection TO it, which is the "+
			"arrangement dial-in exists to stop depending on",
			strings.Join(missing, ", "), dialInModule)
	}
	if len(digests) == 0 {
		t.Fatalf("no adapter ships %s; the dial-in transport is gone", dialInModule)
	}
	if len(digests) > 1 {
		t.Fatalf("%s has diverged between bridges -- it must be byte-identical everywhere. "+
			"Change one copy and copy it to the rest verbatim; do NOT run a formatter "+
			"per-adapter (isort is configured in three adapters and not the other two, so "+
			"that alone gives one file two formattings).\n%s",
			dialInModule, describeDigests(digests))
	}
}

// describeDigests renders a digest -> adapters map as sorted, readable lines,
// so a failure names which bridges agree with which.
func describeDigests(digests map[string][]string) string {
	var lines []string
	for digest, adapters := range digests {
		sort.Strings(adapters)
		lines = append(lines, fmt.Sprintf("  %s: %s", digest[:12], strings.Join(adapters, ", ")))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
