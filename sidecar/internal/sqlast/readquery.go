package sqlast

import "errors"

// ErrUnavailable reports that parse-tree inspection is not compiled in
// (a build without cgo). Callers must treat the query as unverified.
var ErrUnavailable = errors.New("parse-tree inspection unavailable in this build (no cgo)")

// QualifiedName is a catalog object name exactly as the parser resolved
// its spelling: unquoted identifiers are folded, quoted ones kept. Schema
// is empty when the query relies on the search_path.
type QualifiedName struct {
	Schema string
	Name   string
}

// String renders the name for messages: "schema.name" or "name".
func (n QualifiedName) String() string {
	if n.Schema == "" {
		return n.Name
	}
	return n.Schema + "." + n.Name
}

// ReadQuery is everything a read statement could invoke when executed, as
// the parser sees it. Executing it (EXPLAIN ANALYZE) is safe only when the
// catalog proves every entry harmless; the parser cannot see inside views,
// so Relations must be resolved too. All name lists are de-duplicated and
// sorted.
type ReadQuery struct {
	// Functions are called functions, including aggregates and
	// set-returning functions in FROM.
	Functions []QualifiedName
	// AttributeCalls are x.name references: PostgreSQL treats x.name as
	// name(x) when x has no column called name.
	AttributeCalls []QualifiedName
	// Operators are operator names, including those implied by IN,
	// BETWEEN, LIKE, simple CASE, GREATEST/LEAST and ANY/ALL sublinks.
	Operators []QualifiedName
	// Types are explicit cast targets (element type for arrays).
	Types []QualifiedName
	// Relations are FROM-clause relation names. A reference to a CTE also
	// appears here; CTENames lets callers tell the two apart.
	Relations []QualifiedName
	// CTENames are the names of all common table expressions.
	CTENames []string
	// LockingClause is set for FOR UPDATE/NO KEY UPDATE/SHARE/KEY SHARE.
	LockingClause bool
	// ModifiesData is set when INSERT, UPDATE, DELETE or MERGE appears
	// anywhere (a data-modifying CTE).
	ModifiesData bool
	// SelectInto is set for SELECT ... INTO, which creates a table.
	SelectInto bool
}
