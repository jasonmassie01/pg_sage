// Package parsetree marks tests that need the libpg_query parse tree.
// Builds without cgo do not link the parser, and the online-migration
// planner refuses to plan there (migration-02b); such a test skips with
// that reason instead of failing.
package parsetree

import (
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// SkipReason is the skip text Require reports.
const SkipReason = "needs the parse-tree build (cgo): without it online-migration " +
	"planning refuses with sqlast.ErrUnavailable"

// Available reports whether this build links the parse-tree parser.
func Available() bool {
	_, err := sqlast.ParsePlannableAlter("")
	return !errors.Is(err, sqlast.ErrUnavailable)
}

// Require skips t when the build has no parse tree.
func Require(t testing.TB) {
	t.Helper()
	if !Available() {
		t.Skip(SkipReason)
	}
}
