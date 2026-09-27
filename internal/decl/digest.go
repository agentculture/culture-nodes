package decl

import "github.com/agentculture/culture-nodes/internal/contracts"

// CanonicalJSON is the normalized declaration, independent of input format.
func (d *Declaration) CanonicalJSON() ([]byte, error) {
	return contracts.CanonicalJSON(d)
}

// Digest identifies a normalized declaration using the shared contract codec.
func (d *Declaration) Digest() (string, error) {
	canonical, err := d.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return contracts.Digest(canonical), nil
}
