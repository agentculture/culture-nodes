package decl

import "github.com/agentculture/culture-nodes/internal/contracts"

// CanonicalJSON is the normalized declaration, independent of input format.
// The exposes list is normalized here as well as in Parse, so a body built
// in code canonicalizes -- and digests -- exactly like a parsed one.
func (d *Declaration) CanonicalJSON() ([]byte, error) {
	c := *d
	c.Exposes = NormalizeExposes(d.Exposes)
	return contracts.CanonicalJSON(&c)
}

// Digest identifies a normalized declaration using the shared contract codec.
func (d *Declaration) Digest() (string, error) {
	canonical, err := d.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return contracts.Digest(canonical), nil
}
