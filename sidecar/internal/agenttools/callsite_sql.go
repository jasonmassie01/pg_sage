package agenttools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// CallSite traces the pg_stat_statements statement id to the application
// code that sends it: its text gives the sqlcommenter tags and the
// fingerprint FindCallSite matches literals by.
func (t *Tools) CallSite(ctx context.Context, host codehost.Host, repo string,
	id QueryID) (CallSite, error) {
	if err := t.ready(); err != nil {
		return CallSite{}, err
	}
	if id == 0 {
		return CallSite{}, invalid("query_id is required")
	}
	stmts, err := t.statementsByID(ctx, []int64{int64(id)})
	if err != nil {
		return CallSite{}, err
	}
	s, ok := stmts[int64(id)]
	if !ok {
		return CallSite{}, fmt.Errorf("%w: no pg_stat_statements entry for query %d in "+
			"this database", ErrNotFound, id)
	}
	return FindCallSite(ctx, host, repo, id, sqlast.ParseSQLCommenter(s.Text), s.Text)
}

// maxSearchTerms is how many identifiers a literal search sends.
const maxSearchTerms = 3

// fpKeywords are words that say nothing about which statement it is.
var fpKeywords = wordSet(`select from where and or not in is null as on join
		left right inner outer full cross group by order having limit offset insert into
		values update set delete returning with distinct union all case when then else end
		asc desc count sum min max avg exists between like ilike coalesce true false any
		some interval nulls first last lateral using natural fetch next rows only for share
		nowait skip locked conflict do nothing default cast filter over partition window
		array now`)

func wordSet(words string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(words) {
		out[w] = true
	}
	return out
}

// fpToken is one token of a fingerprint; word marks an identifier or keyword.
type fpToken struct {
	text string
	word bool
}

// sqlFingerprint reduces a statement to what identifies it across the
// pg_stat_statements text and the application's literal: comments are
// dropped; literals, numbers and parameters ($1, ?, %s, %(name)s, :name,
// @name, #{...}) become "?"; identifiers and keywords are kept in lower
// case; IN lists collapse and a trailing ";" goes. "" means nothing is left.
func sqlFingerprint(sql string) string {
	toks := fpTokens(sql)
	for len(toks) > 0 && toks[len(toks)-1].text == ";" {
		toks = toks[:len(toks)-1]
	}
	out := make([]string, 0, len(toks))
	for i := 0; i < len(toks); i++ {
		out = append(out, toks[i].text)
		if toks[i].text != "in" || i+2 >= len(toks) || toks[i+1].text != "(" ||
			toks[i+2].text != "?" {
			continue
		}
		j := i + 3
		for j+1 < len(toks) && toks[j].text == "," && toks[j+1].text == "?" {
			j += 2
		}
		if j < len(toks) && toks[j].text == ")" {
			out = append(out, "(", "?", ")")
			i = j
		}
	}
	return strings.Join(out, " ")
}

// searchTerms picks the statement's longest identifiers (3 bytes or more)
// for a code search.
func searchTerms(sql string) []string {
	seen := map[string]bool{}
	var terms []string
	for _, tok := range fpTokens(sql) {
		if tok.word && len(tok.text) >= 3 && !fpKeywords[tok.text] && !seen[tok.text] {
			seen[tok.text] = true
			terms = append(terms, tok.text)
		}
	}
	sort.Slice(terms, func(i, j int) bool {
		if len(terms[i]) != len(terms[j]) {
			return len(terms[i]) > len(terms[j])
		}
		return terms[i] < terms[j]
	})
	if len(terms) > maxSearchTerms {
		terms = terms[:maxSearchTerms]
	}
	return terms
}

func fpTokens(s string) []fpToken {
	var out []fpToken
	for i := 0; i < len(s); {
		tok, next := fpNext(s, i)
		if tok.text != "" {
			out = append(out, tok)
		}
		i = next
	}
	return out
}

// fpNext returns the token at i ("" for skipped input) and where the next starts.
func fpNext(s string, i int) (fpToken, int) {
	c := s[i]
	rest := s[i:]
	switch {
	case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
		return fpToken{}, i + 1
	case strings.HasPrefix(rest, "--"):
		return fpToken{}, endOfLine(s, i)
	case strings.HasPrefix(rest, "/*"):
		return fpToken{}, endOfBlockComment(s, i)
	case c == '\'':
		return fpToken{text: "?"}, endOfQuoted(s, i+1, '\'')
	case c == '"':
		end := endOfQuoted(s, i+1, '"')
		return fpToken{text: strings.ToLower(strings.Trim(s[i:end], `"`)), word: true}, end
	case c == '$' || c == '?' || c == '%' || c == ':' || c == '@' || c == '#':
		return fpParam(s, i)
	case c >= '0' && c <= '9' || c == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9':
		j := i
		for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
			j++
		}
		return fpToken{text: "?"}, j
	case isWordByte(c):
		return fpWord(s, i)
	}
	for _, op := range []string{"->>", "::", "<=", ">=", "<>", "!=", "||", "->", "<@"} {
		if strings.HasPrefix(rest, op) {
			return fpToken{text: op}, i + len(op)
		}
	}
	return fpToken{text: string(c)}, i + 1
}

// fpParam reads a parameter, a dollar-quoted literal, or an operator
// starting with $ ? % : @ #.
func fpParam(s string, i int) (fpToken, int) {
	rest := s[i:]
	switch {
	case rest[0] == '?':
		return fpToken{text: "?"}, i + 1
	case strings.HasPrefix(rest, "::"):
		return fpToken{text: "::"}, i + 2
	case strings.HasPrefix(rest, "%s"), strings.HasPrefix(rest, "%d"):
		return fpToken{text: "?"}, i + 2
	case strings.HasPrefix(rest, "%("):
		if end := strings.Index(rest, ")s"); end > 0 {
			return fpToken{text: "?"}, i + end + 2
		}
	case strings.HasPrefix(rest, "#{"):
		if end := strings.IndexByte(rest, '}'); end > 0 {
			return fpToken{text: "?"}, i + end + 1
		}
	case rest[0] == '$':
		return fpDollar(s, i)
	case (rest[0] == ':' || rest[0] == '@') && len(rest) > 1 && isWordByte(rest[1]):
		_, next := fpWord(s, i+1)
		return fpToken{text: "?"}, next
	}
	return fpToken{text: string(rest[0])}, i + 1
}

// fpDollar reads $1 parameters and $tag$...$tag$ literals.
func fpDollar(s string, i int) (fpToken, int) {
	j := i + 1
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j > i+1 {
		return fpToken{text: "?"}, j
	}
	for j < len(s) && isWordByte(s[j]) {
		j++
	}
	if j < len(s) && s[j] == '$' {
		tag := s[i : j+1]
		if end := strings.Index(s[j+1:], tag); end >= 0 {
			return fpToken{text: "?"}, j + 1 + end + len(tag)
		}
		return fpToken{text: "?"}, len(s)
	}
	return fpToken{text: "$"}, i + 1
}

// fpWord reads a keyword or identifier, or an E'...'-style literal.
func fpWord(s string, i int) (fpToken, int) {
	j := i
	for j < len(s) && (isWordByte(s[j]) || s[j] >= '0' && s[j] <= '9' || s[j] == '$') {
		j++
	}
	word := strings.ToLower(s[i:j])
	if j < len(s) && s[j] == '\'' && len(word) == 1 && strings.Contains("ebxn", word) {
		return fpToken{text: "?"}, endOfQuoted(s, j+1, '\'')
	}
	return fpToken{text: word, word: true}, j
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func endOfLine(s string, i int) int {
	if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
		return i + j + 1
	}
	return len(s)
}

func endOfBlockComment(s string, i int) int {
	depth := 0
	for j := i; j+1 < len(s); j++ {
		switch {
		case s[j] == '/' && s[j+1] == '*':
			depth++
			j++
		case s[j] == '*' && s[j+1] == '/':
			depth--
			j++
			if depth == 0 {
				return j + 1
			}
		}
	}
	return len(s)
}

// endOfQuoted is the index after the closing quote of a literal whose body
// starts at i; a doubled quote or a backslash escape stays inside.
func endOfQuoted(s string, i int, quote byte) int {
	for j := i; j < len(s); j++ {
		switch {
		case s[j] == '\\':
			j++
		case s[j] == quote && j+1 < len(s) && s[j+1] == quote:
			j++
		case s[j] == quote:
			return j + 1
		}
	}
	return len(s)
}
