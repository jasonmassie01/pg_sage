package runway

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// guard-02a review: the multixact member finding is a cluster finding, so
// a database that no longer reports the cluster (fleet ownership moved,
// or the copies of a solo run before the upgrade) resolves its copy once
// the owner has reported, and leaves it open while the owner has not. A
// measured reading without a rate holds an open finding. The oldest
// multixact is the oldest database's datminmxid.

// mxBootstrappedPool is a pool on a fresh bootstrapped database of the
// test server, so cluster-wide updates touch no other test's findings.
func mxBootstrappedPool(t *testing.T, label string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	return livePoolOn(t, testdb.CreateBootstrappedDatabase(t, label))
}

// openMXCopy stores an open warning of the member runway, as an earlier
// owner (or a solo run) left it.
func openMXCopy(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	e := measuredAt(time.Now(), 5000, MXMemberSafeThreshold-1e9, 1000)
	r := mxRunway(e, 1000, true, defaultMXLimits)
	if r.Severity != SeverityWarning {
		t.Fatalf("fixture runway %+v, want a warning", r)
	}
	f := mxFinding(e, r, defaultMXLimits, clusterScope{})
	if _, err := analyzer.UpsertFindingsWithResult(ctx, pool,
		[]analyzer.Finding{f}); err != nil {
		t.Fatalf("store the copy: %v", err)
	}
}

func TestMXMembers_DB_NonOwnerResolvesItsCopyOnceTheOwnerReports(t *testing.T) {
	pool, ctx := mxBootstrappedPool(t, "mx_dup")
	openMXCopy(t, ctx, pool)
	m := mxMonitor(t, pool, "mx_dup_b")
	owners := NewClusterOwners()
	m.opts.Owners = owners
	const key = "sys/1"
	owners.Claim(key, "mx_dup_a", m.ownerTTL())
	scope := clusterScope{key: key, owner: "mx_dup_a", shared: true}
	// The owner has not reported yet: the copy stays open (fail closed).
	if n, err := m.resolveDuplicates(ctx, scope); err != nil || n != 0 {
		t.Fatalf("before the owner reports: resolved %d, %v; want 0", n, err)
	}
	if got := mxFindingRows(t, ctx, pool); fmt.Sprint(got) != "map[open:warning:1]" {
		t.Fatalf("before the owner reports: findings %v, want the copy open", got)
	}
	m.mxRead = (&scriptedMX{list: []MXMemberEstimate{measuredAt(time.Now(), 10, 0, 0)}}).read
	if res, err := m.tickMXMembers(ctx, scope); err != nil || !res.Recorded ||
		res.Opened || res.Resolved {
		t.Fatalf("a non-owner tick = %+v, %v; want a sample and no report", res, err)
	}
	if got := mxFindingRows(t, ctx, pool); fmt.Sprint(got) != "map[open:warning:1]" {
		t.Fatalf("a non-owner's clear reading resolved its copy: findings %v", got)
	}
	owners.Evaluated(key, "mx_dup_a")
	n, err := m.resolveDuplicates(ctx, scope)
	if err != nil || n != 1 {
		t.Fatalf("after the owner reports: resolved %d, %v; want the copy", n, err)
	}
	if got := mxFindingRows(t, ctx, pool); fmt.Sprint(got) != "map[resolved:warning:1]" {
		t.Fatalf("after the owner reports: findings %v, want the copy resolved", got)
	}
	var owner string
	if err := pool.QueryRow(ctx, `SELECT detail ->> 'cluster_owner' FROM sage.findings
		WHERE category = $1`, CategoryMXMembers).Scan(&owner); err != nil ||
		owner != "mx_dup_a" {
		t.Fatalf("resolved copy names owner %q (%v), want mx_dup_a", owner, err)
	}
}

// TestMXMembers_DB_NoRateHoldsAnOpenFinding: after a warning, a reading
// whose offset went back (a restore, pg_resetwal) has no rate, so its
// runway is unknown: the warning is held, not resolved, even though the
// reading itself shows no members.
func TestMXMembers_DB_NoRateHoldsAnOpenFinding(t *testing.T) {
	pool, ctx := livePool(t)
	clearMXFindings(t, ctx, pool)
	m := mxMonitor(t, pool, fmt.Sprintf("mx_hold_%d", time.Now().UnixNano()))
	t0 := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	near := MXMemberSafeThreshold - 1000*86400*10
	m.mxRead = (&scriptedMX{list: []MXMemberEstimate{
		measuredAt(t0, 1000, near, 2000),
		measuredAt(t0.Add(time.Hour), 1000+3600000, near, 2000),
		measuredAt(t0.Add(2*time.Hour), 500, 0, 0),
	}}).read
	var last *MXMemberResult
	for i := 0; i < 3; i++ {
		res, err := m.tickMXMembers(ctx, clusterScope{})
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		last = res
	}
	if got := mxFindingRows(t, ctx, pool); fmt.Sprint(got) != "map[open:warning:1]" {
		t.Fatalf("after the reset: findings %v, want the warning held open", got)
	}
	if !last.Recorded || last.Resolved || last.Opened || !math.IsNaN(last.Runway.RatePerS) {
		t.Fatalf("reset reading = %+v, want recorded without a rate, nothing resolved",
			last)
	}
}

// TestMXMembers_DB_OldestIsTheOldestDatabase: once one database is frozen
// its datminmxid passes the other databases'; the reading still starts at
// the oldest one (the cluster's member space is not freed until every
// database is frozen). On the exclusive server: its CHECKPOINT moves the
// cluster's checkpoint and WAL counters.
func TestMXMembers_DB_OldestIsTheOldestDatabase(t *testing.T) {
	pool, ctx := exclusivePool(t, "CHECKPOINT moves the cluster's checkpoint and WAL counters")
	shareLockFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, "VACUUM (FREEZE)"); err != nil {
		t.Fatalf("freeze this database: %v", err)
	}
	checkpoint(t, ctx, pool)
	var frozen, oldest int64
	if err := pool.QueryRow(ctx, `SELECT datminmxid::text::int8,
		(SELECT min(datminmxid::text::int8) FROM pg_database)
		FROM pg_database WHERE datname = current_database()`).Scan(&frozen,
		&oldest); err != nil {
		t.Fatalf("read datminmxid: %v", err)
	}
	if frozen <= oldest {
		t.Fatalf("the freeze left datminmxid %d, not past the oldest %d", frozen, oldest)
	}
	e, err := measureMXMembers(ctx, pool)
	if err != nil || e.Status != MXMeasured {
		t.Fatalf("reading = %+v, %v; want measured", e, err)
	}
	if int64(e.OldestMXID) != oldest || e.Live != e.NextMXID-e.OldestMXID || e.Live < 50 {
		t.Fatalf("reading %+v, want oldest %d (not this database's %d) and every "+
			"multixact since it live", e, oldest, frozen)
	}
}

// TestMXMembers_DB_NewOwnerHasTheRateAtOnce: a non-owner samples the
// cluster's member space without reporting it, so when it takes over its
// first reading has a rate and opens the finding at once.
func TestMXMembers_DB_NewOwnerHasTheRateAtOnce(t *testing.T) {
	pool, ctx := mxBootstrappedPool(t, "mx_takeover")
	m := mxMonitor(t, pool, "mx_takeover_b")
	t0 := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	near := MXMemberSafeThreshold - 1000*86400*10 // ten days at 1000/s: a warning
	m.mxRead = (&scriptedMX{list: []MXMemberEstimate{
		measuredAt(t0, 1000, near, 2000),
		measuredAt(t0.Add(time.Hour), 1000+3600000, near, 2000),
		measuredAt(t0.Add(2*time.Hour), 1000+7200000, near, 2000),
	}}).read
	other := clusterScope{key: "sys/2", owner: "mx_takeover_a", shared: true}
	for i := 0; i < 2; i++ {
		res, err := m.tickMXMembers(ctx, other)
		if err != nil || !res.Recorded || res.Opened {
			t.Fatalf("non-owner tick %d = %+v, %v; want a sample, no finding", i, res, err)
		}
	}
	if got := mxFindingRows(t, ctx, pool); len(got) != 0 {
		t.Fatalf("a non-owner opened %v", got)
	}
	own := clusterScope{key: "sys/2", owner: "mx_takeover_b", shared: true, isOwner: true}
	res, err := m.tickMXMembers(ctx, own)
	if err != nil || !res.Opened || math.Abs(res.Runway.RatePerS-1000) > 1e-6 {
		t.Fatalf("first owner tick = %+v, %v; want the warning at 1000/s", res, err)
	}
	if got := mxFindingRows(t, ctx, pool); fmt.Sprint(got) != "map[open:warning:1]" {
		t.Fatalf("after the take-over: findings %v, want one open warning", got)
	}
}
