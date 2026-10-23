package sqlast

// StatementKind is the top-level command of a referenced statement.
type StatementKind string

// The statement kinds References reports.
const (
	StmtSelect StatementKind = "select"
	StmtInsert StatementKind = "insert"
	StmtUpdate StatementKind = "update"
	StmtDelete StatementKind = "delete"
	StmtMerge  StatementKind = "merge"
	StmtOther  StatementKind = "other"
)

// LockMode is a table-level lock a statement takes implicitly.
type LockMode string

// The implied table lock modes, weakest first. LockNone is reported for
// utility statements and statements that read no relation.
const (
	LockNone         LockMode = ""
	LockAccessShare  LockMode = "ACCESS SHARE"
	LockRowShare     LockMode = "ROW SHARE"
	LockRowExclusive LockMode = "ROW EXCLUSIVE"
)

// Refs is what one statement reads and writes, as the parser sees it. The
// parser has no catalog: an unqualified column is attributed only when one
// relation in scope can own it (the innermost query level that has a
// candidate wins), and listed in Ambiguous otherwise. Every list is
// de-duplicated and sorted.
type Refs struct {
	Kind StatementKind `json:"kind"`
	// LockMode is the strongest mode in Locks.
	LockMode LockMode `json:"lock_mode,omitempty"`
	// Target is the written relation of INSERT, UPDATE, DELETE and MERGE.
	Target *QualifiedName `json:"target,omitempty"`
	// Relations are the base relations named anywhere (CTE references are
	// not relations; a schema-qualified name never refers to a CTE).
	Relations []QualifiedName `json:"relations,omitempty"`
	// Locks gives each relation its implied mode: ROW EXCLUSIVE for a write
	// target (also inside a data-modifying CTE), ROW SHARE for FOR UPDATE,
	// NO KEY UPDATE, SHARE and KEY SHARE, ACCESS SHARE otherwise. Empty for
	// utility statements.
	Locks []RelationLock `json:"locks,omitempty"`
	// Columns are the (relation, column) pairs resolved through aliases,
	// subqueries and CTEs.
	Columns []ColumnPair `json:"columns,omitempty"`
	// Star are relations read with * or alias.* (including RETURNING *).
	Star []QualifiedName `json:"star,omitempty"`
	// Ambiguous are unqualified columns more than one relation could own.
	Ambiguous []AmbiguousColumn `json:"ambiguous,omitempty"`
	// Unresolved are column references no relation in scope can own, as
	// written ("z.id", "x").
	Unresolved []string `json:"unresolved,omitempty"`
	CTENames   []string `json:"cte_names,omitempty"`
	// ModifiesData is set for INSERT, UPDATE, DELETE and MERGE, at the top
	// level or in a CTE.
	ModifiesData bool `json:"modifies_data,omitempty"`
}

// RelationLock is the implied lock on one relation.
type RelationLock struct {
	Relation QualifiedName `json:"relation"`
	Mode     LockMode      `json:"mode"`
}

// ColumnPair is a column of a base relation.
type ColumnPair struct {
	Relation QualifiedName `json:"relation"`
	Column   string        `json:"column"`
}

// AmbiguousColumn is an unqualified column with several possible owners:
// base relations by name, subqueries, CTEs and functions by alias.
type AmbiguousColumn struct {
	Column     string          `json:"column"`
	Candidates []QualifiedName `json:"candidates"`
}
