package helpers

import "testing"

// Must fails the test that calls it.
func Must(t testing.TB) {
	t.Helper()
	t.Fatal("must")
}

// Soft takes t but can never fail it.
func Soft(t testing.TB) {
	t.Helper()
	t.Log("soft")
}
