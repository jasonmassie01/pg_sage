// Package sqlast validates executor SQL by its PostgreSQL parse tree
// (libpg_query via pg_query_go), as a second layer after the text
// validator. Structure the text rules cannot see (quoted or
// Unicode-escaped identifiers, comments, set operations, several ALTER
// TABLE subcommands) is decided by the real parser.
//
// The parser needs cgo. Release binaries, Docker images and CI build with
// cgo; a build without it reports Available() == false and Check accepts
// everything, leaving the text validator alone in charge.
package sqlast

import "errors"

// ErrRejected wraps every structural refusal.
var ErrRejected = errors.New("SQL rejected by parse-tree validation")

// Rules carries the executor's allowlists so this package does not import
// the executor.
type Rules struct {
	// SystemParam reports whether ALTER SYSTEM SET/RESET may name the GUC.
	SystemParam func(name string) bool
	// DatabaseParam reports whether ALTER DATABASE ... SET may name the GUC.
	DatabaseParam func(name string) bool
	// ProtectedSchema reports whether a schema is off limits.
	ProtectedSchema func(schema string) bool
	// Reloption reports whether ALTER TABLE ... SET/RESET may name the
	// storage parameter (key carries a "toast." namespace; value is "" for
	// RESET and "true" for a bare option). Nil allows every parameter.
	Reloption func(key, value string, reset bool) bool
}
