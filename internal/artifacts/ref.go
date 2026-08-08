package artifacts

import (
	"fmt"
	"strings"
)

// Ref is an opaque artifact identifier of the form
// "artifact://<namespace-id>/<id>" -- never a filesystem path, and never
// meaningful on its own: it is resolved to content only by handing it to a
// Store. Callers construct one only via NewRef (drivers, at Put time) or
// receive one back from Put; ParseRef is for drivers and Router decoding a
// Ref they were handed, not for ordinary callers.
type Ref string

// refScheme is Ref's URI scheme, including the "://" separator.
const refScheme = "artifact://"

// DigestPrefix labels every digest this package records, mirroring
// internal/contracts.DigestPrefix's "sha256:<hex>" shape -- the same
// algorithm and formatting, applied to raw artifact bytes rather than
// canonical JSON, so an artifact digest is never mistaken for a workflow
// content digest even though both happen to look alike.
const DigestPrefix = "sha256:"

// NewRef builds the ref for a freshly stored artifact: namespaceID is the
// tenant/installation boundary (prd-spec §14) the artifact belongs to, and
// id is a fresh identifier (a store.NewULID(), by convention) minted by
// whichever driver is doing the Put.
func NewRef(namespaceID, id string) Ref {
	return Ref(refScheme + namespaceID + "/" + id)
}

// ParseRef splits ref back into the namespace ID and artifact id NewRef
// combined into it. It fails with ErrInvalidRef if ref does not have the
// "artifact://<namespace>/<id>" shape: missing scheme, missing separator,
// an empty segment, or an id segment that itself contains a "/".
func ParseRef(ref Ref) (namespaceID, id string, err error) {
	rest, ok := strings.CutPrefix(string(ref), refScheme)
	if !ok {
		return "", "", fmt.Errorf("%w: %q: missing %q scheme", ErrInvalidRef, ref, refScheme)
	}

	namespaceID, id, ok = strings.Cut(rest, "/")
	if !ok || namespaceID == "" || id == "" {
		return "", "", fmt.Errorf("%w: %q", ErrInvalidRef, ref)
	}
	if strings.Contains(id, "/") {
		return "", "", fmt.Errorf("%w: %q: id segment must not contain \"/\"", ErrInvalidRef, ref)
	}

	return namespaceID, id, nil
}
