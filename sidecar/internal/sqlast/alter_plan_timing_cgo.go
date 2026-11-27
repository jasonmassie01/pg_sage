//go:build cgo

package sqlast

import (
	"fmt"
	"strings"
	"unicode/utf8"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// timingClauses are the constraint timing clauses ADD CONSTRAINT ...
// USING INDEX also takes, as token sequences.
var timingClauses = []struct {
	text   string
	tokens []pg_query.Token
}{
	{"DEFERRABLE", []pg_query.Token{pg_query.Token_DEFERRABLE}},
	{"NOT DEFERRABLE", []pg_query.Token{pg_query.Token_NOT, pg_query.Token_DEFERRABLE}},
	{"INITIALLY DEFERRED", []pg_query.Token{pg_query.Token_INITIALLY,
		pg_query.Token_DEFERRED}},
	{"INITIALLY IMMEDIATE", []pg_query.Token{pg_query.Token_INITIALLY,
		pg_query.Token_IMMEDIATE}},
}

// readTiming reads the timing clauses written after the column list and
// renders them in canonical order. The tree has already refused
// conflicting clauses; a repeated clause, which it accepts, is refused
// here, and what was written must agree with what the tree recorded.
func readTiming(sql string, trailing []*pg_query.ScanToken,
	con *pg_query.Constraint) (string, error) {
	var deferrable, initially string
	for i := 0; i < len(trailing); {
		clause, width := matchTiming(trailing[i:])
		if clause == "" {
			return "", unsupportedOption(textFrom(sql, trailing[i]))
		}
		slot := &deferrable
		if strings.HasPrefix(clause, "INITIALLY") {
			slot = &initially
		}
		if *slot != "" {
			return "", fmt.Errorf("%w: conflicting constraint timing %q after %q; "+
				"plan it by hand", ErrUnsupportedOption, clause, *slot)
		}
		*slot = clause
		i += width
	}
	initDeferred := initially == "INITIALLY DEFERRED"
	if con.GetInitdeferred() != initDeferred ||
		con.GetDeferrable() != (deferrable == "DEFERRABLE" || initDeferred) {
		return "", fmt.Errorf("%w: the constraint timing as written does not match "+
			"the parse tree; plan it by hand", ErrNotPlannable)
	}
	timing := ""
	for _, clause := range []string{deferrable, initially} {
		if clause != "" {
			timing += " " + clause
		}
	}
	return timing, nil
}

func matchTiming(tokens []*pg_query.ScanToken) (string, int) {
	for _, clause := range timingClauses {
		if len(tokens) < len(clause.tokens) {
			continue
		}
		matched := true
		for k, want := range clause.tokens {
			matched = matched && tokens[k].GetToken() == want
		}
		if matched {
			return clause.text, len(clause.tokens)
		}
	}
	return "", 0
}

// checkIdentifiers refuses any identifier PostgreSQL would truncate, and
// a bare non-ASCII one, which the server folds by its own encoding. It
// reads the scanner's tokens: the parser has already cut long names to
// 63 bytes with only a NOTICE. A U&"..." name is measured with its
// escapes still written, which is never shorter than the decoded name.
func checkIdentifiers(sql string, tokens []*pg_query.ScanToken) error {
	for _, tok := range tokens {
		if tok.GetToken() != pg_query.Token_IDENT && tok.GetToken() != pg_query.Token_UIDENT {
			continue
		}
		raw, ok := tokenText(sql, tok)
		if !ok {
			return fmt.Errorf("%w: the scanner reported an identifier outside the "+
				"statement; plan it by hand", ErrNotPlannable)
		}
		name, quoted := identifierText(raw)
		if len(name) > maxNameBytes {
			return fmt.Errorf("%w: %q is %d bytes; PostgreSQL truncates names longer "+
				"than %d bytes; plan it by hand", ErrNameTooLong, echo(name), len(name),
				maxNameBytes)
		}
		if !quoted && !isASCII(name) {
			return fmt.Errorf("%w: bare identifier %q is not ASCII and PostgreSQL folds "+
				"it by the server encoding; quote it", ErrNotPlannable, name)
		}
	}
	return nil
}

// identifierText returns the name a quoted identifier token spells, or a
// bare token as written (folding never changes its length).
func identifierText(raw string) (string, bool) {
	body := raw
	switch {
	case len(raw) >= 4 && (raw[0] == 'U' || raw[0] == 'u') && raw[1] == '&':
		body = raw[2:]
	case !strings.HasPrefix(raw, `"`):
		return raw, false
	}
	if len(body) >= 2 && body[0] == '"' && body[len(body)-1] == '"' {
		body = body[1 : len(body)-1]
	}
	return strings.ReplaceAll(body, `""`, `"`), true
}

func isASCII(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
