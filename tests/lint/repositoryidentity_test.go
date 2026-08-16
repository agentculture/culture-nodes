package testslint

// Repository identity -> checkout is ONE rule, not three copies (task t2,
// issue #125).
//
// The control plane sends a NAME on every dispatch to an actor whose
// registration declares one (internal/actors.RepositoryIdentityKey, task
// t1). Turning that name into a directory is something only the actor's own
// host can do, so it lands in the adapters -- and the moment a rule lands in
// the adapters, the repo's own history says what happens next:
// `resolve_actor_row_id` shipped as the same bug in three deploy lanes
// because each lane inlined its own copy. `preflight.py`, `deployment.py`
// and `dialin.py` are all shared for that reason, each with a guard in this
// package. `repositories.py` joins them here.
//
// Four guards, because the interesting failure is never the one a single
// check would catch:
//
//  1. `repositories.py` is byte-identical in every bridge that has it.
//  2. A bridge has it if and only if it has a `repo_allowlist` -- DISCOVERED
//     from each config.py rather than listed here, so this stays a property
//     rather than a roster somebody has to remember to edit.
//  3. The Python input key equals the Go one, across the language boundary.
//  4. Each checkout bridge actually WIRES it: server.py resolves through it
//     and excludes the key from the Bound-inputs block.
//
// Guard 4's second half is the one found by inspection during t1 and easy to
// lose again. `_transport_keys` is what keeps engine-resolved addressing out
// of the prompt; a repository identity left out of that set is appended to
// the agent's instruction as a "Bound input", which is prose a model would
// be right to read as an instruction about a repository.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// repositoryModule is the shared identity->checkout resolver.
const repositoryModule = "repositories.py"

// allowlistField is the config field that makes a bridge a CHECKOUT bridge:
// one that runs a session inside a directory it is permitted to touch.
// adapters/notify posts a webhook and adapters/human-inbox dispatches to a
// person; neither declares it, and neither has a checkout for an identity to
// name.
const allowlistField = "repo_allowlist: tuple[str, ...]"

// identitiesField is the operator's declaration map -- the deterministic half
// of resolution, for a host whose checkout directories are not named after
// the repository they hold.
const identitiesField = "repo_identities: dict[str, str]"

// TestEveryCheckoutBridgeShipsTheSameRepositoryIdentityResolver is guard 1.
func TestEveryCheckoutBridgeShipsTheSameRepositoryIdentityResolver(t *testing.T) {
	digests := map[string][]string{}
	for _, pkg := range discoverAdapterPackages(t) {
		if !pkg.has(t, repositoryModule) {
			continue
		}
		sum := sha256.Sum256([]byte(pkg.read(t, repositoryModule)))
		digests[hex.EncodeToString(sum[:])] = append(digests[hex.EncodeToString(sum[:])], pkg.adapter)
	}

	if len(digests) == 0 {
		t.Fatalf("no adapter ships %s; the repository-identity resolver is gone, and every "+
			"triggered dispatch to a multi-repository bridge is back to failing closed on "+
			"`input.repo is required` (issue #125)", repositoryModule)
	}
	if len(digests) > 1 {
		t.Fatalf("%s has diverged between bridges -- it must be byte-identical everywhere. "+
			"Change one copy and copy it to the rest verbatim; do NOT run a formatter "+
			"per-adapter (isort is configured in three adapters and not the other two, so "+
			"that alone gives one file two formattings).\n%s",
			repositoryModule, describeDigests(digests))
	}
}

// TestABridgeResolvesIdentitiesIfAndOnlyIfItHasAnAllowlist is guard 2.
//
// Both directions matter. A bridge with an allowlist and no resolver is the
// all-backends rule broken: a feature on some backends and not others is a
// bug, not a rollout. A bridge with a resolver and no allowlist is dead code
// carrying a name that suggests it does something.
func TestABridgeResolvesIdentitiesIfAndOnlyIfItHasAnAllowlist(t *testing.T) {
	var checkoutBridges []string
	for _, pkg := range discoverAdapterPackages(t) {
		config := pkg.read(t, "config.py")
		hasAllowlist := strings.Contains(config, allowlistField)
		hasResolver := pkg.has(t, repositoryModule)

		switch {
		case hasAllowlist && !hasResolver:
			t.Errorf("adapters/%s has a %s but no %s: it runs sessions in a checkout, so a "+
				"dispatch carrying only a repository identity has nowhere to resolve it and "+
				"falls back to inferring the repository from the allowlist holding exactly "+
				"one entry -- the inference issue #125 exists to retire",
				pkg.adapter, allowlistField, repositoryModule)
		case hasResolver && !hasAllowlist:
			t.Errorf("adapters/%s ships %s but declares no %s: there is no permitted surface "+
				"for an identity to resolve against, so the module can only ever refuse",
				pkg.adapter, repositoryModule, allowlistField)
		case hasAllowlist:
			checkoutBridges = append(checkoutBridges, pkg.adapter)
			if !strings.Contains(config, identitiesField) {
				t.Errorf("adapters/%s declares no `%s`: inference alone cannot serve a host "+
					"whose checkout directories are not named after the repository they hold, "+
					"and that host is spark", pkg.adapter, identitiesField)
			}
		}
	}

	if len(checkoutBridges) < 3 {
		sort.Strings(checkoutBridges)
		t.Fatalf("only %d checkout bridges discovered (%s); three ship today, so the scan is "+
			"looking for the wrong thing rather than reporting a real shrinkage",
			len(checkoutBridges), strings.Join(checkoutBridges, ", "))
	}
}

// TestThePythonAndGoHalvesAgreeOnTheIdentityKey is guard 3.
//
// The two halves are written in different languages and reviewed at
// different times. A disagreement here is silent in the worst way: the
// engine sets the key, the bridge reads a different one, no error is raised
// anywhere, and the dispatch simply falls back to the cardinality inference
// as if the actor had declared nothing.
func TestThePythonAndGoHalvesAgreeOnTheIdentityKey(t *testing.T) {
	var source string
	for _, pkg := range discoverAdapterPackages(t) {
		if pkg.has(t, repositoryModule) {
			source = pkg.read(t, repositoryModule)
			break
		}
	}
	if source == "" {
		t.Fatalf("no adapter ships %s", repositoryModule)
	}

	if got := pythonConstant(t, source, "INPUT_KEY"); got != actors.RepositoryIdentityKey {
		t.Errorf("the bridges read input.%q while internal/actors sends input.%q; nothing "+
			"reports this mismatch at runtime -- the dispatch just behaves as if the actor "+
			"declared no repository identity", got, actors.RepositoryIdentityKey)
	}
}

// TestEveryCheckoutBridgeWiresTheResolverAndTreatsTheKeyAsTransport is guard 4.
func TestEveryCheckoutBridgeWiresTheResolverAndTreatsTheKeyAsTransport(t *testing.T) {
	for _, pkg := range discoverAdapterPackages(t) {
		if !pkg.has(t, repositoryModule) {
			continue
		}
		t.Run(pkg.adapter, func(t *testing.T) {
			server := pkg.read(t, "server.py")
			requireContains(t, server, "repositories.resolve_for_input",
				fmt.Sprintf("adapters/%s ships %s but never calls it: a bridge that carries "+
					"the resolver and does not resolve is the half-done state this guard "+
					"exists to refuse", pkg.adapter, repositoryModule))

			transport := transportKeysBlock(t, server, pkg.adapter)
			if !strings.Contains(transport, "repositories.INPUT_KEY") {
				t.Errorf("adapters/%s does not list repositories.INPUT_KEY in its "+
					"_transport_keys set: the identity is an ADDRESSING field, and left out "+
					"of that set it is appended to the agent's instruction as an "+
					"engine-resolved \"Bound input\" -- prose a model would be right to read "+
					"as an instruction about a repository.\n%s", pkg.adapter, transport)
			}
		})
	}
}

// transportKeysBlock returns the `_transport_keys = { ... }` literal, so
// guard 4 asserts membership of that set rather than the mere presence of a
// symbol anywhere in the file.
func transportKeysBlock(t *testing.T, server, adapter string) string {
	t.Helper()
	const opening = "_transport_keys = {"
	start := strings.Index(server, opening)
	if start < 0 {
		t.Fatalf("adapters/%s has no %s literal: this guard reads that set, and a bridge "+
			"that stopped keeping one has stopped separating transport from content",
			adapter, opening)
	}
	rest := server[start:]
	end := strings.Index(rest, "\n        }")
	if end < 0 {
		t.Fatalf("adapters/%s's %s literal is not closed where this guard expects "+
			"(a line of eight spaces and `}`); the shape changed and so must this scan",
			adapter, opening)
	}
	return rest[:end]
}
