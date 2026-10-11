// Package secretscan finds known leaked secrets in files while knowing only
// their SHA-256 hashes and lengths, so the check never embeds the plaintext.
//
// A file is split into tokens at whitespace and common delimiters (quotes,
// brackets, '=', ':', '@', '/', ...). Every window of a target's length
// inside a token is hashed and compared. A target must therefore contain
// none of those delimiters; the AU-10 targets are letters, digits and '!'.
package secretscan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Target is one secret, known by the byte length and hex SHA-256 of its
// plaintext.
type Target struct {
	Length int
	SHA256 string
}

// Hit is one occurrence of a target. It names the file, the 1-based line and
// the target's index, never the matched text.
type Hit struct {
	Path   string
	Line   int
	Target int
}

func (h Hit) String() string {
	return fmt.Sprintf("%s:%d (target %d)", h.Path, h.Line, h.Target)
}

// Matcher scans for a fixed set of targets. It keeps no per-scan state, so
// one Matcher serves concurrent scans.
type Matcher struct {
	byLength map[int]map[[sha256.Size]byte]int
	lengths  []int // ascending, for a deterministic hit order
}

// NewMatcher validates targets and indexes them by length.
func NewMatcher(targets []Target) (*Matcher, error) {
	if len(targets) == 0 {
		return nil, errors.New("secretscan: no targets")
	}
	m := &Matcher{byLength: map[int]map[[sha256.Size]byte]int{}}
	for i, target := range targets {
		if target.Length <= 0 {
			return nil, fmt.Errorf("secretscan: target %d: length %d must be positive",
				i, target.Length)
		}
		raw, err := hex.DecodeString(strings.ToLower(target.SHA256))
		if err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("secretscan: target %d: SHA256 must be %d hex bytes",
				i, sha256.Size)
		}
		var sum [sha256.Size]byte
		copy(sum[:], raw)
		if m.byLength[target.Length] == nil {
			m.byLength[target.Length] = map[[sha256.Size]byte]int{}
			m.lengths = append(m.lengths, target.Length)
		}
		m.byLength[target.Length][sum] = i
	}
	sort.Ints(m.lengths)
	return m, nil
}

// Scan reads r to the end and returns every target occurrence, in file order.
func (m *Matcher) Scan(path string, r io.Reader) ([]Hit, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("secretscan: reading %s: %w", path, err)
	}
	var hits []Hit
	line := 1
	for start := 0; start < len(data); {
		if data[start] == '\n' {
			line++
		}
		if isDelimiter(data[start]) {
			start++
			continue
		}
		end := start
		for end < len(data) && !isDelimiter(data[end]) {
			end++
		}
		for _, hit := range m.scanToken(data[start:end]) {
			hits = append(hits, Hit{Path: path, Line: line, Target: hit})
		}
		start = end
	}
	return hits, nil
}

// scanToken returns the targets found in one delimiter-free token.
func (m *Matcher) scanToken(token []byte) []int {
	var found []int
	for offset := 0; offset+m.lengths[0] <= len(token); offset++ {
		for _, length := range m.lengths {
			if offset+length > len(token) {
				break
			}
			sums := m.byLength[length]
			if i, ok := sums[sha256.Sum256(token[offset:offset+length])]; ok {
				found = append(found, i)
			}
		}
	}
	return found
}

// delimiters end a token. Whitespace is handled separately.
var delimiters = []byte("\"'`<>()[]{},;=:@/\\|")

func isDelimiter(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == 0 ||
		bytes.IndexByte(delimiters, b) >= 0
}
