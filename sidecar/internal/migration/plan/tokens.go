package plan

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

type tokenKind int

const (
	tokEOF    tokenKind = iota
	tokWord             // bare keyword or unquoted identifier: [A-Za-z0-9_$]+
	tokQuoted           // "double-quoted identifier", raw form with quotes
	tokString           // 'string literal'
	tokOther            // punctuation or any other single character
)

type token struct {
	kind  tokenKind
	raw   string // exact source text
	start int    // byte offset in the statement
}

// tokenize splits sql with the quoting rules validate.go uses: a
// doubled quote inside a quoted identifier or literal is an escape.
// Bare identifiers are ASCII only; PostgreSQL does not lower-case
// non-ASCII letters in UTF-8, so guessing their folded form is refused.
func tokenize(sql string) ([]token, error) {
	var toks []token
	for i := 0; i < len(sql); {
		c := sql[i]
		start := i
		switch {
		case strings.IndexByte(" \t\n\r\f\v", c) >= 0:
			i++
			continue
		case c == '"' || c == '\'':
			end, err := quotedEnd(sql, i)
			if err != nil {
				return nil, err
			}
			kind := tokQuoted
			if c == '\'' {
				kind = tokString
			}
			toks = append(toks, token{kind: kind, raw: sql[i:end], start: start})
			i = end
			continue
		case isWordByte(c):
			for i < len(sql) && isWordByte(sql[i]) {
				i++
			}
			toks = append(toks, token{kind: tokWord, raw: sql[start:i], start: start})
			continue
		}
		_, size := utf8.DecodeRuneInString(sql[i:])
		i += size
		toks = append(toks, token{kind: tokOther, raw: sql[start:i], start: start})
	}
	return toks, nil
}

// quotedEnd returns the offset just past the quoted token at i.
func quotedEnd(sql string, i int) (int, error) {
	q := sql[i]
	for j := i + 1; j < len(sql); j++ {
		if sql[j] != q {
			continue
		}
		if j+1 < len(sql) && sql[j+1] == q {
			j++
			continue
		}
		if q == '"' && j == i+1 {
			return 0, fmt.Errorf("%w: zero-length quoted identifier",
				errUnsupportedStatement)
		}
		return j + 1, nil
	}
	return 0, fmt.Errorf("%w: unterminated quoted text starting at byte %d",
		errUnsupportedStatement, i)
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || (c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

type parser struct {
	sql  string
	toks []token
	pos  int
}

func (p *parser) peek() token {
	if p.pos >= len(p.toks) {
		return token{kind: tokEOF, start: len(p.sql)}
	}
	return p.toks[p.pos]
}

func (p *parser) done() bool { return p.pos >= len(p.toks) }

// keyword consumes the bare words in order, or nothing at all. A
// quoted "UNIQUE" is an identifier, never a keyword.
func (p *parser) keyword(words ...string) bool {
	for k, word := range words {
		at := p.pos + k
		if at >= len(p.toks) || p.toks[at].kind != tokWord ||
			!strings.EqualFold(p.toks[at].raw, word) {
			return false
		}
	}
	p.pos += len(words)
	return true
}

func (p *parser) peekKeyword(word string) bool {
	tok := p.peek()
	return tok.kind == tokWord && strings.EqualFold(tok.raw, word)
}

func (p *parser) punct(text string) bool {
	if tok := p.peek(); tok.kind == tokOther && tok.raw == text {
		p.pos++
		return true
	}
	return false
}

// ident consumes one identifier and returns the name PostgreSQL
// resolves it to: quoted names unquoted verbatim, bare names folded.
func (p *parser) ident(what string) (string, error) {
	tok := p.peek()
	bare := tok.kind == tokWord && !(tok.raw[0] >= '0' && tok.raw[0] <= '9') &&
		tok.raw[0] != '$'
	if !bare && tok.kind != tokQuoted {
		found := "end of statement"
		if tok.kind != tokEOF {
			found = fmt.Sprintf("%q", tok.raw)
		}
		return "", fmt.Errorf("%w: expected %s, found %s",
			errUnsupportedStatement, what, found)
	}
	p.pos++
	return foldIdent(tok.raw), nil
}

func (p *parser) rest() string { return p.restFrom(p.pos) }

func (p *parser) restFrom(pos int) string {
	if pos >= len(p.toks) {
		return ""
	}
	return p.sql[p.toks[pos].start:]
}
