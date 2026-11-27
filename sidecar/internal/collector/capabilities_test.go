package collector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/config"
)

// collection-02: the capability key probe and the column probe, against a
// scripted querier. The live-server behaviour is in capabilities_db_test.go.

// fakeProbeDB answers the two capability probes and counts them.
type fakeProbeDB struct {
	mu       sync.Mutex
	key      []any // server_version_num, view oid, extversion, schema
	keyErr   error
	cols     []string
	colErr   error
	keyCalls int
	colCalls int
}

func newFakeProbeDB(version int, oid uint32, ext string, cols ...string) *fakeProbeDB {
	return &fakeProbeDB{key: []any{version, oid, ext, "public"}, cols: cols}
}

var allStatementColumns = []string{"wal_bytes", "total_plan_time", "shared_blk_read_time"}

func (f *fakeProbeDB) set(version int, oid uint32, ext string, cols ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.key, f.cols = []any{version, oid, ext, "public"}, cols
}

func (f *fakeProbeDB) fail(keyErr, colErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyErr, f.colErr = keyErr, colErr
}

func (f *fakeProbeDB) calls() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keyCalls, f.colCalls
}

func (f *fakeProbeDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(sql, "capabilities: key") {
		return fakeRow{err: fmt.Errorf("fake: unexpected row read %q", sql)}
	}
	f.keyCalls++
	if f.keyErr != nil {
		return fakeRow{err: f.keyErr}
	}
	return fakeRow{vals: f.key}
}

func (f *fakeProbeDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(sql, "capabilities: columns") {
		return nil, fmt.Errorf("fake: unexpected read %q", sql)
	}
	if len(args) != 1 || args[0] != f.key[1] {
		return nil, fmt.Errorf("fake: column probe args %v, want the view oid %v",
			args, f.key[1])
	}
	f.colCalls++
	if f.colErr != nil {
		return nil, f.colErr
	}
	rows := make([][]any, 0, len(f.cols))
	for _, c := range f.cols {
		rows = append(rows, []any{c})
	}
	return &fakeRows{rows: rows}, nil
}

func capsConfig(hasWAL, hasPlan bool) *config.Config {
	cfg := testConfig()
	cfg.HasWALColumns, cfg.HasPlanTimeColumns = hasWAL, hasPlan
	return cfg
}

func probedCollector(db *fakeProbeDB, cfg *config.Config, version int,
	logs *logRecorder) *Collector {
	c := New(nil, cfg, version, logs.log)
	c.probeDB = db
	return c
}

func (l *logRecorder) count(level, sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.HasPrefix(s, level) && strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// The startup flags only seed the capabilities.
func TestCapabilities_SeededFromStartupFlags(t *testing.T) {
	c := New(nil, capsConfig(true, false), 160005, noopLog)
	got := c.Capabilities()
	want := Capabilities{PGVersionNum: 160005, StatStatementsView: true, WALColumns: true,
		blk: blockTimeExprs{read: "0", write: "0"}}
	if got != want {
		t.Fatalf("seeded capabilities = %+v, want %+v", got, want)
	}
	// Zero value: nothing detected at startup.
	got = New(nil, capsConfig(false, false), 0, noopLog).Capabilities()
	if got != (Capabilities{blk: zeroBlockTime}) {
		t.Fatalf("degraded seed = %+v, want nothing", got)
	}
}

// CHECK-22 (unit): an unchanged key costs one statement per refresh; the
// columns are probed once and derived from what the view exposes.
func TestRefreshCapabilities_SteadyStateIsOneStatement(t *testing.T) {
	db := newFakeProbeDB(170002, 4242, "1.11", allStatementColumns...)
	logs := &logRecorder{}
	c := probedCollector(db, capsConfig(false, false), 170002, logs)
	for i := 1; i <= 3; i++ {
		c.refreshCapabilities(context.Background())
		if key, col := db.calls(); key != i || col != 1 {
			t.Fatalf("refresh %d: %d key and %d column probes, want %d and 1", i, key, col, i)
		}
	}
	got := c.Capabilities()
	want := Capabilities{PGVersionNum: 170002, StatStatementsView: true,
		StatStatementsVersion: "1.11", WALColumns: true, PlanTimeColumns: true,
		BlockIOTime: true, blk: blockTimeExprs{
			read:  "shared_blk_read_time + local_blk_read_time",
			write: "shared_blk_write_time + local_blk_write_time"}}
	if got != want {
		t.Fatalf("capabilities = %+v, want %+v", got, want)
	}
	if n := logs.count("INFO", "now collected"); n != 1 {
		t.Errorf("%d INFO lines naming the columns, want 1; logs %v", n, logs.lines)
	}
}

// Nil/empty: a column probe that found nothing is not cached; the next
// refresh probes again.
func TestRefreshCapabilities_EmptyColumnProbeNotCached(t *testing.T) {
	db := newFakeProbeDB(170002, 4242, "1.11")
	c := probedCollector(db, capsConfig(false, false), 170002, &logRecorder{})
	c.refreshCapabilities(context.Background())
	c.refreshCapabilities(context.Background())
	if _, col := db.calls(); col != 2 {
		t.Fatalf("%d column probes after an empty one, want 2 (not cached)", col)
	}
	if got := c.Capabilities(); got.WALColumns || got.PlanTimeColumns || got.BlockIOTime ||
		got.blk != (blockTimeExprs{read: "0", write: "0"}) {
		t.Fatalf("empty probe capabilities = %+v, want none", got)
	}
}

// No view: no column probe at all, nothing cached, the plain read stays.
func TestRefreshCapabilities_AbsentViewSkipsColumnProbe(t *testing.T) {
	db := newFakeProbeDB(170002, 0, "")
	c := probedCollector(db, capsConfig(true, true), 170002, &logRecorder{})
	c.refreshCapabilities(context.Background())
	c.refreshCapabilities(context.Background())
	if key, col := db.calls(); key != 2 || col != 0 {
		t.Fatalf("%d key and %d column probes without the view, want 2 and 0", key, col)
	}
	if got := c.Capabilities(); got.StatStatementsView || got.WALColumns ||
		got.PlanTimeColumns || got.BlockIOTime {
		t.Fatalf("absent view capabilities = %+v, want none", got)
	}
	if tpl := queryStatsTemplate(c.Capabilities().WALColumns,
		c.Capabilities().PlanTimeColumns); tpl != queryStatsSQL {
		t.Fatal("absent view does not use the plain statements read")
	}
}

// State transitions: absent -> created -> dropped -> re-created (new oid)
// -> upgraded extension. Every key change re-probes and logs once.
func TestRefreshCapabilities_KeyChangesReprobe(t *testing.T) {
	db := newFakeProbeDB(160004, 0, "")
	logs := &logRecorder{}
	c := probedCollector(db, capsConfig(false, false), 160004, logs)
	ctx := context.Background()
	c.refreshCapabilities(ctx)
	if n := len(logs.lines); n != 0 {
		t.Fatalf("first probe matching the seed logged %v, want nothing", logs.lines)
	}
	db.set(160004, 5001, "1.10", "wal_bytes", "total_plan_time", "blk_read_time")
	c.refreshCapabilities(ctx)
	got := c.Capabilities()
	if !got.WALColumns || !got.PlanTimeColumns || got.blk.read != "blk_read_time" {
		t.Fatalf("after CREATE EXTENSION = %+v, want WAL, plan, blk_read_time", got)
	}
	if !logs.has("INFO", "pg_stat_statements 1.10 detected") {
		t.Fatalf("no INFO for the created extension; logs %v", logs.lines)
	}
	db.set(160004, 0, "")
	c.refreshCapabilities(ctx)
	if got := c.Capabilities(); got.StatStatementsView || got.WALColumns {
		t.Fatalf("after DROP EXTENSION = %+v, want none", got)
	}
	if !logs.has("WARN", "pg_stat_statements not found") {
		t.Errorf("no WARN for the dropped extension; logs %v", logs.lines)
	}
	db.set(160004, 5002, "1.10", "wal_bytes", "total_plan_time", "blk_read_time")
	c.refreshCapabilities(ctx)
	db.set(160004, 5002, "1.11", allStatementColumns...)
	c.refreshCapabilities(ctx)
	c.refreshCapabilities(ctx)
	if _, col := db.calls(); col != 3 {
		t.Fatalf("%d column probes over three key changes, want 3", col)
	}
	if got := c.Capabilities(); got.StatStatementsVersion != "1.11" ||
		!strings.HasPrefix(got.blk.read, "shared_blk_read_time") {
		t.Fatalf("after ALTER EXTENSION UPDATE = %+v", got)
	}
}

// Boundary: a major upgrade behind the same endpoint re-derives the
// version, and with it the pg_stat_io gate (16) and the system SQL (17).
func TestRefreshCapabilities_MajorUpgradeMovesVersionGates(t *testing.T) {
	db := newFakeProbeDB(150007, 6001, "1.10", "wal_bytes", "total_plan_time",
		"blk_read_time")
	logs := &logRecorder{}
	c := probedCollector(db, capsConfig(true, true), 150007, logs)
	c.WithoutConfigSnapshots()
	ctx := context.Background()
	c.refreshCapabilities(ctx)
	if hasStep(c.extraSteps(), "io") {
		t.Fatal("io read below PG16")
	}
	db.set(170002, 6001, "1.11", allStatementColumns...)
	c.refreshCapabilities(ctx)
	if v := c.Capabilities().PGVersionNum; v != 170002 {
		t.Fatalf("version after upgrade = %d, want 170002", v)
	}
	if !hasStep(c.extraSteps(), "io") {
		t.Fatal("io not read after the upgrade to PG17")
	}
	if !logs.has("INFO", "server 170002") {
		t.Errorf("no INFO naming the new server version; logs %v", logs.lines)
	}
}

func hasStep(steps []catalogStep, name string) bool {
	for _, s := range steps {
		if s.name == name {
			return true
		}
	}
	return false
}

// Error propagation: a failing probe keeps the last capabilities and warns
// once per failure streak; recovery logs once and a new streak warns again.
func TestRefreshCapabilities_ProbeFailureKeepsLastAndWarnsOnce(t *testing.T) {
	db := newFakeProbeDB(170002, 4242, "1.11", allStatementColumns...)
	logs := &logRecorder{}
	c := probedCollector(db, capsConfig(false, false), 170002, logs)
	ctx := context.Background()
	c.refreshCapabilities(ctx)
	before := c.Capabilities()
	db.fail(stmtTimeout(), nil)
	for i := 0; i < 3; i++ {
		c.refreshCapabilities(ctx)
	}
	if got := c.Capabilities(); got != before {
		t.Fatalf("capabilities changed on a failed probe: %+v, want %+v", got, before)
	}
	if n := logs.count("WARN", "capability probe"); n != 1 {
		t.Fatalf("%d WARN lines over three failing probes, want 1; logs %v", n, logs.lines)
	}
	if !logs.has("WARN", "57014") {
		t.Errorf("WARN does not carry the cause; logs %v", logs.lines)
	}
	db.fail(nil, nil)
	c.refreshCapabilities(ctx)
	if !logs.has("INFO", "capability probe recovered") {
		t.Errorf("no INFO on recovery; logs %v", logs.lines)
	}
	db.fail(stmtTimeout(), nil)
	c.refreshCapabilities(ctx)
	if n := logs.count("WARN", "capability probe"); n != 2 {
		t.Fatalf("%d WARN lines after a second streak, want 2", n)
	}
}

// A failed column probe after a key change keeps the last capabilities and
// retries the columns next refresh.
func TestRefreshCapabilities_ColumnProbeFailureRetried(t *testing.T) {
	db := newFakeProbeDB(170002, 4242, "1.11")
	db.fail(nil, stmtTimeout())
	logs := &logRecorder{}
	c := probedCollector(db, capsConfig(true, true), 160000, logs)
	seed := c.Capabilities()
	c.refreshCapabilities(context.Background())
	if got := c.Capabilities(); got != seed {
		t.Fatalf("capabilities after a failed column probe = %+v, want the seed %+v", got, seed)
	}
	db.fail(nil, nil)
	db.set(170002, 4242, "1.11", allStatementColumns...)
	c.refreshCapabilities(context.Background())
	if _, col := db.calls(); col != 2 {
		t.Fatalf("%d column probes, want 2 (failed one retried)", col)
	}
	if got := c.Capabilities(); got.PGVersionNum != 170002 || !got.BlockIOTime {
		t.Fatalf("capabilities after the retry = %+v", got)
	}
}

// Invalid input: an unknown block-time column set reads zero, never an
// expression the view cannot answer.
func TestBlockTimeFor(t *testing.T) {
	for _, tc := range []struct {
		cols []string
		want string
	}{
		{nil, "0"},
		{[]string{"wal_bytes"}, "0"},
		{[]string{"blk_read_time"}, "blk_read_time"},
		{[]string{"blk_read_time", "shared_blk_read_time"},
			"shared_blk_read_time + local_blk_read_time"},
	} {
		set := map[string]bool{}
		for _, c := range tc.cols {
			set[c] = true
		}
		if got := blockTimeFor(set); got.read != tc.want {
			t.Errorf("blockTimeFor(%v).read = %q, want %q", tc.cols, got.read, tc.want)
		}
	}
}

// Concurrency: the API goroutine reads Capabilities while the collector
// re-derives them; every read is one consistent derivation (run with
// -race where cgo is available).
func TestCapabilities_ReadWhileRederiving(t *testing.T) {
	db := newFakeProbeDB(160004, 7001, "1.10")
	c := probedCollector(db, capsConfig(false, false), 160004, &logRecorder{})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var torn []string
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got := c.Capabilities()
			if (got.PGVersionNum == 170002) != got.WALColumns {
				torn = append(torn, fmt.Sprintf("%+v", got))
			}
		}
	}()
	for i := 0; i < 200; i++ {
		if i%2 == 0 {
			db.set(170002, 7002, "1.11", allStatementColumns...)
		} else {
			db.set(160004, 7001, "1.10")
		}
		c.refreshCapabilities(context.Background())
	}
	close(stop)
	wg.Wait()
	if len(torn) > 0 {
		t.Fatalf("%d torn reads, first %s", len(torn), torn[0])
	}
}

// Error propagation through a cycle: the key probe times out on three
// cycles; the snapshots are kept, the capabilities unchanged, one WARN.
func TestCollect_CapabilityProbeTimeoutKeepsSnapshot(t *testing.T) {
	pool := testPool(t)
	logs := &logRecorder{}
	c := New(pool, testConfig(), serverVersion(t, pool), logs.log)
	db := newFakeProbeDB(0, 0, "")
	db.fail(stmtTimeout(), nil)
	c.probeDB = db
	seed := c.Capabilities()
	for i := 0; i < 3; i++ {
		snap, err := c.collect(context.Background())
		if err != nil || snap == nil {
			t.Fatalf("cycle %d with a failing probe: snap %v, err %v", i, snap != nil, err)
		}
		if snap.System.MaxConnections <= 0 {
			t.Fatalf("cycle %d system stats missing: %+v", i, snap.System)
		}
	}
	if got := c.Capabilities(); got != seed {
		t.Fatalf("capabilities = %+v, want the seed %+v", got, seed)
	}
	if n := logs.count("WARN", "capability probe"); n != 1 {
		t.Fatalf("%d WARN lines over three cycles, want 1; logs %v", n, logs.lines)
	}
}

// A system read that hits the other major version's SQL falls back and
// forces the next refresh to re-derive everything (an upgrade between the
// key probe and the system read).
func TestCollectSystem_FallbackForcesReprobe(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	version := serverVersion(t, pool)
	c := New(pool, testConfig(), version, noopLog)
	counter := &probeCounter{}
	c.onCatalogQuery = counter.hook
	c.refreshCapabilities(ctx)
	stale := 160000
	if version < 170000 {
		stale = 170000
	}
	c.mu.Lock()
	c.caps.caps.PGVersionNum = stale
	c.mu.Unlock()
	if _, err := c.collectSystem(ctx); err != nil {
		t.Fatalf("system read with stale version %d on %d: %v", stale, version, err)
	}
	c.refreshCapabilities(ctx)
	if _, col := counter.counts(); col != 2 {
		t.Fatalf("%d column probes, want 2 (fallback forces a re-probe)", col)
	}
	if v := c.Capabilities().PGVersionNum; v != version {
		t.Fatalf("version after the re-probe = %d, want %d", v, version)
	}
}

// A statements read after a failed refresh in the same cycle uses the last
// capabilities instead of probing again: a probe that times out must not
// cost two timeouts per cycle before its first success.
func TestQueryCapabilities_NoSecondProbeAfterFailedRefresh(t *testing.T) {
	db := newFakeProbeDB(170002, 4242, "1.11", allStatementColumns...)
	db.fail(stmtTimeout(), nil)
	c := probedCollector(db, capsConfig(true, false), 160000, &logRecorder{})
	seed := c.Capabilities()
	c.refreshCapabilities(context.Background())
	if got := c.queryCapabilities(context.Background()); got != seed {
		t.Fatalf("capabilities after a failed refresh = %+v, want the seed %+v", got, seed)
	}
	if key, _ := db.calls(); key != 1 {
		t.Fatalf("%d key probes, want 1 (no second probe in the cycle)", key)
	}
	// A direct read with no refresh yet still probes once.
	direct := probedCollector(newFakeProbeDB(170002, 4242, "1.11",
		allStatementColumns...), capsConfig(false, false), 160000, &logRecorder{})
	if got := direct.queryCapabilities(context.Background()); !got.WALColumns ||
		got.PGVersionNum != 170002 {
		t.Fatalf("direct read capabilities = %+v, want the probed ones", got)
	}
}
