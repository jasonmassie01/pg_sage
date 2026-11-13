package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

var errUnscripted = errors.New("scripted parser: statement not in the test's script")

// scripted is how the C parser reads one statement: the read, or the
// refusal it returns. Builds without cgo answer from a script of these
// (useScriptedParser); builds with cgo ignore the script and parse.
type scripted struct {
	read sqlast.BrokeredRead
	err  error
}

// selectRead scripts one SELECT: its relations (unqualified names on the
// search path), called functions, $n count, bare output columns and the
// columns it references anywhere else.
func selectRead(sql string, params int, rels, funcs, out, other []string) scripted {
	q := sqlast.ReadQuery{}
	for _, r := range rels {
		q.Relations = append(q.Relations, sqlast.QualifiedName{Name: r})
	}
	for _, f := range funcs {
		q.Functions = append(q.Functions, sqlast.QualifiedName{Name: f})
	}
	sum := sha256.Sum256([]byte(sql))
	return scripted{read: sqlast.BrokeredRead{Canonical: sql,
		Fingerprint: hex.EncodeToString(sum[:8]), Params: params, Query: q,
		OutputColumns: out, OtherColumnRefs: other}}
}

// scriptedParser answers only the statements of script; anything else is
// refused as the parser refuses an unsupported shape.
func scriptedParser(script map[string]scripted) parser {
	brokered := func(sql string) (sqlast.BrokeredRead, error) {
		s, ok := script[sql]
		if !ok {
			return sqlast.BrokeredRead{}, errUnscripted
		}
		return s.read, s.err
	}
	return parser{brokered: brokered, read: func(sql string) (sqlast.ReadQuery, error) {
		read, err := brokered(sql)
		return read.Query, err
	}}
}
