package contracts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// DigestPrefix is the algorithm label every digest this package issues carries.
// It is part of the stored value so a future algorithm change is visible in the
// data rather than inferred from length.
const DigestPrefix = "sha256:"

// CanonicalJSON returns the canonical JSON encoding of v: object keys sorted
// lexicographically, no insignificant whitespace, UTF-8 strings, and number
// literals left exactly as encoding/json produced them. The rules are spelled
// out in the package comment; they are stable API because digests depend on
// them.
//
// v may be any value encoding/json can marshal, including a json.RawMessage
// carrying a document read from disk or off the wire.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := marshalWithoutHTMLEscaping(v)
	if err != nil {
		return nil, fmt.Errorf("contracts: marshal value: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("contracts: decode value: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("contracts: expected a single JSON value, found trailing content")
	}

	w := newCanonicalWriter()
	if err := w.writeValue(tree); err != nil {
		return nil, err
	}
	return w.out.Bytes(), nil
}

// Digest returns the content identifier of already-canonical bytes, formatted
// as "sha256:<hex>". Callers that hold a value rather than its canonical form
// should use DigestValue, which cannot forget the canonicalization step.
func Digest(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return DigestPrefix + hex.EncodeToString(sum[:])
}

// DigestValue canonicalizes v and returns the digest of the result.
func DigestValue(v any) (string, error) {
	canonical, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	return Digest(canonical), nil
}

// marshalWithoutHTMLEscaping is json.Marshal with escapeHTML disabled, so a
// '<' in a description stays a '<' rather than becoming a six-character
// unicode escape. Encode appends a newline, which is not part of the value.
func marshalWithoutHTMLEscaping(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// canonicalWriter emits the canonical form of a decoded JSON tree. It keeps one
// scratch encoder so string escaping stays encoding/json's, without allocating
// an encoder per string.
type canonicalWriter struct {
	out     bytes.Buffer
	scratch bytes.Buffer
	enc     *json.Encoder
}

func newCanonicalWriter() *canonicalWriter {
	w := &canonicalWriter{}
	w.enc = json.NewEncoder(&w.scratch)
	w.enc.SetEscapeHTML(false)
	return w
}

func (w *canonicalWriter) writeValue(v any) error {
	switch value := v.(type) {
	case nil:
		w.out.WriteString("null")
	case bool:
		if value {
			w.out.WriteString("true")
		} else {
			w.out.WriteString("false")
		}
	case json.Number:
		// The decoder validated the literal; emit it unchanged.
		w.out.WriteString(value.String())
	case string:
		return w.writeString(value)
	case []any:
		return w.writeArray(value)
	case map[string]any:
		return w.writeObject(value)
	default:
		// Unreachable for trees produced by a json.Decoder with UseNumber.
		return fmt.Errorf("contracts: unexpected value of type %T in decoded JSON", v)
	}
	return nil
}

func (w *canonicalWriter) writeArray(values []any) error {
	w.out.WriteByte('[')
	for i, item := range values {
		if i > 0 {
			w.out.WriteByte(',')
		}
		if err := w.writeValue(item); err != nil {
			return err
		}
	}
	w.out.WriteByte(']')
	return nil
}

func (w *canonicalWriter) writeObject(members map[string]any) error {
	keys := make([]string, 0, len(members))
	for key := range members {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	w.out.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			w.out.WriteByte(',')
		}
		if err := w.writeString(key); err != nil {
			return err
		}
		w.out.WriteByte(':')
		if err := w.writeValue(members[key]); err != nil {
			return err
		}
	}
	w.out.WriteByte('}')
	return nil
}

func (w *canonicalWriter) writeString(s string) error {
	w.scratch.Reset()
	if err := w.enc.Encode(s); err != nil {
		return fmt.Errorf("contracts: encode string: %w", err)
	}
	w.out.Write(bytes.TrimSuffix(w.scratch.Bytes(), []byte("\n")))
	return nil
}
