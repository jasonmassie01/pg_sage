//go:build cgo

package decide

import "testing"

// Invalid input (migration-04): SQL the parser rejects is never additive,
// even when its text looks like an allowlisted CREATE TABLE or VIEW.
func TestClassifyDDLParseErrorIsLocking(t *testing.T) {
	requireClass(t, CapDDLLocking,
		"CREATE TABLE app.t (id int",
		"CREATE TABLE app.t (id int))",
		"CREATE VIEW app.v AS SELECT FROM WHERE",
		"ALTER TABLE app.t ADD COLUMN c int int",
	)
	// The same shapes, well formed, stay additive.
	requireClass(t, CapDDLAdditive,
		"CREATE TABLE app.t (id int)",
		"CREATE VIEW app.v AS SELECT 1 AS one",
		"ALTER TABLE app.t ADD COLUMN c int",
	)
}
