package actors

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"os"
)

// InboundCredentialVerifier is the deliberately simple verifier for a worker
// or bridge presenting authority to dial in.
//
// EXPIRY (#111): this simple record is acceptable only until the dial-in path
// accepts its first connection; that dial-in event replaces it with #111's
// per-actor authentication and authorization model.
//
// A verifier contains either a SHA-256 digest or the name of an environment
// variable. It never contains the presented value. Environment-backed values
// are read at use time, matching DBRegistry's outbound credential discipline.
type InboundCredentialVerifier struct {
	sha256Digest []byte
	envName      string
	lookupEnv    func(string) (string, bool)
}

// NewInboundCredentialVerifier constructs the Go representation of one
// inbound_authentication row. Exactly one material form must be populated.
func NewInboundCredentialVerifier(digest []byte, envName string, lookupEnv func(string) (string, bool)) (*InboundCredentialVerifier, error) {
	if (len(digest) == 0) == (envName == "") {
		return nil, errors.New("actors: inbound verifier requires exactly one of sha256 digest or environment name")
	}
	if len(digest) != 0 && len(digest) != sha256.Size {
		return nil, errors.New("actors: inbound verifier SHA-256 digest has the wrong width")
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	return &InboundCredentialVerifier{
		sha256Digest: append([]byte(nil), digest...),
		envName:      envName,
		lookupEnv:    lookupEnv,
	}, nil
}

// Verify reads any referenced environment value and compares the presented
// value in constant time. Both sides are hashed first so comparison time does
// not disclose either value's length; hmac.Equal follows TokenSigner's idiom.
func (v *InboundCredentialVerifier) Verify(presented string) bool {
	want := v.sha256Digest
	if v.envName != "" {
		value, ok := v.lookupEnv(v.envName)
		if !ok {
			return false
		}
		digest := sha256.Sum256([]byte(value))
		want = digest[:]
	}
	presentedDigest := sha256.Sum256([]byte(presented))
	return hmac.Equal(presentedDigest[:], want)
}
