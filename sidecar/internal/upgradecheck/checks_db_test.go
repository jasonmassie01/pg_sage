package upgradecheck

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// liveDB is one disposable database on the designated test server.
type liveDB struct {
	pool  *pgxpool.Pool
	ctx   context.Context
	name  string
	major int
	cfg   *pgxpool.Config
}

func randomSuffix(t *testing.T) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

// newDB creates a fresh database (createOpts are extra CREATE DATABASE
// options) and drops it, with its logical slots, when the test ends.
func newDB(t *testing.T, createOpts string) liveDB {
	t.Helper()
	base := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	name := "pgsage_x_upg_" + randomSuffix(t)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+
		" TEMPLATE template0 "+createOpts); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	cfg, err := pgxpool.ParseConfig(base)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg.Copy())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		bg := context.Background()
		_, _ = admin.Exec(bg, `SELECT pg_drop_replication_slot(slot_name)
			FROM pg_replication_slots WHERE database = $1`, name)
		if _, err := admin.Exec(bg, "DROP DATABASE IF EXISTS "+
			pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})
	db := liveDB{pool: pool, ctx: ctx, name: name, cfg: cfg}
	var v int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&v); err != nil {
		t.Fatalf("version: %v", err)
	}
	db.major = v / 10000
	return db
}

func (d liveDB) exec(t *testing.T, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := d.pool.Exec(d.ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// requireBelow18 is for the cases that plan an upgrade to 18: a PG18
// server has no newer major pg_sage knows to plan for.
func requireBelow18(t *testing.T, d liveDB) {
	t.Helper()
	if d.major >= 18 {
		t.Skipf("PostgreSQL %d: no known major above it to plan an upgrade to", d.major)
	}
}

func mustCheck(t *testing.T, r Report, id string) Check {
	t.Helper()
	c, ok := r.Check(id)
	if !ok {
		t.Fatalf("report has no %s: %+v", id, r.Checks)
	}
	return c
}

func checksumsOff(t *testing.T, d liveDB) bool {
	var s string
	if err := d.pool.QueryRow(d.ctx, "SHOW data_checksums").Scan(&s); err != nil {
		t.Fatalf("data_checksums: %v", err)
	}
	return s == "off"
}

// The fixture of the plan: a PK-less table, a REPLICA IDENTITY FULL
// table, a logical slot and a gin_trgm_ops index (on an ICU database
// where the server has database-level ICU, PG15+).
func fixtureDB(t *testing.T) liveDB {
	base := newDB(t, "")
	opts := ""
	if base.major >= 15 {
		opts = "LOCALE_PROVIDER icu ICU_LOCALE 'en-US'"
	}
	d := base
	if opts != "" {
		d = newDB(t, opts)
	}
	d.exec(t, "CREATE EXTENSION pg_trgm",
		"CREATE TABLE public.nopk (a int)",
		"CREATE TABLE public.fullid (a int)",
		"ALTER TABLE public.fullid REPLICA IDENTITY FULL",
		"CREATE TABLE public.docs (id int PRIMARY KEY, body text)",
		"CREATE INDEX docs_trgm ON public.docs USING gin (body gin_trgm_ops)",
		"SELECT pg_create_logical_replication_slot('upg_"+randomSuffix(t)+"', "+
			"'test_decoding')")
	return d
}

func TestRun_HappyPathFixture(t *testing.T) {
	d := fixtureDB(t)
	requireBelow18(t, d)
	rep, err := Run(d.ctx, d.pool, Target{Major: 18}, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Checks) != 7 || rep.SourceMajor != d.major || rep.Database != d.name ||
		rep.Target.Method != PGUpgrade {
		t.Fatalf("report = %+v", rep)
	}
	u1 := mustCheck(t, rep, "U1")
	if u1.Status != Warn || u1.Total != 1 || len(u1.Rows) != 1 ||
		u1.Rows[0]["table"] != "public.nopk" || u1.Rows[0]["replica_identity"] != "default" {
		t.Fatalf("U1 = %+v, want only public.nopk as a pg_upgrade warning", u1)
	}
	u2 := mustCheck(t, rep, "U2")
	if u2.Status != Unknown || u2.Total != 1 || u2.Rows[0]["extension"] != "pg_trgm" {
		t.Fatalf("U2 = %+v, want unknown listing pg_trgm", u2)
	}
	assertU3(t, d, mustCheck(t, rep, "U3"))
	assertU5(t, d, mustCheck(t, rep, "U5"))
	if u4 := mustCheck(t, rep, "U4"); checksumsOff(t, d) != (u4.Status == Block) {
		t.Fatalf("U4 = %+v, want block exactly when checksums are off", u4)
	}
	if u7 := mustCheck(t, rep, "U7"); u7.Status != Unknown {
		t.Fatalf("U7 with no provider = %+v, want unknown", u7)
	}
	bg, err := Run(d.ctx, d.pool, Target{Major: 18, Method: BlueGreen}, Options{})
	if err != nil {
		t.Fatalf("Run blue/green: %v", err)
	}
	if u1 := mustCheck(t, bg, "U1"); u1.Status != Block || u1.Total != 1 {
		t.Fatalf("blue/green U1 = %+v, want block on public.nopk", u1)
	}
}

func assertU3(t *testing.T, d liveDB, u3 Check) {
	t.Helper()
	if u3.Total != 1 || len(u3.Rows) != 1 || u3.Rows[0]["kind"] != "slot" ||
		u3.Rows[0]["plugin"] != "test_decoding" || u3.Rows[0]["database"] != d.name {
		t.Fatalf("U3 rows = %+v, want the one logical slot", u3)
	}
	want := Block
	if d.major >= 17 {
		want = Pass // pg_upgrade from 17 carries logical slots
	}
	if u3.Status != want {
		t.Fatalf("U3 on PG%d = %s (%s), want %s", d.major, u3.Status, u3.Reason, want)
	}
}

func assertU5(t *testing.T, d liveDB, u5 Check) {
	t.Helper()
	skipped := strings.Join(u5.Skipped, "; ")
	if d.major < 15 {
		if !strings.Contains(skipped, "database: PostgreSQL 14 records no database "+
			"collation version") || !strings.Contains(skipped, "index: the database uses "+
			"the libc provider") {
			t.Fatalf("PG14 U5 skipped = %q, want the database and index arms skipped", skipped)
		}
		return
	}
	if strings.Contains(skipped, "database:") || strings.Contains(skipped, "index:") {
		t.Fatalf("PG%d ICU U5 skipped = %q, want both arms run", d.major, skipped)
	}
	found := false
	for _, r := range u5.Rows {
		if r["kind"] == "index" && r["name"] == "public.docs_trgm" && r["access_method"] == "gin" {
			found = true
		}
	}
	if !found || u5.Status != Warn {
		t.Fatalf("U5 = %+v, want a warning listing public.docs_trgm", u5)
	}
}

func TestRun_EmptyDatabase(t *testing.T) {
	d := newDB(t, "")
	requireBelow18(t, d)
	rep, err := Run(d.ctx, d.pool, Target{Major: 18, Provider: "self-managed"}, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, id := range []string{"U1", "U2", "U3", "U5", "U6"} {
		c := mustCheck(t, rep, id)
		if c.Status != Pass || c.Total != 0 || len(c.Rows) != 0 {
			t.Fatalf("%s on an empty database = %+v, want pass with zero rows", id, c)
		}
	}
	if u7 := mustCheck(t, rep, "U7"); u7.Status != Pass || u7.Total != 2 {
		t.Fatalf("U7 = %+v, want pass with the dated source and target rows", u7)
	}
	if u4 := mustCheck(t, rep, "U4"); (u4.Status == Block) != checksumsOff(t, d) {
		t.Fatalf("U4 = %+v", u4)
	}
}

func TestRun_TargetAtOrBelowServerMajor(t *testing.T) {
	d := newDB(t, "")
	for _, major := range []int{d.major, d.major - 1} {
		_, err := Run(d.ctx, d.pool, Target{Major: major}, Options{})
		if !errors.Is(err, ErrInvalidTarget) {
			t.Fatalf("target %d on PG%d = %v, want ErrInvalidTarget", major, d.major, err)
		}
	}
}

func TestRun_PermissionDeniedMakesOnlyThatCheckUnknown(t *testing.T) {
	d := newDB(t, "")
	requireBelow18(t, d)
	role := "pgsage_upg_" + randomSuffix(t)
	secret := randomSuffix(t) + randomSuffix(t)
	d.exec(t, "REVOKE EXECUTE ON FUNCTION pg_catalog.pg_collation_actual_version(oid) "+
		"FROM PUBLIC", "CREATE ROLE "+role+" LOGIN PASSWORD '"+secret+"'")
	t.Cleanup(func() { _, _ = d.pool.Exec(context.Background(), "DROP ROLE "+role) })
	cfg := d.cfg.Copy()
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, secret
	limited, err := pgxpool.NewWithConfig(d.ctx, cfg)
	if err != nil {
		t.Fatalf("pool as %s: %v", role, err)
	}
	defer limited.Close()
	rep, err := Run(d.ctx, limited, Target{Major: 18, Provider: "self-managed"}, Options{})
	if err != nil {
		t.Fatalf("Run as %s: %v", role, err)
	}
	u5 := mustCheck(t, rep, "U5")
	if u5.Status != Unknown || u5.SQLState != "42501" {
		t.Fatalf("U5 = %+v, want unknown with SQLSTATE 42501", u5)
	}
	for _, id := range []string{"U1", "U2", "U3", "U6", "U7"} {
		if c := mustCheck(t, rep, id); c.Status != Pass || c.SQLState != "" {
			t.Fatalf("%s = %+v, want it to still run and pass", id, c)
		}
	}
}

func TestRun_RowCapBoundary(t *testing.T) {
	d := newDB(t, "")
	requireBelow18(t, d)
	d.exec(t, `DO $$ BEGIN FOR i IN 1..51 LOOP
		EXECUTE format('CREATE TABLE public.t%s (a int)', lpad(i::text, 2, '0'));
		END LOOP; END $$`)
	rep, err := Run(d.ctx, d.pool, Target{Major: 18, Method: Logical}, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	u1 := mustCheck(t, rep, "U1")
	if len(u1.Rows) != MaxRows || u1.Total != 51 || u1.Status != Block ||
		u1.Rows[0]["table"] != "public.t01" || u1.Rows[49]["table"] != "public.t50" {
		t.Fatalf("U1 = %d rows, total %d, %s; want 50 rows t01..t50, total 51, block",
			len(u1.Rows), u1.Total, u1.Status)
	}
}

func TestRun_RemovedSettings(t *testing.T) {
	d := newDB(t, "")
	requireBelow18(t, d)
	if d.major < 16 {
		d.exec(t, "ALTER DATABASE "+pgx.Identifier{d.name}.Sanitize()+
			" SET force_parallel_mode = on")
		d.pool.Reset() // the setting applies to new sessions
	}
	rep, err := Run(d.ctx, d.pool, Target{Major: 18}, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	u6 := mustCheck(t, rep, "U6")
	if d.major >= 16 {
		if u6.Status != Pass || u6.Total != 0 {
			t.Fatalf("PG%d U6 = %+v, want pass with no rows", d.major, u6)
		}
		return
	}
	if u6.Status != Warn || u6.Total != 1 || u6.Rows[0]["name"] != "force_parallel_mode" ||
		u6.Rows[0]["source"] != "database" ||
		!strings.Contains(u6.Rows[0]["change"], "debug_parallel_query") {
		t.Fatalf("PG%d U6 = %+v, want a warning on force_parallel_mode", d.major, u6)
	}
}

func TestCheckExtensions_AgainstTargetDatabase(t *testing.T) {
	src := newDB(t, "")
	dst := newDB(t, "")
	src.exec(t, "CREATE EXTENSION pg_trgm")
	tx, err := src.pool.BeginTx(src.ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	in := input{src: source{major: src.major}, target: Target{Major: src.major},
		opts: Options{TargetDB: dst.pool, TargetDatabase: dst.name}}
	got, err := checkExtensions(src.ctx, tx, in)
	if err != nil {
		t.Fatalf("checkExtensions: %v", err)
	}
	if got.Status != Pass || got.Total != 0 {
		t.Fatalf("same-server target = %+v, want pass: it offers pg_trgm's version", got)
	}
	in.target.Major = src.major + 1
	got, err = checkExtensions(src.ctx, tx, in)
	if err != nil {
		t.Fatalf("checkExtensions: %v", err)
	}
	if got.Status != Unknown || !strings.Contains(got.Reason, "not the target") ||
		got.Rows[0]["extension"] != "pg_trgm" {
		t.Fatalf("wrong-major target = %+v, want unknown naming the mismatch", got)
	}
}

func TestRun_WriteInsideRunFailsReadOnly(t *testing.T) {
	d := newDB(t, "")
	requireBelow18(t, d)
	var writeErr error
	opts := Options{inTx: func(ctx context.Context, tx pgx.Tx) {
		_, writeErr = tx.Exec(ctx, "CREATE TABLE public.upg_probe (a int)")
	}}
	if _, err := Run(d.ctx, d.pool, Target{Major: 18}, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(writeErr, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("write inside Run = %v, want SQLSTATE 25006", writeErr)
	}
	var exists bool
	if err := d.pool.QueryRow(d.ctx, "SELECT to_regclass('public.upg_probe') IS NOT NULL").
		Scan(&exists); err != nil || exists {
		t.Fatalf("probe table exists = %v (%v), want it never created", exists, err)
	}
}

func TestRun_ConcurrentOnOnePool(t *testing.T) {
	d := newDB(t, "")
	requireBelow18(t, d)
	d.exec(t, "CREATE TABLE public.nopk (a int)")
	const n = 8
	errs := make(chan error, n)
	totals := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			rep, err := Run(d.ctx, d.pool, Target{Major: 18}, Options{})
			if err != nil {
				errs <- err
				return
			}
			c, _ := rep.Check("U1")
			totals <- c.Total
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			t.Fatalf("concurrent Run: %v", err)
		case total := <-totals:
			if total != 1 {
				t.Fatalf("concurrent Run U1 total = %d, want 1", total)
			}
		}
	}
}

func TestMonitor_FindingOpensAndResolves(t *testing.T) {
	d := newDB(t, "")
	requireBelow18(t, d)
	d.exec(t, "CREATE TABLE public.nopk (a int)")
	clock := at("2026-11-12")
	m := NewMonitor(d.pool, MonitorOptions{Provider: func() string { return "self-managed" },
		Due: func(int, time.Time) bool { return true }, Now: func() time.Time { return clock }})
	u1ID := "PostgreSQL " + strconv.Itoa(d.major) + " to 18 via pg_upgrade: U1"
	found := detectIDs(t, d, m)
	if found[u1ID] != "info" {
		t.Fatalf("first run findings = %v, want %q", found, u1ID)
	}
	u4ID := "PostgreSQL " + strconv.Itoa(d.major) + " to 18 via pg_upgrade: U4"
	if checksumsOff(t, d) && found[u4ID] != "warning" {
		t.Fatalf("first run findings = %v, want the U4 block as a warning", found)
	}
	if got := m.LastEvaluatedCategories(); len(got) != 1 || got[0] != Category {
		t.Fatalf("evaluated = %v, want [%s]", got, Category)
	}
	d.exec(t, "ALTER TABLE public.nopk ADD COLUMN id bigserial PRIMARY KEY")
	clock = clock.Add(time.Hour)
	if found := detectIDs(t, d, m); len(found) != 0 || len(m.LastEvaluatedCategories()) != 0 {
		t.Fatalf("an hour later = %v, want no run (open findings stay)", found)
	}
	clock = clock.Add(24 * time.Hour)
	found = detectIDs(t, d, m)
	if _, still := found[u1ID]; still || len(m.LastEvaluatedCategories()) != 1 {
		t.Fatalf("after the fix = %v, want U1 gone and the category evaluated", found)
	}
	notDue := NewMonitor(d.pool, MonitorOptions{Due: func(int, time.Time) bool {
		return false
	}})
	if found := detectIDs(t, d, notDue); len(found) != 0 ||
		len(notDue.LastEvaluatedCategories()) != 1 {
		t.Fatalf("not due = %v, want nothing found and the category evaluated", found)
	}
}

func detectIDs(t *testing.T, d liveDB, m *Monitor) map[string]string {
	t.Helper()
	got, err := m.Detect(d.ctx)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	out := map[string]string{}
	for _, f := range got {
		if f.Category != Category {
			t.Fatalf("finding category = %q", f.Category)
		}
		out[f.ObjectIdentifier] = f.Severity
	}
	return out
}
