package explain

import (
	"fmt"
	"strings"
)

// allowedLeadingKeywords is the single-statement allowlist for
// /explain. Anything else (DML, DDL, transaction control, utility
// statements) is rejected before a connection is acquired.
var allowedLeadingKeywords = map[string]bool{
	"SELECT": true, "WITH": true, "VALUES": true, "TABLE": true,
}

// validateExplainQuery accepts exactly one read statement. It rejects
// any statement separator outside string literals, quoted identifiers,
// dollar quotes and comments; a single trailing ';' is tolerated.
func validateExplainQuery(query string) error {
	_, err := explainBody(query)
	return err
}

// explainBody returns the query with any trailing terminator removed,
// or an ErrExplainInvalidRequest error.
func explainBody(query string) (string, error) {
	s := sqlScanner{src: query}
	first, err := s.firstKeyword()
	if err != nil {
		return "", err
	}
	if !allowedLeadingKeywords[first] {
		return "", invalidExplain(
			"DDL, DML and utility statements cannot be explained; " +
				"only a single SELECT, WITH, VALUES or TABLE " +
				"statement is allowed")
	}
	end, err := s.statementEnd()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(query[:end]), nil
}

func invalidExplain(msg string) error {
	return fmt.Errorf("%w: %s", ErrExplainInvalidRequest, msg)
}

// sqlScanner walks PostgreSQL SQL text, skipping literals, quoted
// identifiers, dollar-quoted bodies and (nested) comments.
type sqlScanner struct {
	src string
	pos int
}

// firstKeyword skips whitespace, comments and opening parentheses and
// returns the upper-cased leading word.
func (s *sqlScanner) firstKeyword() (string, error) {
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch {
		case isSpace(c) || c == '(':
			s.pos++
		case s.startsComment():
			if err := s.skipComment(); err != nil {
				return "", err
			}
		default:
			start := s.pos
			for s.pos < len(s.src) && isWordByte(s.src[s.pos]) {
				s.pos++
			}
			if start == s.pos {
				return "", invalidExplain("query must start with a keyword")
			}
			return strings.ToUpper(s.src[start:s.pos]), nil
		}
	}
	return "", invalidExplain("query is empty")
}

// statementEnd returns the byte offset where the single statement
// ends: either len(src) or the position of a terminating ';' that is
// followed only by whitespace and comments.
func (s *sqlScanner) statementEnd() (int, error) {
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch {
		case c == ';':
			end := s.pos
			s.pos++
			if err := s.onlyTriviaRemains(); err != nil {
				return 0, err
			}
			return end, nil
		case s.startsComment():
			if err := s.skipComment(); err != nil {
				return 0, err
			}
		default:
			if err := s.skipToken(); err != nil {
				return 0, err
			}
		}
	}
	return len(s.src), nil
}

func (s *sqlScanner) onlyTriviaRemains() error {
	for s.pos < len(s.src) {
		switch {
		case isSpace(s.src[s.pos]):
			s.pos++
		case s.startsComment():
			if err := s.skipComment(); err != nil {
				return err
			}
		default:
			return invalidExplain("multiple statements are not allowed")
		}
	}
	return nil
}

// skipToken consumes one literal, quoted identifier, dollar-quoted
// body, or plain byte.
func (s *sqlScanner) skipToken() error {
	c := s.src[s.pos]
	switch {
	case c == '\'':
		// E'...' strings honour backslash escapes; all other string
		// forms assume standard_conforming_strings=on (PG default).
		// If a server ran with it off, PG would see a longer literal
		// than we do, which only makes this scan stricter.
		escapes := s.pos > 0 && (s.src[s.pos-1] == 'E' ||
			s.src[s.pos-1] == 'e') && !s.prevIsWordAt(s.pos-2)
		return s.skipQuoted('\'', escapes)
	case c == '"':
		return s.skipQuoted('"', false)
	case c == '$':
		return s.skipDollar()
	case isWordByte(c):
		// Consume whole identifiers/keywords, including embedded '$'
		// (legal after the first character), so "a$b$" is never
		// mistaken for the start of a dollar-quoted body.
		for s.pos < len(s.src) &&
			(isWordByte(s.src[s.pos]) || s.src[s.pos] == '$') {
			s.pos++
		}
		return nil
	default:
		s.pos++
		return nil
	}
}

func (s *sqlScanner) prevIsWordAt(i int) bool {
	return i >= 0 && isWordByte(s.src[i])
}

func (s *sqlScanner) skipQuoted(quote byte, backslashEscapes bool) error {
	s.pos++
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch {
		case backslashEscapes && c == '\\':
			s.pos += 2
		case c == quote:
			if s.pos+1 < len(s.src) && s.src[s.pos+1] == quote {
				s.pos += 2
				continue
			}
			s.pos++
			return nil
		default:
			s.pos++
		}
	}
	return invalidExplain("unterminated quoted string or identifier")
}

// skipDollar handles $n parameters and $tag$...$tag$ bodies.
func (s *sqlScanner) skipDollar() error {
	start := s.pos
	i := s.pos + 1
	if i < len(s.src) && s.src[i] >= '0' && s.src[i] <= '9' {
		s.pos++ // positional parameter such as $1
		return nil
	}
	for i < len(s.src) && isWordByte(s.src[i]) {
		i++
	}
	if i >= len(s.src) || s.src[i] != '$' {
		s.pos++
		return nil
	}
	tag := s.src[start : i+1]
	closing := strings.Index(s.src[i+1:], tag)
	if closing < 0 {
		return invalidExplain("unterminated dollar-quoted string")
	}
	s.pos = i + 1 + closing + len(tag)
	return nil
}

func (s *sqlScanner) startsComment() bool {
	return strings.HasPrefix(s.src[s.pos:], "--") ||
		strings.HasPrefix(s.src[s.pos:], "/*")
}

// skipComment consumes a line comment or a nested block comment.
func (s *sqlScanner) skipComment() error {
	if strings.HasPrefix(s.src[s.pos:], "--") {
		nl := strings.IndexByte(s.src[s.pos:], '\n')
		if nl < 0 {
			s.pos = len(s.src)
		} else {
			s.pos += nl + 1
		}
		return nil
	}
	depth := 0
	for s.pos < len(s.src) {
		switch {
		case strings.HasPrefix(s.src[s.pos:], "/*"):
			depth++
			s.pos += 2
		case strings.HasPrefix(s.src[s.pos:], "*/"):
			depth--
			s.pos += 2
			if depth == 0 {
				return nil
			}
		default:
			s.pos++
		}
	}
	return invalidExplain("unterminated block comment")
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c >= 0x80
}
