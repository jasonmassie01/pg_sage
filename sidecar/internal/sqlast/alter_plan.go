package sqlast

import "errors"

// AlterKind names the ALTER TABLE forms the online-migration planner
// rewrites (migration-02b).
type AlterKind string

const (
	// AlterAddUnique is ADD CONSTRAINT name UNIQUE (col, ...).
	AlterAddUnique AlterKind = "add_unique"
	// AlterSetNotNull is ALTER [COLUMN] col SET NOT NULL.
	AlterSetNotNull AlterKind = "set_not_null"
)

// PlannableAlter is one ALTER TABLE the planner can rewrite, read from
// the parse tree. Every name is folded the way PostgreSQL resolves it.
type PlannableAlter struct {
	Kind AlterKind
	// Schema is "" when the target is unqualified.
	Schema, Table string
	// Constraint, Columns, Deferrable, InitDeferred and Timing are set
	// for AlterAddUnique.
	Constraint string
	Columns    []string
	// Deferrable and InitDeferred are the constraint's semantics as the
	// tree records them.
	Deferrable, InitDeferred bool
	// Timing is the constraint timing as written, in canonical order and
	// with a leading space (" DEFERRABLE INITIALLY DEFERRED"), or "".
	Timing string
	// Column is set for AlterSetNotNull.
	Column string
}

// Refusals ParsePlannableAlter reports; each message ends with what the
// caller should do.
var (
	// ErrNotPlannable is a statement that is not one ALTER TABLE with one
	// ADD CONSTRAINT ... UNIQUE or ALTER COLUMN ... SET NOT NULL clause.
	ErrNotPlannable = errors.New("unsupported migration statement")
	// ErrUnsupportedOption is a clause the rewritten plan cannot carry
	// (INCLUDE, WITH, USING INDEX, NULLS [NOT] DISTINCT, repeated timing).
	ErrUnsupportedOption = errors.New("unsupported option")
	// ErrSyntax is SQL that PostgreSQL's own parser or scanner rejects.
	ErrSyntax = errors.New("PostgreSQL cannot parse the migration SQL")
	// ErrNameTooLong is an identifier longer than 63 bytes, which
	// PostgreSQL would silently truncate.
	ErrNameTooLong = errors.New("identifier too long")
)

// maxNameBytes is NAMEDATALEN-1, the longest name PostgreSQL keeps.
const maxNameBytes = 63

// maxEcho bounds how much refused SQL an error message repeats.
const maxEcho = 60
