// Package deploytest (see codexworkerenv_test.go's doc comment for the
// package's purpose).
//
// This file closes the gap task t12's credential audit surfaced on its first
// real run: orin's prod.env carries no NODES_ACTOR_CLAUDE_TOKEN, while
// compose.orin.yml declares the variable for its worker. The cause is one
// call site -- install-secrets.sh relays the externally-issued claude actor
// token to $THOR only, and never to $ORIN.
//
// It is a real defect rather than a deliberate asymmetry, and the script's
// own neighbouring lanes are the evidence: the codex bridge tokens go to
// BOTH hosts through update_actor_token_line, precisely because either
// worker may dispatch either actor and so both need the credential. The
// claude token is the same shape of fact and was the outlier.
//
// The check is written against the compose files rather than against a
// hard-coded pair of host names, so a third host added tomorrow is covered
// by construction: whichever compose file declares the variable, its host's
// lane must install it.
package deploytest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readProdFile reads one file out of deploy/prod. prodComposeDir resolves
// that directory from its CALLER's path via runtime.Caller(1), which is why
// this wrapper lives in tests/deploy alongside it.
func readProdFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(prodComposeDir(t), name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// composeHostsDeclaringClaudeToken returns the install-secrets.sh shell
// variable ($THOR / $ORIN) for every prod compose file whose service
// environment declares NODES_ACTOR_CLAUDE_TOKEN.
//
// The mapping from compose file to host variable is the one thing that
// cannot be derived -- compose files do not name the shell variable the
// install script uses for their host -- so it is stated here, in one place,
// and a compose file this map does not cover fails the test rather than
// being skipped.
func composeHostsDeclaringClaudeToken(t *testing.T) []string {
	t.Helper()
	byFile := map[string]string{
		"compose.thor.yml": "THOR",
		"compose.orin.yml": "ORIN",
	}
	var hosts []string
	for file, hostVar := range byFile {
		raw := readProdFile(t, file)
		if strings.Contains(raw, "NODES_ACTOR_CLAUDE_TOKEN:") {
			hosts = append(hosts, hostVar)
		}
	}
	if len(hosts) == 0 {
		t.Fatal("no prod compose file declares NODES_ACTOR_CLAUDE_TOKEN; this test is asserting nothing")
	}
	return hosts
}

// TestTheClaudeActorTokenIsInstalledOnEveryHostWhoseComposeDeclaresIt is the
// regression test for the defect the credential audit found live: a worker
// whose compose declares the token but whose prod.env never receives it
// answers 401 policy_denied on every claude node it is dispatched, and
// nothing in the deploy says so.
func TestTheClaudeActorTokenIsInstalledOnEveryHostWhoseComposeDeclaresIt(t *testing.T) {
	script := readInstallSecrets(t)
	calls := regexp.MustCompile(`install_claude_actor_token\s+"\$([A-Z_]+)"`).FindAllStringSubmatch(script, -1)

	installed := map[string]bool{}
	for _, m := range calls {
		installed[m[1]] = true
	}
	if len(installed) == 0 {
		t.Fatal("install-secrets.sh never calls install_claude_actor_token; the relay lane is gone entirely")
	}

	for _, hostVar := range composeHostsDeclaringClaudeToken(t) {
		if !installed[hostVar] {
			t.Errorf("compose declares NODES_ACTOR_CLAUDE_TOKEN for $%s, but install-secrets.sh "+
				"never calls install_claude_actor_token \"$%s\" — that host's worker will answer "+
				"401 policy_denied on every claude node dispatched to it, and the deploy will not "+
				"mention it. The codex token lanes already install to both hosts for this reason.",
				hostVar, hostVar)
		}
	}
}
