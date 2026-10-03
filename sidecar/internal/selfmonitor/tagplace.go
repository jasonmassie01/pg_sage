package selfmonitor

import (
	"regexp"
	"strings"
)

// StatementTag marks every statement pg_sage sends. pg_stat_statements
// keeps it in the statement text, so a DBA can see (and price) pg_sage's
// own work, and pg_sage's own readers leave those statements out of the
// workload they analyze. It sits right after the statement's first
// keyword: PostgreSQL 18's pg_stat_statements drops comments before the
// first token.
const StatementTag = "/* " + ApplicationName + " */"

var tagPattern = regexp.MustCompile(`/\*\s*` + ApplicationName)

// IsTagged reports whether sql carries a pg_sage tag comment anywhere
// (before or after its first keyword).
func IsTagged(sql string) bool {
	return tagPattern.MatchString(sql)
}

// placeTag returns sql with a pg_sage tag right after its first keyword.
// A leading pg_sage comment (it may name a component) is moved there; a
// tag already there is left alone. A statement that does not start with
// a keyword (a parenthesized query) gets the tag in front; text that
// cannot be read (an unterminated comment, white space only) is returned
// unchanged.
func placeTag(sql string) string {
	i, tag, tagStart, tagEnd, ok := scanLeading(sql)
	if !ok || i == len(sql) {
		return sql
	}
	word := keywordEnd(sql, i)
	if word == i {
		if tag != "" {
			return sql
		}
		return StatementTag + " " + sql
	}
	rest := sql[word:]
	if strings.HasPrefix(strings.TrimLeft(rest, " \t\r\n"), "/*") &&
		IsTagged(firstComment(strings.TrimLeft(rest, " \t\r\n"))) {
		return sql
	}
	if tag == "" {
		tag = StatementTag
	}
	lead := sql[:i]
	if tagEnd > 0 {
		lead = sql[:tagStart] + sql[tagEnd:i]
	}
	sep := ""
	if rest != "" && !strings.ContainsAny(rest[:1], " \t\r\n") {
		sep = " "
	}
	return lead + sql[i:word] + " " + tag + sep + rest
}

// scanLeading skips white space and comments before the first token. It
// returns the token's offset and the first pg_sage comment met (its text
// and the span to remove, trailing white space included); ok is false for
// an unterminated comment.
func scanLeading(sql string) (i int, tag string, start, end int, ok bool) {
	for i < len(sql) {
		switch {
		case strings.ContainsAny(sql[i:i+1], " \t\r\n"):
			i++
		case strings.HasPrefix(sql[i:], "--"):
			nl := strings.IndexByte(sql[i:], '\n')
			if nl < 0 {
				return len(sql), tag, start, end, true
			}
			i += nl + 1
		case strings.HasPrefix(sql[i:], "/*"):
			n := commentLen(sql[i:])
			if n < 0 {
				return i, tag, start, end, false
			}
			if tag == "" && IsTagged(sql[i:i+n]) {
				tag, start = sql[i:i+n], i
				end = i + n + len(sql[i+n:]) - len(strings.TrimLeft(sql[i+n:], " \t\r\n"))
			}
			i += n
		default:
			return i, tag, start, end, true
		}
	}
	return i, tag, start, end, true
}

// commentLen is the length of the block comment s starts with (nested
// comments included, as PostgreSQL reads them), or -1 when unterminated.
func commentLen(s string) int {
	depth := 0
	for i := 0; i+1 < len(s); i++ {
		switch s[i : i+2] {
		case "/*":
			depth++
			i++
		case "*/":
			depth--
			i++
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

func firstComment(s string) string {
	if n := commentLen(s); n > 0 {
		return s[:n]
	}
	return ""
}

// keywordEnd is the end of the identifier-like word at sql[i:] (i itself
// when there is none).
func keywordEnd(sql string, i int) int {
	j := i
	for j < len(sql) {
		c := sql[j]
		isLetter := c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z')
		if !isLetter && (j == i || c < '0' || c > '9') {
			break
		}
		j++
	}
	return j
}
