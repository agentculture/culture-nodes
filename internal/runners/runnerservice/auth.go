package runnerservice

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// Caller authentication, which the runner protocol makes mandatory on every
// request — execute, status, and cancel alike — with no loopback exemption.
//
// The exemption is the part worth being explicit about. A local exemption is
// the one every remote deployment inherits by copy-paste: a service that
// trusts 127.0.0.1 today is a service that trusts a port-forward, a sidecar,
// or an SSRF tomorrow. So there is no branch here on the peer address at all.

// authenticator holds the deployment's bearer secret as a digest and compares
// presented credentials against it in constant time.
//
// The comparison is over SHA-256 digests rather than the raw strings, and
// that is deliberate: subtle.ConstantTimeCompare is constant-time only for
// equal-length inputs and returns early on a length mismatch, so comparing
// raw secrets would leak the secret's length. Digesting first makes every
// comparison a fixed 32 bytes, so neither the length nor a shared prefix is
// observable from timing.
type authenticator struct {
	digest [sha256.Size]byte
}

// newAuthenticator builds an authenticator for a non-empty secret.
func newAuthenticator(secret string) (*authenticator, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("runnerservice: a bearer secret is required; a runner service accepting operations " +
			"over the network is a remote-code-execution surface, and an unauthenticated one executes code for " +
			"anyone who can reach it")
	}
	return &authenticator{digest: sha256.Sum256([]byte(secret))}, nil
}

// authenticate reports whether a request carries the deployment's secret.
func (a *authenticator) authenticate(r *http.Request) bool {
	token, ok := bearerToken(r.Header.Get(runners.AuthorizationHeader))
	if !ok {
		return false
	}
	presented := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(presented[:], a.digest[:]) == 1
}

// bearerToken extracts the credential from an Authorization header value.
//
// The scheme is matched case-insensitively (RFC 7235 says schemes are), and
// anything that is not a bearer credential — a Basic header, a bare token, an
// empty one — is reported as absent rather than passed on as a candidate the
// comparison would then reject. "Cannot parse the credential" and "the
// credential is wrong" are the same answer to the caller (401) but not the
// same event in a log.
func bearerToken(header string) (string, bool) {
	const scheme = "bearer"
	trimmed := strings.TrimSpace(header)
	if len(trimmed) <= len(scheme) || !strings.EqualFold(trimmed[:len(scheme)], scheme) {
		return "", false
	}
	rest := trimmed[len(scheme):]
	if rest[0] != ' ' && rest[0] != '\t' {
		return "", false
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", false
	}
	return token, true
}
