package executor

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/jackc/pgx/v5/pgxpool"
)

// observability-04: executor outcomes and verification verdicts are
// counted for /metrics, with bounded labels.

// outcomeCount is the counted value of one (database, class, outcome).
func outcomeCount(database, class, outcome string) int64 {
	for _, c := range OutcomeCounts() {
		if c.Database == database && c.Class == class && c.Outcome == outcome {
			return c.Count
		}
	}
	return 0
}

func verdictCount(database, class, verdict string) int64 {
	for _, c := range VerdictCounts() {
		if c.Database == database && c.Class == class && c.Verdict == verdict {
			return c.Count
		}
	}
	return 0
}

// seriesOf lists the outcome series of database.
func seriesOf(database string) []OutcomeCount {
	var out []OutcomeCount
	for _, c := range OutcomeCounts() {
		if c.Database == database {
			out = append(out, c)
		}
	}
	return out
}

// testDatabase is a database label no other test counts under.
func testDatabase(t *testing.T) string {
	t.Helper()
	name := "obs04-" + strings.ReplaceAll(t.Name(), "/", "-")
	t.Cleanup(func() { PruneCounts(name) })
	return name
}

func TestRecordOutcome_CountsEachTerminalOutcome(t *testing.T) {
	db := testDatabase(t)
	RecordOutcome(db, "index", "success")
	RecordOutcome(db, "index", "success")
	RecordOutcome(db, "index", "rolled_back")
	RecordOutcome(db, "vacuum", "failed")
	if got := outcomeCount(db, "index", "success"); got != 2 {
		t.Fatalf("index success = %d, want 2", got)
	}
	if got := outcomeCount(db, "index", "rolled_back"); got != 1 {
		t.Fatalf("index rolled_back = %d, want 1", got)
	}
	if got := outcomeCount(db, "vacuum", "failed"); got != 1 {
		t.Fatalf("vacuum failed = %d, want 1", got)
	}
	if got := len(seriesOf(db)); got != 3 {
		t.Fatalf("series = %d (%v), want 3", got, seriesOf(db))
	}
}

// The action_log outcomes fold into the bounded set an alert can name.
func TestRecordOutcome_FoldsActionLogOutcomes(t *testing.T) {
	cases := map[string]string{
		"success": "success", "failed": "failed", "partial": "failed",
		"rollback_failed": "failed", "rolled_back": "rolled_back",
		"reverted": "rolled_back", "already_restored": "rolled_back",
		"cancelled": "cancelled", "unverifiable": "unverifiable",
		"verification_expired": "unverifiable", "rollback_skipped": "other",
		"rollback_withheld": "other", "weird": "other",
	}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			db := testDatabase(t)
			RecordOutcome(db, "index", raw)
			if got := outcomeCount(db, "index", want); got != 1 {
				t.Fatalf("outcome %q counted as %v, want one %q", raw, seriesOf(db), want)
			}
		})
	}
}

// Non-terminal states are not outcomes: the action has not ended.
func TestRecordOutcome_SkipsNonTerminalStates(t *testing.T) {
	db := testDatabase(t)
	for _, s := range []string{"pending", "monitoring", "rolling_back", "interrupted",
		"applied_pending_restart", ""} {
		RecordOutcome(db, "index", s)
	}
	if got := seriesOf(db); len(got) != 0 {
		t.Fatalf("non-terminal states were counted: %v", got)
	}
}

func TestRecordOutcome_EmptyDatabaseRecordsNothing(t *testing.T) {
	before := len(OutcomeCounts())
	beforeVerdicts := len(VerdictCounts())
	RecordOutcome("", "index", "failed")
	RecordVerdict("", "index", "revert")
	if got := outcomeCount("", "index", "failed"); got != 0 {
		t.Fatalf("an unnamed database counted %d outcomes", got)
	}
	if len(OutcomeCounts()) != before || len(VerdictCounts()) != beforeVerdicts {
		t.Fatal("an unnamed database added a series")
	}
}

// Classes outside the policy's change classes are "other", so a typo or
// a new action type cannot grow the label set.
func TestRecordOutcome_UnknownClassIsOther(t *testing.T) {
	db := testDatabase(t)
	RecordOutcome(db, "index_build", "success")
	RecordOutcome(db, "", "success")
	if got := outcomeCount(db, "other", "success"); got != 2 {
		t.Fatalf("unknown classes = %v, want two under other", seriesOf(db))
	}
}

// Property: whatever strings arrive, every label is from the bounded sets.
func TestRecordOutcome_LabelSetStaysBounded(t *testing.T) {
	db := testDatabase(t)
	rng := rand.New(rand.NewSource(4))
	for i := 0; i < 2000; i++ {
		RecordOutcome(db, randomLabel(rng), randomLabel(rng))
		RecordVerdict(db, randomLabel(rng), randomLabel(rng))
	}
	for _, c := range OutcomeCounts() {
		if c.Database != db {
			continue
		}
		if !metricsClasses[c.Class] || !outcomeLabels[c.Outcome] {
			t.Fatalf("unbounded series %+v", c)
		}
	}
	for _, c := range VerdictCounts() {
		if c.Database != db {
			continue
		}
		if !metricsClasses[c.Class] || !verdictLabels[c.Verdict] {
			t.Fatalf("unbounded verdict series %+v", c)
		}
	}
	if n := len(seriesOf(db)); n > len(metricsClasses)*len(outcomeLabels) {
		t.Fatalf("%d series exceed the bounded product", n)
	}
}

// randomLabel is a random string, sometimes a valid label.
func randomLabel(rng *rand.Rand) string {
	known := []string{"index", "vacuum", "success", "failed", "revert", "agent_access"}
	if rng.Intn(4) == 0 {
		return known[rng.Intn(len(known))]
	}
	b := make([]byte, rng.Intn(12))
	for i := range b {
		b[i] = byte(rng.Intn(256))
	}
	return string(b)
}

func TestRecordVerdict_CountsAndFolds(t *testing.T) {
	db := testDatabase(t)
	RecordVerdict(db, "index", "revert")
	RecordVerdict(db, "index", "success")
	RecordVerdict(db, "index", "unverifiable")
	RecordVerdict(db, "index", "failed")
	RecordVerdict(db, "index", "maybe")
	for _, v := range []string{"revert", "success", "unverifiable", "failed", "other"} {
		if got := verdictCount(db, "index", v); got != 1 {
			t.Fatalf("verdict %s = %d, want 1 (%v)", v, got, VerdictCounts())
		}
	}
}

// Concurrent recordings give exact totals (run under -race where cgo is
// available).
func TestRecordOutcome_ConcurrentRecordingsAreExact(t *testing.T) {
	dbs := []string{testDatabase(t) + "-a", testDatabase(t) + "-b", testDatabase(t) + "-c"}
	for _, db := range dbs {
		t.Cleanup(func() { PruneCounts(db) })
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				db := dbs[(g+i)%len(dbs)]
				RecordOutcome(db, "index", "failed")
				RecordVerdict(db, "index", "revert")
				_ = OutcomeCounts()
			}
		}(g)
	}
	wg.Wait()
	var total, verdicts int64
	for _, db := range dbs {
		total += outcomeCount(db, "index", "failed")
		verdicts += verdictCount(db, "index", "revert")
	}
	if total != 8000 || verdicts != 8000 {
		t.Fatalf("totals = %d outcomes, %d verdicts; want 8000 each", total, verdicts)
	}
	if got := outcomeCount(dbs[0], "index", "failed"); got != 2667 {
		t.Fatalf("%s = %d, want 2667", dbs[0], got)
	}
}

// State transition: a database removed by a fleet reload drops its
// series; added again it counts from zero.
func TestPruneCounts_DropsOutcomeAndVerdictSeries(t *testing.T) {
	db, other := testDatabase(t), testDatabase(t)+"-kept"
	t.Cleanup(func() { PruneCounts(other) })
	RecordOutcome(db, "index", "failed")
	RecordVerdict(db, "index", "revert")
	RecordOutcome(other, "index", "failed")
	PruneCounts(db)
	if len(seriesOf(db)) != 0 || verdictCount(db, "index", "revert") != 0 {
		t.Fatalf("pruned database still has series: %v", seriesOf(db))
	}
	if outcomeCount(other, "index", "failed") != 1 {
		t.Fatal("pruning one database dropped another's series")
	}
	RecordOutcome(db, "index", "failed")
	if got := outcomeCount(db, "index", "failed"); got != 1 {
		t.Fatalf("re-added database counts %d, want 1 (from zero)", got)
	}
}

func TestOutcomeCounts_AreSorted(t *testing.T) {
	db := testDatabase(t)
	RecordOutcome(db+"-z", "vacuum", "failed")
	t.Cleanup(func() { PruneCounts(db + "-z") })
	RecordOutcome(db, "vacuum", "success")
	RecordOutcome(db, "index", "success")
	RecordOutcome(db, "index", "failed")
	got := OutcomeCounts()
	for i := 1; i < len(got); i++ {
		a, b := got[i-1], got[i]
		ka := fmt.Sprint(a.Database, "\x00", a.Class, "\x00", a.Outcome)
		kb := fmt.Sprint(b.Database, "\x00", b.Class, "\x00", b.Outcome)
		if ka >= kb {
			t.Fatalf("not sorted at %d: %+v before %+v", i, a, b)
		}
	}
}

// The class of an action_log row comes from its action type and, for the
// catch-all "alter", from its statement.
func TestOutcomeClass(t *testing.T) {
	cases := []struct{ actionType, sql, want string }{
		{"create_index", "CREATE INDEX CONCURRENTLY i ON t (a)", "index"},
		{"create_index_concurrently", "", "index"},
		{"drop_index", "", "index"},
		{"reindex", "", "index"},
		{ActionTypeReplaceIndex, "", "index"},
		{"create_statistics", "", "analyze"},
		{"drop_statistics", "", "analyze"},
		{"analyze", "", "analyze"},
		{"vacuum", "", "vacuum"},
		{"cancel_backend", "", "backend_signal"},
		{"retention_delete", "", "retention"},
		{ActionTypeGuardKill, "", "agent_access"},
		{"alter", "ALTER SYSTEM SET work_mem = '8MB'", "config_guc"},
		{"alter", "alter database app set work_mem = '8MB'", "config_guc"},
		{"alter", "ALTER TABLE t SET (autovacuum_vacuum_scale_factor = 0.05)", "autovacuum_tuning"},
		{"alter", "ALTER TABLE t ADD COLUMN c int", "schema_change"},
		{"alter", "", "other"},
		{"ddl", "SELECT 1", "other"},
		{"", "", "other"},
		{ActionTypeRevertAction, "", "other"},
	}
	for _, c := range cases {
		if got := OutcomeClass(c.actionType, c.sql); got != c.want {
			t.Errorf("OutcomeClass(%q, %q) = %q, want %q", c.actionType, c.sql, got, c.want)
		}
		if !metricsClasses[OutcomeClass(c.actionType, c.sql)] {
			t.Errorf("OutcomeClass(%q) is outside the bounded set", c.actionType)
		}
	}
}

// The bounded class set is the policy's 18 change classes plus other.
func TestMetricsClasses_AreThePolicyClassesPlusOther(t *testing.T) {
	if len(metricsClasses) != 19 || !metricsClasses["other"] {
		t.Fatalf("metricsClasses has %d entries, want the 18 policy classes and other",
			len(metricsClasses))
	}
}

// An executor names its pool's database, so a pool-only write path counts
// under it; an unnamed executor's pool counts nothing.
func TestMetricsDatabaseFor_FollowsTheExecutorName(t *testing.T) {
	if got := metricsDatabaseFor(nil); got != "" {
		t.Fatalf("nil pool names %q", got)
	}
	pool := closedPool(t)
	if got := metricsDatabaseFor(pool); got != "" {
		t.Fatalf("an unbound pool names %q", got)
	}
	db := testDatabase(t)
	e := New(pool, resumeExecutorConfig(), time.Time{}, nopLog)
	e.WithDatabaseName(db)
	if got := metricsDatabaseFor(pool); got != db {
		t.Fatalf("bound pool names %q, want %q", got, db)
	}
	PruneCounts(db)
	if got := metricsDatabaseFor(pool); got != "" {
		t.Fatalf("a pruned database's pool still names %q", got)
	}
}

// A reload prepares a replacement runtime beside the running one. Naming
// the replacement's pool must not unbind the running pool: when the
// replacement is discarded the running runtime still counts, and while
// the old one drains after a publish it counts too. Pruning the database
// unbinds both.
func TestMetricsDatabaseFor_AReplacementKeepsTheRunningPool(t *testing.T) {
	db := testDatabase(t)
	running, candidate := closedPool(t), closedPool(t)
	New(running, resumeExecutorConfig(), time.Time{}, nopLog).WithDatabaseName(db)
	New(candidate, resumeExecutorConfig(), time.Time{}, nopLog).WithDatabaseName(db)
	if got := metricsDatabaseFor(running); got != db {
		t.Fatalf("running pool names %q after a replacement was prepared, want %q", got, db)
	}
	if got := metricsDatabaseFor(candidate); got != db {
		t.Fatalf("replacement pool names %q, want %q", got, db)
	}
	PruneCounts(db)
	for name, p := range map[string]*pgxpool.Pool{"running": running, "candidate": candidate} {
		if got := metricsDatabaseFor(p); got != "" {
			t.Fatalf("%s pool of a pruned database still names %q", name, got)
		}
	}
}

// A pool nothing holds any more leaves the bindings once collected, so
// reloads that replace a database's runtime do not grow them.
func TestBindMetricsPool_DropsCollectedPools(t *testing.T) {
	db := testDatabase(t)
	t.Cleanup(func() { PruneCounts(db) })
	gone := func() weak.Pointer[pgxpool.Pool] {
		p := closedPool(t)
		bindMetricsPool(p, db)
		return weak.Make(p)
	}()
	// A closed pool's background goroutines exit shortly after Close.
	for i := 0; i < 200 && gone.Value() != nil; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if gone.Value() != nil {
		t.Fatal("a closed, unreferenced pool was not collected within 2s")
	}
	keep := closedPool(t)
	bindMetricsPool(keep, db)
	metricsPools.mu.RLock()
	_, stale := metricsPools.byPool[gone]
	live := metricsPools.byPool[weak.Make(keep)]
	metricsPools.mu.RUnlock()
	if stale {
		t.Fatal("a collected pool is still bound")
	}
	if live != db {
		t.Fatalf("the live pool names %q, want %q", live, db)
	}
}

// closedPool is a pool that never connected and is closed: every query on
// it fails at once.
func closedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://obs04@127.0.0.1:1/obs04?connect_timeout=1")
	if err != nil {
		t.Fatalf("closed pool: %v", err)
	}
	pool.Close()
	return pool
}
