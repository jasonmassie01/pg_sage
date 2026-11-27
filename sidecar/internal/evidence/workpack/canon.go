package workpack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// canonical encodes v as canonical JSON: object keys sorted, numbers with
// a fraction or exponent as strconv.FormatFloat(v, 'g', -1, 64), integers
// as written, no insignificant space, one trailing newline. The same value
// always gives the same bytes.
func canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("workpack: encode a section: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("workpack: encode a section: %w", err)
	}
	var b bytes.Buffer
	if err := writeCanonical(&b, tree); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case json.Number:
		return writeNumber(b, x)
	case string:
		writeString(b, x)
	default: // bool, nil
		raw, _ := json.Marshal(x)
		b.Write(raw)
	}
	return nil
}

func writeString(b *bytes.Buffer, s string) {
	raw, _ := json.Marshal(s)
	b.Write(raw)
}

func writeNumber(b *bytes.Buffer, n json.Number) error {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		b.WriteString(s)
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("workpack: encode number %s: %w", s, err)
	}
	b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	return nil
}

// contentSHA256 is the SHA-256 of the sorted "name sha256" lines of the
// files (name to the hex SHA-256 of its bytes).
func contentSHA256(sums map[string]string) string {
	names := make([]string, 0, len(sums))
	for n := range sums {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		_, _ = fmt.Fprintf(h, "%s %s\n", n, sums[n])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// round12 keeps 12 significant digits, so a sum's last-bit noise never
// reaches the content.
func round12(v float64) float64 {
	r, err := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 12, 64), 64)
	if err != nil {
		return v
	}
	return r
}
