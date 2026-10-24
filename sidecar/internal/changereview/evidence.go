package changereview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
)

var (
	evidenceKind = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	evidenceID   = regexp.MustCompile(`^[a-z][a-z0-9_]*:[!-~]+$`)
)

// ValidEvidenceID reports whether id is "kind:ref": a lower-case kind and
// a printable ASCII reference without spaces, at most 200 bytes.
func ValidEvidenceID(id string) bool {
	return len(id) <= maxEvidenceIDLen && evidenceID.MatchString(id)
}

// NewEvidence is evidence "kind:ref" over data, which is stored as
// canonical JSON (sorted keys, compact, numbers as written) and digested
// with the id and label. data may be any JSON-marshalable value or a
// json.RawMessage.
func NewEvidence(kind, ref, label string, data any) (Evidence, error) {
	if !evidenceKind.MatchString(kind) {
		return Evidence{}, fmt.Errorf("%w: evidence kind %q", ErrInvalid, kind)
	}
	id := kind + ":" + ref
	if !ValidEvidenceID(id) {
		return Evidence{}, fmt.Errorf("%w: evidence id %q", ErrInvalid, id)
	}
	canon, err := CanonicalJSON(data)
	if err != nil {
		return Evidence{}, fmt.Errorf("%w: evidence %s data: %v", ErrInvalid, id, err)
	}
	e := Evidence{ID: id, Kind: kind, Label: label, Data: canon}
	e.Digest = evidenceDigest(e)
	return e, nil
}

// Verify reports whether the digest still matches the id, label and data.
func (e Evidence) Verify() bool {
	canon, err := CanonicalJSON(e.Data)
	if err != nil {
		return false
	}
	e.Data = canon
	return e.Digest == evidenceDigest(e)
}

func evidenceDigest(e Evidence) string {
	h := sha256.New()
	h.Write([]byte(e.ID))
	h.Write([]byte{0})
	h.Write([]byte(e.Label))
	h.Write([]byte{0})
	h.Write(e.Data)
	return hex.EncodeToString(h.Sum(nil))
}

// CanonicalJSON renders v as compact JSON with object keys sorted and
// numbers kept exactly as written, so the same value hashes the same
// after a round trip through jsonb.
func CanonicalJSON(v any) (json.RawMessage, error) {
	raw, ok := v.(json.RawMessage)
	if !ok {
		var err error
		if raw, err = json.Marshal(v); err != nil {
			return nil, err
		}
	}
	if len(raw) == 0 {
		return json.RawMessage("null"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after the JSON value")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(tree); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// canonicalEvidence returns ev with every Data canonical (for storage).
func canonicalEvidence(ev []Evidence) ([]Evidence, error) {
	out := make([]Evidence, len(ev))
	for i, e := range ev {
		canon, err := CanonicalJSON(e.Data)
		if err != nil {
			return nil, fmt.Errorf("evidence %s: %w", e.ID, err)
		}
		e.Data = canon
		out[i] = e
	}
	return out, nil
}
