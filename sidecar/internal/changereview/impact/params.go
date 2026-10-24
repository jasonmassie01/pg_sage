package impact

import (
	"strconv"
	"strings"
)

// denseParams renumbers a statement's $n parameters 1..k in order of
// first use, outside string literals and quoted identifiers. A read shape
// keeps only the WHERE clause's parameters (UPDATE ... SET a = $1 WHERE
// b = $2 plans as SELECT 1 ... WHERE b = $2), and PostgreSQL cannot
// prepare a statement whose $1 it never sees.
func denseParams(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	seen := map[string]int{}
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case c == '\'' || c == '"':
			end := closingQuote(sql, i)
			b.WriteString(sql[i:end])
			i = end - 1
		case c == '$' && i+1 < len(sql) && isDigit(sql[i+1]):
			j := i + 1
			for j < len(sql) && isDigit(sql[j]) {
				j++
			}
			n, ok := seen[sql[i+1:j]]
			if !ok {
				n = len(seen) + 1
				seen[sql[i+1:j]] = n
			}
			b.WriteString("$" + strconv.Itoa(n))
			i = j - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// closingQuote is the index after the quote that closes the one at i (a
// doubled quote is an escaped one); len(s) when it is never closed.
func closingQuote(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] != q {
			continue
		}
		if j+1 < len(s) && s[j+1] == q {
			j++
			continue
		}
		return j + 1
	}
	return len(s)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
