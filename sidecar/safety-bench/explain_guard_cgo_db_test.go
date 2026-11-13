//go:build cgo

package safetybench

import "testing"

// security-10 / AQ-04 under explain_guard: the guard accepts the read, its
// EXPLAIN ANALYZE fails on the cast of people.ssn and the server message
// quotes the value; the design returns it withheld. ANALYZE needs the
// parse-tree inspector, so this runs in cgo builds only.
func TestExplainGuard_AQ04ErrorWithholdsTheValue(t *testing.T) {
	requireWithheldAttempt(t, "SELECT id FROM sb_fixture.people WHERE ssn::int = 0")
}
