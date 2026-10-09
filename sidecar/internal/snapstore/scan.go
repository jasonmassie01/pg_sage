package snapstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// A catalog document is an array of flat objects. Once the document is
// validated, its elements and their members are slices of its text: a
// scanner finds their ends instead of decoding through reflection (the
// sidecar's largest CPU cost at 20,000 relations, nightly perf gate). The
// slices are never modified. scan_test.go holds it to encoding/json.

// splitArray validates data and returns its array elements. Malformed JSON
// is an error; valid JSON that is not an array is errNotEncodable.
func splitArray(data []byte) ([]json.RawMessage, error) {
	if !json.Valid(data) {
		return nil, errors.New("parse snapshot document: malformed JSON")
	}
	i := skipSpace(data, 0)
	if data[i] != '[' {
		return nil, fmt.Errorf("%w: not an array", errNotEncodable)
	}
	var out []json.RawMessage
	for i = skipSpace(data, i+1); data[i] != ']'; {
		end := skipValue(data, i)
		out = append(out, json.RawMessage(data[i:end:end]))
		if i = skipSpace(data, end); data[i] == ',' {
			i = skipSpace(data, i+1)
		}
	}
	return out, nil
}

// scanObject returns the members of a valid JSON object text, ok false
// for anything it leaves to encoding/json: not an object, or a key with an
// escape or invalid UTF-8 (encoding/json rewrites those). A repeated key
// keeps its last value, as encoding/json does. sizeHint presizes the map
// (a document's elements share a shape: the previous element's size).
func scanObject(text []byte, sizeHint int) (map[string]json.RawMessage, bool) {
	if len(text) == 0 || text[0] != '{' {
		return nil, false
	}
	m := make(map[string]json.RawMessage, sizeHint)
	for i := skipSpace(text, 1); text[i] != '}'; {
		end := skipString(text, i)
		key := text[i+1 : end-1]
		if bytes.IndexByte(key, '\\') >= 0 || !utf8.Valid(key) {
			return nil, false
		}
		i = skipSpace(text, skipSpace(text, end)+1) // past the colon
		vend := skipValue(text, i)
		m[string(key)] = json.RawMessage(text[i:vend:vend])
		if i = skipSpace(text, vend); text[i] == ',' {
			i = skipSpace(text, i+1)
		}
	}
	return m, true
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// skipString returns the index after the string starting at b[i].
func skipString(b []byte, i int) int {
	for i++; ; i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
}

// skipValue returns the index after the value starting at b[i].
func skipValue(b []byte, i int) int {
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for {
			switch b[i] {
			case '"':
				i = skipString(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			i++
			if depth == 0 {
				return i
			}
		}
	}
	for i < len(b) && !bytes.ContainsRune([]byte(",]} \t\n\r"), rune(b[i])) {
		i++
	}
	return i
}
