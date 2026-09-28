package optimizer

import (
	"fmt"
	"strings"
	"unicode"
)

// ddlScanner is a minimal tokenizer for the CREATE INDEX grammar used by
// ParseIndexDDL. It understands quoted identifiers, string literals and
// balanced parentheses; it is not a general SQL parser.
type ddlScanner struct {
	src string
	pos int
}

func (s *ddlScanner) skipSpace() {
	for s.pos < len(s.src) && unicode.IsSpace(rune(s.src[s.pos])) {
		s.pos++
	}
}

func (s *ddlScanner) rest() string {
	s.skipSpace()
	return s.src[s.pos:]
}

// peekKeyword reports whether the next token is kw (case-insensitive).
func (s *ddlScanner) peekKeyword(kw string) bool {
	s.skipSpace()
	end := s.pos + len(kw)
	if end > len(s.src) || !strings.EqualFold(s.src[s.pos:end], kw) {
		return false
	}
	return end == len(s.src) || !isIdentByte(s.src[end])
}

// keyword consumes kw when it is the next token.
func (s *ddlScanner) keyword(kw string) bool {
	if !s.peekKeyword(kw) {
		return false
	}
	s.pos += len(kw)
	return true
}

func (s *ddlScanner) consume(c byte) bool {
	s.skipSpace()
	if s.pos < len(s.src) && s.src[s.pos] == c {
		s.pos++
		return true
	}
	return false
}

// identifier reads one identifier. Quoted identifiers keep their case
// (with "" unescaped); unquoted ones are case-folded like PostgreSQL.
func (s *ddlScanner) identifier() (string, error) {
	s.skipSpace()
	if s.pos >= len(s.src) {
		return "", fmt.Errorf("unexpected end of statement")
	}
	if s.src[s.pos] == '"' {
		return s.quotedIdentifier()
	}
	start := s.pos
	for s.pos < len(s.src) && isIdentByte(s.src[s.pos]) {
		s.pos++
	}
	if s.pos == start {
		return "", fmt.Errorf("expected identifier at %q", s.src[start:])
	}
	return strings.ToLower(s.src[start:s.pos]), nil
}

func (s *ddlScanner) quotedIdentifier() (string, error) {
	var b strings.Builder
	for i := s.pos + 1; i < len(s.src); i++ {
		if s.src[i] != '"' {
			b.WriteByte(s.src[i])
			continue
		}
		if i+1 < len(s.src) && s.src[i+1] == '"' {
			b.WriteByte('"')
			i++
			continue
		}
		s.pos = i + 1
		if b.Len() == 0 {
			return "", fmt.Errorf("empty quoted identifier")
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("unterminated quoted identifier")
}

// parens reads a balanced "( ... )" group and returns its inner text.
func (s *ddlScanner) parens() (string, error) {
	if !s.consume('(') {
		return "", fmt.Errorf("expected '('")
	}
	start, depth := s.pos, 1
	var quote byte
	for ; s.pos < len(s.src); s.pos++ {
		c := s.src[s.pos]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				inner := s.src[start:s.pos]
				s.pos++
				return inner, nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced parentheses")
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' ||
		c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}
