package rca

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// rca-01 review: holders tied on xmin age, and an empty holder read.

// No concurrent access test: the ordering and the fallback read one
// snapshot and keep no state.

// An idle writer's XID pins a reader's snapshot at the same age; the
// query orders the tie by pid, so the reader (lower pid) comes first.
// The writer, whose transaction began first, is named and terminated.
func TestVacuumBlockedHolder_TieNamesTheOlderTransaction(t *testing.T) {
	idleTx := "idle in transaction"
	inc := blockedHolder(t, []probes.XminHolder{
		sessionAt(801, 9000, 20*time.Minute, idleTx),
		sessionAt(802, 9000, time.Hour, idleTx),
	}, sessionLock(801, 20*time.Minute, 20*time.Minute, idleTx),
		sessionLock(802, time.Hour, time.Hour, idleTx))
	if b := linkBlocker(t, inc); b == nil || b.PID != 802 {
		t.Fatalf("blocker = %+v (%q), want the older transaction 802", b, inc.RootCause)
	}
	if inc.RecommendedSQL != "SELECT pg_terminate_backend(802); -- idle-in-tx xmin holder" {
		t.Fatalf("sql %q, want the terminate of 802, never the reader 801", inc.RecommendedSQL)
	}
}

// A session whose snapshot is pinned at a prepared transaction's XID ties
// with it; the prepared transaction owns the XID and is named even when
// the session's transaction began before the PREPARE.
func TestVacuumBlockedHolder_TiePrefersThePreparedXact(t *testing.T) {
	snap := holderSnapshot([]probes.XminHolder{
		sessionAt(811, 7000, 3*time.Hour, "idle in transaction"),
		preparedHolder("pinned", 7000, 2*time.Hour),
	}, sessionLock(811, 3*time.Hour, 3*time.Hour, "idle in transaction"))
	snap.PreparedXacts = []collector.PreparedTransaction{
		preparedXact("pinned", 7000, 2*time.Hour)}
	inc := newTestEngine().treeVacuumBlocked(holderSignal(), snap, testVacuumRules())
	if inc.RecommendedSQL != "ROLLBACK PREPARED 'pinned';" {
		t.Fatalf("sql %q (%q), want the prepared transaction named", inc.RecommendedSQL,
			inc.RootCause)
	}
	if b := linkBlocker(t, inc); b == nil || b.HolderKind != probes.HolderPreparedXact {
		t.Fatalf("blocker = %+v, want the prepared transaction", b)
	}
}

// Ages that differ still win over the tie-break: the older xmin is named
// although its transaction is younger.
func TestVacuumBlockedHolder_TieBreakOnlyOnEqualAges(t *testing.T) {
	inc := blockedHolder(t, []probes.XminHolder{
		sessionAt(821, 8000, 2*time.Hour, "active"),
		sessionAt(822, 9000, 10*time.Minute, "active"),
	})
	if b := linkBlocker(t, inc); b == nil || b.PID != 822 {
		t.Fatalf("blocker = %+v, want 822 (xmin age 9000)", b)
	}
}

// A successful holder read with no holders means nothing holds the
// horizon: a lock-graph session with a snapshot (a lazy VACUUM, which
// the holder query leaves out) is not named. A missing read (nil) still
// falls back to the lock rows.
func TestVacuumBlockedHolder_EmptyReadIgnoresLockRows(t *testing.T) {
	age := int64(5000)
	vacuum := sessionLock(831, time.Hour, time.Hour, "active")
	vacuum.BackendXminAge = &age
	assertBranch3(t, blockedHolder(t, []probes.XminHolder{}, vacuum))
	inc := blockedHolder(t, nil, vacuum)
	if b := linkBlocker(t, inc); b == nil || b.PID != 831 {
		t.Fatalf("blocker = %+v (%q), want the lock-row fallback when the read is missing",
			b, inc.RootCause)
	}
	assertNoSignal(t, inc)
}
