package changereview

import (
	"strings"
	"unicode"
)

// textScanner normalizes SQL text without a parser: comments become
// whitespace, whitespace runs collapse to one space, text outside quotes
// is lower-cased (unquoted identifiers fold, keywords are case-blind),
// and statements split on semicolons outside quotes, comments and
// dollar-quoted bodies. Quoted text is kept byte for byte.
type textScanner struct {
	src   []rune
	i     int
	cur   strings.Builder
	out   []string
	space bool
}

// textStatements is the normalized text of each statement of sql.
func textStatements(sql string) []string {
	s := &textScanner{src: []rune(sql)}
	for s.i < len(s.src) {
		s.step()
	}
	s.flush()
	return s.out
}

func (s *textScanner) step() {
	r := s.src[s.i]
	switch {
	case r == '-' && s.peek(1) == '-':
		s.skipUntil("\n")
	case r == '/' && s.peek(1) == '*':
		s.skipBlockComment()
	case r == '\'' || r == '"':
		s.copyQuoted(r)
	case r == '$':
		s.copyDollar()
	case r == ';':
		s.i++
		s.flush()
	case unicode.IsSpace(r):
		s.i++
		s.space = true
	default:
		s.emit(string(unicode.ToLower(r)))
		s.i++
	}
}

func (s *textScanner) peek(n int) rune {
	if s.i+n < len(s.src) {
		return s.src[s.i+n]
	}
	return 0
}

// emit writes text, preceded by one space when whitespace came before.
func (s *textScanner) emit(text string) {
	if s.space && s.cur.Len() > 0 {
		s.cur.WriteByte(' ')
	}
	s.space = false
	s.cur.WriteString(text)
}

func (s *textScanner) flush() {
	if stmt := strings.TrimSpace(s.cur.String()); stmt != "" {
		s.out = append(s.out, stmt)
	}
	s.cur.Reset()
	s.space = false
}

func (s *textScanner) skipUntil(end string) {
	rest := string(s.src[s.i:])
	j := strings.Index(rest, end)
	if j < 0 {
		s.i = len(s.src)
	} else {
		s.i += len([]rune(rest[:j+len(end)]))
	}
	s.space = true
}

func (s *textScanner) skipBlockComment() {
	depth := 0
	for s.i < len(s.src) {
		switch {
		case s.src[s.i] == '/' && s.peek(1) == '*':
			depth++
			s.i += 2
		case s.src[s.i] == '*' && s.peek(1) == '/':
			depth--
			s.i += 2
			if depth == 0 {
				s.space = true
				return
			}
		default:
			s.i++
		}
	}
	s.space = true
}

// copyQuoted copies a '...' or "..." token verbatim; a doubled quote is
// an escaped quote.
func (s *textScanner) copyQuoted(q rune) {
	start := s.i
	s.i++
	for s.i < len(s.src) {
		if s.src[s.i] == q {
			if s.peek(1) == q {
				s.i += 2
				continue
			}
			s.i++
			break
		}
		s.i++
	}
	s.emit(string(s.src[start:s.i]))
}

// copyDollar copies a $tag$...$tag$ body verbatim, or a lone $ (a
// parameter like $1) as text.
func (s *textScanner) copyDollar() {
	j := s.i + 1
	for j < len(s.src) && (s.src[j] == '_' || unicode.IsLetter(s.src[j]) ||
		(j > s.i+1 && unicode.IsDigit(s.src[j]))) {
		j++
	}
	if j >= len(s.src) || s.src[j] != '$' {
		s.emit("$")
		s.i++
		return
	}
	tag := string(s.src[s.i : j+1])
	rest := string(s.src[j+1:])
	end := strings.Index(rest, tag)
	if end < 0 {
		s.emit(string(s.src[s.i:]))
		s.i = len(s.src)
		return
	}
	body := tag + rest[:end] + tag
	s.emit(body)
	s.i = j + 1 + len([]rune(rest[:end])) + len([]rune(tag))
}
