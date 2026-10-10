package broker

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Request bounds.
const (
	MaxSQLBytes = 100000
	MaxParams   = 100
	maxParamLen = 10000
)

// validateRequest checks a call's arguments and renders its parameters as
// text; it returns the row bound that applies.
func validateRequest(cfg Config, req Request) (int, [][]byte, error) {
	switch {
	case strings.TrimSpace(req.Database) == "":
		return 0, nil, invalidf("database is required")
	case strings.TrimSpace(req.SQL) == "":
		return 0, nil, invalidf("sql is required")
	case len(req.SQL) > MaxSQLBytes:
		return 0, nil, invalidf("sql is longer than %d bytes", MaxSQLBytes)
	case req.MaxRows < 0 || req.MaxRows > cfg.MaxRowsCeiling:
		return 0, nil, invalidf("max_rows must be 1-%d", cfg.MaxRowsCeiling)
	case len(req.Params) > MaxParams:
		return 0, nil, invalidf("at most %d params", MaxParams)
	}
	rows := req.MaxRows
	if rows == 0 {
		rows = cfg.MaxRows
	}
	params := make([][]byte, len(req.Params))
	for i, p := range req.Params {
		text, ok := paramText(p)
		if !ok {
			return 0, nil, invalidf("param %d must be a string, number, boolean or null",
				i+1)
		}
		if len(text) > maxParamLen {
			return 0, nil, invalidf("param %d is longer than %d bytes", i+1, maxParamLen)
		}
		params[i] = text
	}
	return rows, params, nil
}

// paramText renders a JSON scalar as the text the server parses; nil is
// SQL NULL.
func paramText(p any) ([]byte, bool) {
	switch v := p.(type) {
	case nil:
		return nil, true
	case string:
		return []byte(v), true
	case json.Number:
		return []byte(v.String()), true
	case float64:
		return []byte(strconv.FormatFloat(v, 'g', -1, 64)), true
	case int:
		return []byte(strconv.Itoa(v)), true
	case int64:
		return []byte(strconv.FormatInt(v, 10)), true
	case bool:
		return []byte(strconv.FormatBool(v)), true
	}
	return nil, false
}

// forbiddenRune finds the first code point rejected before parsing (§6.8):
// format (Cf), private use (Co) and surrogate (Cs) characters, the TAG
// block, bidi controls and zero-width characters. Invalid UTF-8 cannot be
// screened, so it is rejected too (reported as U+FFFD).
func forbiddenRune(sql string) (rune, int, bool) {
	for i, r := range sql {
		if r == utf8.RuneError {
			if _, size := utf8.DecodeRuneInString(sql[i:]); size <= 1 {
				return r, i, true
			}
		}
		if forbidden(r) {
			return r, i, true
		}
	}
	return 0, 0, false
}

func forbidden(r rune) bool {
	switch {
	case r >= 0xE0000 && r <= 0xE007F, r >= 0x202A && r <= 0x202E,
		r >= 0x2066 && r <= 0x2069, r >= 0x200B && r <= 0x200D, r == 0xFEFF:
		return true
	}
	return unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs)
}

// envelopeHash binds a call: principal, database id, canonical statement
// and parameters, each length-prefixed so field boundaries are
// unambiguous; a NULL parameter differs from an empty one.
func envelopeHash(principalID, databaseID, canonical string, params [][]byte) string {
	h := sha256.New()
	field := func(b []byte, null bool) {
		var n [9]byte
		if null {
			n[0] = 1
		}
		binary.BigEndian.PutUint64(n[1:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	field([]byte(principalID), false)
	field([]byte(databaseID), false)
	field([]byte(canonical), false)
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(params)))
	h.Write(count[:])
	for _, p := range params {
		field(p, p == nil)
	}
	return hex.EncodeToString(h.Sum(nil))
}
