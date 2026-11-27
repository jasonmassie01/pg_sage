package planstability

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// The predicate shapes the rules read.
const (
	opEqual   = "="
	opNotNull = "is_not_null"
	opRange   = "range"
)

// predicate is one column compared with a value (a $n parameter, a
// literal, or a call without arguments such as now()) in a WHERE, ON or
// HAVING clause: col = $1, $1 = col, col IS NOT NULL, col < $1. Qualifier
// is the table or alias written before the column ("" when none); names
// are as PostgreSQL resolves them.
type predicate struct {
	qualifier, column, op string
}

// predicates reads the filters of a normalized statement text. Text the
// lexer cannot read (unterminated, several statements) is an error with
// the reason; the caller skips the query and says why.
func predicates(text string) ([]predicate, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("empty statement text")
	}
	scan, err := sqlast.ScanSQL(text)
	if err != nil {
		return nil, fmt.Errorf("statement text does not lex: %w", err)
	}
	if scan.Statements != 1 {
		return nil, fmt.Errorf("statement text holds %d statements, not one", scan.Statements)
	}
	toks := tokenize(text)
	var out []predicate
	active := false
	for i := range toks {
		switch {
		case isWord(toks[i], "WHERE"), isWord(toks[i], "ON"), isWord(toks[i], "HAVING"):
			active = true
		case isWord(toks[i], "SET"):
			active = false // UPDATE ... SET col = $1 assigns, it does not filter
		case active:
			if p, ok := matchAt(toks, i); ok {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// matchAt reads a predicate starting at toks[i]: a column then its
// comparison, or a value, "=" and a column.
func matchAt(toks []token, i int) (predicate, bool) {
	if i > 0 && (toks[i-1].text == "." || isOperator(toks[i-1].text)) {
		return predicate{}, false
	}
	if isValue(toks, i) && i+2 < len(toks) && toks[i+1].text == "=" {
		p, next, ok := columnRef(toks, i+2)
		if ok && (next >= len(toks) || !isOperator(toks[next].text) && toks[next].text != "(") {
			p.op = opEqual
			return p, true
		}
		return predicate{}, false
	}
	p, next, ok := columnRef(toks, i)
	if !ok || next >= len(toks) {
		return predicate{}, false
	}
	if next+2 < len(toks) && isWord(toks[next], "IS") && isWord(toks[next+1], "NOT") &&
		isWord(toks[next+2], "NULL") {
		p.op = opNotNull
		return p, true
	}
	switch toks[next].text {
	case "=":
		p.op = opEqual
	case "<", ">", "<=", ">=":
		p.op = opRange
	default:
		return predicate{}, false
	}
	if next+1 >= len(toks) || !isValue(toks, next+1) {
		return predicate{}, false
	}
	return p, true
}

// columnRef reads ident or ident.ident at i; next is the token after it.
// A call (name followed by "(") is not a column.
func columnRef(toks []token, i int) (predicate, int, bool) {
	if !toks[i].ident {
		return predicate{}, 0, false
	}
	if i+2 < len(toks) && toks[i+1].text == "." && toks[i+2].ident {
		return predicate{qualifier: toks[i].text, column: toks[i+2].text}, i + 3, true
	}
	if i+1 < len(toks) && toks[i+1].text == "(" {
		return predicate{}, 0, false
	}
	return predicate{column: toks[i].text}, i + 1, true
}

// isValue: a parameter, a literal, a number or a call with no arguments
// (now()), never another column (a join) or ANY/ALL/SOME(...).
func isValue(toks []token, i int) bool {
	t := toks[i]
	switch {
	case t.ident:
		return !isWord(t, "ANY") && !isWord(t, "ALL") && !isWord(t, "SOME") &&
			i+2 < len(toks) && toks[i+1].text == "(" && toks[i+2].text == ")"
	case strings.HasPrefix(t.text, "$"), strings.HasPrefix(t.text, "'"):
		return true
	}
	return t.text != "" && t.text[0] >= '0' && t.text[0] <= '9'
}

func isWord(t token, word string) bool {
	return t.ident && !t.quoted && strings.EqualFold(t.text, word)
}

func isOperator(s string) bool {
	return s != "" && strings.ContainsRune(operatorChars, rune(s[0]))
}
