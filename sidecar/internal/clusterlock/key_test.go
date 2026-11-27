package clusterlock_test

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/clusterlock"
)

// Key is the text Lock, LockConfig and TryLock hash into the advisory lock
// key, and what a test matches in pg_locks to find a lock's holder. Every
// process that serializes on a name must hash the same text, so the
// namespace is pinned here: changing it would let an old and a new binary
// hold one name at the same time.
//
// No DB, concurrency or state tests: Key is a pure string function. The
// exclusive-server tests in internal/testdb find the exclusive lock's
// holder and waiters in pg_locks through it on a real server.
func TestKeyNamespacesTheLockName(t *testing.T) {
	cases := []struct{ name, want string }{
		{"exclusive", "pg_sage_cluster_fixture:exclusive"},
		{"agent_roles", "pg_sage_cluster_fixture:agent_roles"},
		{"", "pg_sage_cluster_fixture:"},
	}
	for _, c := range cases {
		if got := clusterlock.Key(c.name); got != c.want {
			t.Errorf("Key(%q) = %q, want %q", c.name, got, c.want)
		}
	}
	if clusterlock.Key("a") == clusterlock.Key("b") {
		t.Error("two lock names share one key")
	}
}
