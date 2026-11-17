package workpack

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// evid-02a on a real server, in both history placements: a 28-day fixture
// rollup builds a pack whose families, shapes and totals match hand-computed
// values, every id resolves, the same window and data give one hash (also
// from a fresh pool), a stored pack is never replaced, and two concurrent
// builds of one window store one row.

var fixFrom = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) // a Monday

const fixHours = 28 * 24

func execHist(t *testing.T, db any, sql string, args ...any) {
	t.Helper()
	if _, err := histstore.Resolve(db).Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed: %v\n%s", err, sql)
	}
}

// seedHourly writes calls/exec for qid at every hour h of the fixture with
// h % every == at.
func seedHourly(t *testing.T, db any, qid int64, calls int64, exec float64, every, at int) {
	t.Helper()
	execHist(t, db, `INSERT INTO sage.query_rollup_hourly (queryid, hour, calls, exec_ms,
		rows, shared_blks_hit, shared_blks_read, temp_blks_read, temp_blks_written,
		wal_bytes, plan_hash, epoch_break, pairs{dbcol})
		SELECT $1::int8, $2::timestamptz + make_interval(hours => h), $3::int8, $4::float8,
		       $3::int8, 0, 0, 0, 0, 0, NULL, false, 12{dbval}
		  FROM generate_series(0, $5::int - 1) h WHERE h % $6::int = $7::int`,
		qid, fixFrom, calls, exec, fixHours, every, at)
}

func seedFixture(t *testing.T, db any) {
	t.Helper()
	execHist(t, db, `INSERT INTO sage.query_rollup_coverage (hour, db_exec_ms,
		sampled_exec_ms, sampled_queryids, epoch_breaks, dealloc_delta, snapshots,
		epoch_source{dbcol})
		SELECT $1::timestamptz + make_interval(hours => h), 200, 150, 4, 0, 0, 12,
		       'recorded'{dbval} FROM generate_series(0, $2::int - 1) h`, fixFrom, fixHours)
	seedHourly(t, db, 1, 10, 20, 1, 0)   // orders, every hour
	seedHourly(t, db, 4, 2, 1.5, 168, 9) // orders, Monday 09:00 UTC
	seedHourly(t, db, 2, 1, 100, 1, 0)   // customers join, every hour
	seedHourly(t, db, 3, 3, 0.25, 2, 0)  // events insert, even hours
	seedHourly(t, db, 5, 1, 1, fixHours, 0)
	last := fixFrom.Add((fixHours - 1) * time.Hour)
	snap := func(at time.Time, category, doc string) {
		execHist(t, db, `INSERT INTO sage.snapshots (collected_at, category, data{dbcol})
			VALUES ($1, $2, $3::jsonb{dbval})`, at, category, doc)
	}
	snap(last, "queries", `[{"queryid":1,"query":"SELECT * FROM orders WHERE id = $1"},
		{"queryid":2,"query":"SELECT * FROM customers c JOIN orders o ON o.c = c.id"},
		{"queryid":3,"query":"INSERT INTO app.events VALUES ($1)"},
		{"queryid":4,"query":"select * from orders where x = $1"}]`)
	snap(last, "tables", `[{"schemaname":"public","relname":"orders","total_bytes":5000},
		{"schemaname":"public","relname":"customers","total_bytes":3000},
		{"schemaname":"app","relname":"events","total_bytes":1000}]`)
	for d := range 28 {
		snap(fixFrom.AddDate(0, 0, d).Add(12*time.Hour), "system",
			`{"db_size_bytes":`+itoa(int64(1_000_000_000+d*1_000_000))+`}`)
	}
}

func itoa(v int64) string { b, _ := json.Marshal(v); return string(b) }

// noise writes another database's rollup with the same queryids into the
// meta database: a reader that forgets its database id sees it.
func noise(t *testing.T, p *histfixture.Pair) {
	t.Helper()
	other, err := histstore.NewMeta(p.Meta, histfixture.OtherDatabaseID)
	if err != nil {
		t.Fatal(err)
	}
	seedHourly(t, other, 1, 99999, 99999, 1, 0)
	p.Noise(t, fixFrom.Add(time.Hour), 1, 2)
}

func clearPacks(t *testing.T, p *histfixture.Pair) {
	t.Helper()
	for _, pool := range []*pgxpool.Pool{p.Monitored, p.Meta} {
		if _, err := pool.Exec(context.Background(), `TRUNCATE sage.query_rollup_hourly,
			sage.query_rollup_daily, sage.query_rollup_coverage, sage.workload_packs`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
	}
}

func fixRequest() Request {
	to := fixFrom.Add(fixHours * time.Hour)
	return Request{Database: "orders_db", Window: Window{fixFrom, to}, Location: time.UTC,
		PGVersion: "17", Platform: "self-managed", HourlyDays: 35, Now: to}
}

func familyByKey(t *testing.T, wl Workload, key string) Family {
	t.Helper()
	for _, fam := range wl.Families {
		if fam.Key == key {
			return fam
		}
	}
	t.Fatalf("no family %q in %d families", key, len(wl.Families))
	return Family{}
}

func checkFixtureWorkload(t *testing.T, wl Workload) {
	t.Helper()
	keys := []string{}
	for _, fam := range wl.Families {
		keys = append(keys, fam.Key)
	}
	want := "select public.customers,public.orders|select public.orders|" +
		"insert app.events|unparsed:5"
	if strings.Join(keys, "|") != want {
		t.Fatalf("families %v, want %s (by exec time)", keys, want)
	}
	join := familyByKey(t, wl, "select public.customers,public.orders")
	orders := familyByKey(t, wl, "select public.orders")
	ins := familyByKey(t, wl, "insert app.events")
	if join.Calls != 672 || join.ExecMs != 67200 || orders.Calls != 6728 ||
		orders.ExecMs != 13446 || ins.Calls != 1008 || ins.ExecMs != 84 {
		t.Fatalf("totals join %d/%v orders %d/%v insert %d/%v", join.Calls, join.ExecMs,
			orders.Calls, orders.ExecMs, ins.Calls, ins.ExecMs)
	}
	if orders.Shape.CallsMean[9] != 12 || orders.Shape.ExecMsMean[9] != 21.5 ||
		orders.Shape.CallsMean[10] != 10 || orders.Shape.Weeks != 4 {
		t.Fatalf("orders shape bucket 9 %v/%v, bucket 10 %v, weeks %d",
			orders.Shape.CallsMean[9], orders.Shape.ExecMsMean[9], orders.Shape.CallsMean[10],
			orders.Shape.Weeks)
	}
	if ins.Shape.CallsMean[0] != 3 || ins.Shape.CallsMean[1] != 0 ||
		ins.Shape.ExecMsMean[0] != 0.25 {
		t.Fatalf("insert shape buckets 0/1 %v/%v", ins.Shape.CallsMean[0],
			ins.Shape.CallsMean[1])
	}
	if len(orders.QueryIDs) != 2 || orders.QueryIDs[0].QueryID != "1" ||
		orders.QueryIDs[1].QueryID != "4" {
		t.Fatalf("orders queryids %+v, want 1 then 4", orders.QueryIDs)
	}
}

func TestBuildDB_FixtureMatchesHandComputedValuesInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	hashes := map[histstore.Mode]string{}
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		clearPacks(t, p)
		seedFixture(t, p.Monitored)
		if mode == histstore.ModeMeta {
			noise(t, p)
		}
		pack, err := Build(ctx, p.Monitored, fixRequest())
		if err != nil {
			t.Fatalf("%s: build: %v", mode, err)
		}
		checkFixtureWorkload(t, workloadOf(t, pack))
		var cov Coverage
		if err := json.Unmarshal(pack.File(fileCoverage), &cov); err != nil ||
			cov.Status != "ok" || cov.Hours != fixHours || cov.CoveredExecShare == nil ||
			*cov.CoveredExecShare != 0.75 {
			t.Fatalf("%s: coverage %s (%v)", mode, pack.File(fileCoverage), err)
		}
		checkFixtureGrowth(t, pack)
		for _, id := range listedIDs(t, pack) {
			if _, err := Resolve(pack, id); err != nil {
				t.Fatalf("%s: %s does not resolve: %v", mode, id, err)
			}
		}
		hashes[mode] = pack.ContentSHA256
	}
	if hashes[histstore.ModeMonitored] != hashes[histstore.ModeMeta] {
		t.Fatalf("placements give different hashes %v", hashes)
	}
}

func checkFixtureGrowth(t *testing.T, pack *Pack) {
	t.Helper()
	var g Growth
	if err := json.Unmarshal(pack.File(fileGrowth), &g); err != nil {
		t.Fatal(err)
	}
	if g.DBSize.Fit.Slope == nil || *g.DBSize.Fit.Slope != 1_000_000 ||
		g.DBSize.Fit.Trend != trendRising || g.DBSize.MissingDays != 0 {
		t.Fatalf("db size growth %+v, want 1e6 bytes/day rising", g.DBSize)
	}
	if len(g.Tables) != 3 || g.Tables[0].ID != "growth/table/public.orders" ||
		g.Tables[0].Fit.Trend != trendUnknown || g.Tables[0].MissingDays != 27 {
		t.Fatalf("tables %+v, want 3 tables, orders first, one day only (unknown)", g.Tables)
	}
	if len(g.Workload) != 4 || g.Workload[1].Calls.Trend != trendFlat {
		t.Fatalf("workload growth %+v, want 4 families with orders flat", g.Workload)
	}
}

func TestBuildDB_SameWindowSameHashAcrossAFreshPool(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	clearPacks(t, p)
	seedFixture(t, p.Monitored)
	ctx := context.Background()
	first, err := Build(ctx, p.Monitored, fixRequest())
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := pgxpool.New(ctx, p.Monitored.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	again, err := Build(ctx, fresh, fixRequest())
	if err != nil || again.ContentSHA256 != first.ContentSHA256 {
		t.Fatalf("rebuild from a new pool: %v, hash %s want %s", err, again.ContentSHA256,
			first.ContentSHA256)
	}
}

func TestBuildDB_BoundaryWindowsBuild(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	clearPacks(t, p)
	seedFixture(t, p.Monitored)
	for _, days := range []int{14, 100} {
		req := fixRequest()
		req.Window.From = req.Window.To.Add(-time.Duration(days) * 24 * time.Hour)
		pack, err := Build(context.Background(), p.Monitored, req)
		if err != nil {
			t.Fatalf("%d days: %v", days, err)
		}
		var hdr Header
		if err := json.Unmarshal(pack.File(fileHeader), &hdr); err != nil {
			t.Fatal(err)
		}
		if days == 100 && (hdr.Days[0].Granularity != granularityDaily || len(hdr.Days) != 100) {
			t.Fatalf("100 days: header days %d first %+v, want daily", len(hdr.Days), hdr.Days[0])
		}
	}
	req := fixRequest()
	req.Window.From = req.Window.To.Add(-14*24*time.Hour + time.Hour)
	if _, err := Build(context.Background(), p.Monitored, req); !errors.Is(err, ErrWindow) {
		t.Fatalf("13 days 23 hours: %v, want ErrWindow", err)
	}
}

func TestBuildDB_UnregisteredMetaPoolIsRefusedAndStoresNothing(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "workpack_unreg")
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := histstore.SetProcessMode(histstore.ModeMeta); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = histstore.SetProcessMode(histstore.ModeMonitored) }()
	_, err = Build(context.Background(), pool, fixRequest())
	if !errors.Is(err, histstore.ErrNotRegistered) {
		t.Fatalf("build on an unregistered pool: %v, want histstore.ErrNotRegistered", err)
	}
	if _, err := List(context.Background(), pool, 10); !errors.Is(err,
		histstore.ErrNotRegistered) {
		t.Fatalf("list on an unregistered pool: %v, want histstore.ErrNotRegistered", err)
	}
}

func TestBuildDB_AHistoryReadFailureNamesTheTableAndWindow(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "workpack_noschema")
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = Build(context.Background(), pool, fixRequest())
	if err == nil || !strings.Contains(err.Error(), "sage.query_rollup_hourly") ||
		!strings.Contains(err.Error(), "2026-10-05T00:00:00Z") {
		t.Fatalf("build without the rollup tables: %v, want the table and the window", err)
	}
}

func TestBuildDB_StoredPacksAreReusedNeverReplaced(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		clearPacks(t, p)
		seedFixture(t, p.Monitored)
		pack, err := Build(ctx, p.Monitored, fixRequest())
		if err != nil {
			t.Fatal(err)
		}
		first, err := Save(ctx, p.Monitored, pack, fixFrom, "op@example.test")
		if err != nil || first.Reused || first.ID == 0 || first.Bytes != int64(len(pack.Body)) {
			t.Fatalf("%s: first save %+v, %v", mode, first, err)
		}
		again, err := Save(ctx, p.Monitored, pack, fixFrom.Add(time.Hour), "other@example.test")
		if err != nil || !again.Reused || again.ID != first.ID ||
			!again.BuiltAt.Equal(first.BuiltAt) || again.BuiltBy != "op@example.test" {
			t.Fatalf("%s: second save %+v, %v, want the first row reused", mode, again, err)
		}
		row, loaded, err := Load(ctx, p.Monitored, pack.ContentSHA256)
		if err != nil || row.ID != first.ID || loaded.ContentSHA256 != pack.ContentSHA256 {
			t.Fatalf("%s: load %+v, %v", mode, row, err)
		}
		checkStateTransition(t, p, pack, first)
	}
}

// checkStateTransition: a new hour inside the window changes the hash and
// stores a second row; the first row is untouched.
func checkStateTransition(t *testing.T, p *histfixture.Pair, pack *Pack, first Stored) {
	t.Helper()
	ctx := context.Background()
	seedHourly(t, p.Monitored, 6, 1, 1, fixHours, 0)
	next, err := Build(ctx, p.Monitored, fixRequest())
	if err != nil || next.ContentSHA256 == pack.ContentSHA256 {
		t.Fatalf("a new queryid hour: %v, hash unchanged %v", err,
			next.ContentSHA256 == pack.ContentSHA256)
	}
	second, err := Save(ctx, p.Monitored, next, fixFrom, "op@example.test")
	if err != nil || second.Reused || second.ID == first.ID {
		t.Fatalf("second pack %+v, %v, want a new row", second, err)
	}
	rows, err := List(ctx, p.Monitored, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list %+v, %v, want both packs", rows, err)
	}
	_, old, err := Load(ctx, p.Monitored, pack.ContentSHA256)
	if err != nil || string(old.Body) != string(pack.Body) {
		t.Fatalf("first pack changed after a second build: %v", err)
	}
	if _, _, err := Load(ctx, p.Monitored, strings.Repeat("a", 64)); !errors.Is(err,
		ErrNotFound) {
		t.Fatalf("load of an unknown hash: %v, want ErrNotFound", err)
	}
}

func TestBuildDB_ConcurrentBuildsOfOneWindowStoreOneRow(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	clearPacks(t, p)
	seedFixture(t, p.Monitored)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make([]Stored, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pack, err := Build(ctx, p.Monitored, fixRequest())
			if err == nil {
				results[i], err = Save(ctx, p.Monitored, pack, fixFrom, "op@example.test")
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	fresh := 0
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("build %d: %v", i, errs[i])
		}
		if !r.Reused {
			fresh++
		}
		if r.ID != results[0].ID {
			t.Fatalf("builds stored rows %d and %d", r.ID, results[0].ID)
		}
	}
	var n int
	if err := p.Monitored.QueryRow(ctx, "SELECT count(*) FROM sage.workload_packs").
		Scan(&n); err != nil || n != 1 || fresh != 1 {
		t.Fatalf("%d rows (%v), %d fresh saves, want one of each", n, err, fresh)
	}
}
