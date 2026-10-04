package agenttools

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds of parsed sqlcommenter tags.
const maxTagValue = 256

var tagKeyPattern = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)

// ParseSQLCommenter returns the sqlcommenter tags of sql
// (https://google.github.io/sqlcommenter/): block comments made only of
// key='value' pairs separated by commas, URL-encoded, with \' escaping a
// quote. Several such comments merge and a later key wins. A comment that
// is not entirely such pairs adds nothing; comment-like text inside a
// literal or a -- line comment is not a comment. Keys outside
// [a-z0-9_.-]{1,64} are dropped and values are cut to 256 bytes. The map
// is never nil.
func ParseSQLCommenter(sql string) map[string]string {
	tags := map[string]string{}
	// An unterminated token only ends the scan: the comments closed before
	// it are whole, and an unterminated comment is never collected.
	scan, _ := scanSQL(sql)
	for _, body := range scan.comments {
		pairs, ok := parseCommenterBody(body)
		if !ok {
			continue
		}
		for k, v := range pairs {
			tags[k] = v
		}
	}
	return tags
}

// parseCommenterBody parses key='value'(,key='value')*; ok is false when
// the body is anything else.
func parseCommenterBody(body string) (map[string]string, bool) {
	s := strings.TrimSpace(body)
	if s == "" {
		return nil, false
	}
	out := map[string]string{}
	for {
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			return nil, false
		}
		rawKey := s[:eq]
		value, rest, ok := quotedTagValue(s[eq+1:])
		if !ok {
			return nil, false
		}
		if key, ok := tagKey(rawKey); ok {
			if v, ok := tagValue(value); ok {
				out[key] = v
			}
		}
		if rest == "" {
			return out, true
		}
		if rest[0] != ',' {
			return nil, false
		}
		s = rest[1:]
	}
}

// quotedTagValue reads '...' at the start of s, \x escaping x.
func quotedTagValue(s string) (string, string, bool) {
	if s == "" || s[0] != '\'' {
		return "", "", false
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 == len(s) {
				return "", "", false
			}
			i++
			b.WriteByte(s[i])
		case '\'':
			return b.String(), s[i+1:], true
		default:
			b.WriteByte(s[i])
		}
	}
	return "", "", false
}

func tagKey(raw string) (string, bool) {
	key, err := url.PathUnescape(raw)
	if err != nil || !tagKeyPattern.MatchString(key) {
		return "", false
	}
	return key, true
}

// tagValue URL-decodes a value, replaces control characters and cuts it
// to maxTagValue bytes on a rune boundary.
func tagValue(raw string) (string, bool) {
	v, err := url.PathUnescape(raw)
	if err != nil || !utf8.ValidString(v) {
		return "", false
	}
	v = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, v)
	return clipBytes(v, maxTagValue), true
}

// clipBytes cuts s to at most n bytes without splitting a UTF-8 sequence.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
