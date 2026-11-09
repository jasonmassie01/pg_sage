package explain

import (
	"errors"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// ReadStatement returns query without a trailing terminator when it is a
// single read statement (SELECT, WITH, VALUES or TABLE), or an
// ErrExplainInvalidRequest error. It checks the leading keyword only, as
// the /explain API's validator does.
func ReadStatement(query string) (string, error) { return explainBody(query) }

// ReadOnlyStatement is ReadStatement that also refuses a read-looking
// statement that writes or locks rows: a data-modifying CTE, FOR
// UPDATE/SHARE or SELECT INTO. PREPARE alone takes such a statement's
// locks, so the SRE investigator's plan-only EXPLAIN uses it on the text
// it explains. With cgo the parse tree decides (a text it cannot parse
// is refused); without cgo a token scan refuses the write and locking
// keywords outside literals, quoted identifiers and comments.
func ReadOnlyStatement(query string) (string, error) {
	body, err := explainBody(query)
	if err != nil {
		return "", err
	}
	shape, err := sqlast.InspectReadQuery(body)
	switch {
	case err == nil:
		if reason := writeRefusal(shape); reason != "" {
			return "", invalidExplain("statement " + reason)
		}
		return body, nil
	case !errors.Is(err, sqlast.ErrUnavailable):
		return "", invalidExplain("statement is not one read statement: " + err.Error())
	}
	s := sqlScanner{src: body}
	word, err := s.firstWriteKeyword()
	if err != nil {
		return "", err
	}
	if word != "" {
		return "", invalidExplain("statement contains " + word +
			", which writes, creates or locks rows")
	}
	return body, nil
}

// writeKeywords make a read-looking statement write or lock rows:
// INSERT/UPDATE/DELETE/MERGE in a CTE, FOR [NO KEY] UPDATE and FOR [KEY]
// SHARE, SELECT INTO. A column named like one is refused too (fail
// closed); the parse tree tells them apart when cgo is available.
var writeKeywords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "INTO": true,
	"SHARE": true,
}

// firstWriteKeyword returns the first write keyword outside literals,
// quoted identifiers, dollar quotes and comments ("" without one).
func (s *sqlScanner) firstWriteKeyword() (string, error) {
	for s.pos < len(s.src) {
		if s.startsComment() {
			if err := s.skipComment(); err != nil {
				return "", err
			}
			continue
		}
		start := s.pos
		word := isWordByte(s.src[s.pos])
		if err := s.skipToken(); err != nil {
			return "", err
		}
		if w := strings.ToUpper(s.src[start:s.pos]); word && writeKeywords[w] {
			return w, nil
		}
	}
	return "", nil
}
