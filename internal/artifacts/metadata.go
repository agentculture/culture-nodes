package artifacts

import "encoding/json"

// extraMetadata carries the ArtifactMeta fields that do not have a
// dedicated column on the artifacts table
// (migrations/0004_observability.sql) -- currently just Name -- inside that
// table's `metadata JSONB` column.
type extraMetadata struct {
	Name string `json:"name,omitempty"`
}

// EncodeExtraMetadata renders name into the JSON object drivers pass as
// internal/store/postgres.InsertArtifactInput.Metadata. Both driver
// subpackages (internal/artifacts/postgres, internal/artifacts/s3) call
// this at Put time so the encoding lives in exactly one place.
func EncodeExtraMetadata(name string) json.RawMessage {
	b, err := json.Marshal(extraMetadata{Name: name})
	if err != nil {
		// extraMetadata is a single plain string field; Marshal cannot
		// fail on it in practice, but a zero-value fallback keeps this
		// function infallible for callers rather than a func() ([]byte, error).
		return json.RawMessage(`{}`)
	}
	return b
}

// DecodeExtraMetadataName recovers Name from a metadata column value
// produced by EncodeExtraMetadata (or from "{}"/nil/malformed JSON, for
// which it returns "" rather than erroring -- Name is descriptive-only, not
// load-bearing for correctness).
func DecodeExtraMetadataName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v extraMetadata
	_ = json.Unmarshal(raw, &v)
	return v.Name
}
