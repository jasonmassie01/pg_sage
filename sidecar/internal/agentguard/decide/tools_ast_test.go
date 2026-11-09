package decide

import "testing"

// migration-04 state pins: the plan's two named forms are locking, and
// so is the empty statement inside a batch (never additive).
func TestClassifyDDLNamedFormsAreLocking(t *testing.T) {
	requireClass(t, CapDDLLocking,
		"ALTER TABLE t SET LOGGED",
		"ALTER TABLE t ADD COLUMN c int REFERENCES p(id)",
		"ALTER TABLE t ADD COLUMN c int; ; SELECT 1",
	)
}
