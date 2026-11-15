package workpack

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// evid-02b on a real server, in both history placements: a fixture with
// two tables, a partitioned table, a duplicate and a partial index, a
// work_mem change, two vacuums, a captured plan and a confirmed brief
// gives every section its expected facts and ids, the same hash from
// either placement, and never another database's snapshots.

const privacyMarker = "MARKER-7f3a-evid02b@example.com"

const catalogFixtureSQL = `
DROP TABLE IF EXISTS public.orders, public.customers CASCADE;
DROP SCHEMA IF EXISTS app CASCADE;
CREATE TABLE public.customers (id int PRIMARY KEY, email text NOT NULL);
CREATE TABLE public.orders (id int PRIMARY KEY, c int REFERENCES public.customers (id),
    status text, x int);
CREATE INDEX orders_c ON public.orders (c);
CREATE INDEX orders_c_dup ON public.orders (c);
CREATE INDEX orders_c_status ON public.orders (c, status);
CREATE INDEX orders_open ON public.orders (x) WHERE status = 'open';
CREATE SCHEMA app;
CREATE TABLE app.events (at date NOT NULL, v int) PARTITION BY RANGE (at);
CREATE TABLE app.events_p1 PARTITION OF app.events FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE app.events_p2 PARTITION OF app.events FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
CREATE TABLE app.events_p3 PARTITION OF app.events FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');
INSERT INTO public.customers SELECT g, CASE WHEN g % 4 <> 0 THEN '` + privacyMarker +
	`' ELSE 'c' || g END FROM generate_series(1, 400) g;
INSERT INTO public.orders SELECT g, 1 + g % 400, 'open', g FROM generate_series(1, 400) g;
ANALYZE public.customers;
ANALYZE public.orders;
DELETE FROM sage.explain_cache WHERE queryid BETWEEN 1 AND 5;
DELETE FROM sage.facts;
DELETE FROM sage.host_usage_hourly WHERE resource = 'evid02b-resource';
`

func seedCatalog(t *testing.T, p *histfixture.Pair) {
	t.Helper()
	ctx := context.Background()
	if _, err := p.Monitored.Exec(ctx, catalogFixtureSQL); err != nil {
		t.Fatalf("catalog fixture: %v", err)
	}
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO sage.explain_cache (captured_at, queryid, plan_json, source) VALUES
			($1, 1, '[{"Plan": {"Node Type": "Index Scan", "Index Name": "orders_pkey",
			  "Relation Name": "orders", "Schema": "public", "Index Cond": "(id = 42)",
			  "Plan Rows": 1, "Total Cost": 8.3}}]', 'auto_explain'),
			($1, 2, '{"no": "plan"}', 'explain')`, []any{fixFrom.Add(time.Hour)}},
		{`INSERT INTO sage.facts (fact_type, subject_kind, subject, value, source, status,
			decided_by, decided_at) VALUES ('owner_brief', 'database', '*',
			'{"owner": "payments team"}', 'operator', 'confirmed', 'ops@example.com', $1),
			('append_only', 'table', 'public.orders', '{}', 'operator', 'confirmed', 'dba', $1)`,
			[]any{fixFrom}},
		{`INSERT INTO sage.host_usage_hourly (resource, hour, instance_class, vcpus,
			memory_total_bytes, allocated_storage_bytes, samples) VALUES
			('evid02b-resource', $1, 'db.r6g.large', 2, 17179869184, 107374182400, 4)`,
			[]any{fixFrom.Add(24 * time.Hour)}},
	}
	for _, s := range stmts {
		if _, err := p.Monitored.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, s.sql)
		}
	}
}

// seedMaintenance writes the window's config, index, table, system and
// workload_sessions snapshots.
func seedMaintenance(t *testing.T, db any) {
	t.Helper()
	snap := func(at time.Time, category, doc string) {
		execHist(t, db, `INSERT INTO sage.snapshots (collected_at, category, data{dbcol})
			VALUES ($1, $2, $3::jsonb{dbval})`, at, category, doc)
	}
	day := func(d int, h time.Duration) time.Time { return fixFrom.AddDate(0, 0, d).Add(h) }
	cfg := func(workMem string) string {
		return `{"pg_settings": [{"name": "work_mem", "setting": "` + workMem +
			`", "unit": "kB", "source": "configuration file"},
			{"name": "max_connections", "setting": "100", "unit": "", "source": "default"},
			{"name": "max_wal_size", "setting": "1024", "unit": "MB", "source": "default"}]}`
	}
	snap(day(0, 12*time.Hour), "config_data", cfg("4096"))
	snap(day(10, 12*time.Hour), "config_data", cfg("8192"))
	snap(day(0, 12*time.Hour), "indexes", `[
		{"schemaname": "public", "indexrelname": "orders_c", "idx_scan": 5},
		{"schemaname": "public", "indexrelname": "orders_c_dup", "idx_scan": 0}]`)
	snap(day(27, 13*time.Hour), "indexes", `[
		{"schemaname": "public", "indexrelname": "orders_c", "idx_scan": 25},
		{"schemaname": "public", "indexrelname": "orders_c_dup", "idx_scan": 0},
		{"schemaname": "public", "indexrelname": "orders_open", "idx_scan": 9}]`)
	snap(day(0, 12*time.Hour), "tables", `[{"schemaname": "public", "relname": "orders",
		"n_tup_ins": 100, "n_tup_upd": 10, "n_tup_del": 0, "n_dead_tup": 50,
		"autovacuum_count": 1, "autoanalyze_count": 0, "total_bytes": 5000}]`)
	snap(day(27, 23*time.Hour+30*time.Minute), "tables", `[{"schemaname": "public",
		"relname": "orders", "n_tup_ins": 400, "n_tup_upd": 40, "n_tup_del": 5,
		"n_dead_tup": 7, "autovacuum_count": 3, "autoanalyze_count": 1,
		"last_autovacuum": "2026-11-01T03:00:00Z", "total_bytes": 5000,
		"reloptions": ["fillfactor=90", "bogus"]}]`)
	execHist(t, db, `UPDATE sage.snapshots s SET data = s.data || jsonb_build_object(
		'relation_stats_epoch', '2026-09-01T00:00:00Z',
		'checkpoint_stats_epoch', '2026-09-01T00:00:00Z',
		'wal_stats_epoch', '2026-09-01T00:00:00Z',
		'temp_bytes', d.n * 24000, 'checkpoints_timed', d.n * 10,
		'checkpoints_requested', d.n * 2, 'cluster_wal_bytes', d.n * 2400000,
		'client_backends', CASE WHEN d.n = 5 THEN 40 ELSE 10 END,
		'max_connections', 100, 'reserved_connections', 3)
		FROM (SELECT x.id, floor(extract(epoch FROM x.collected_at - $1::timestamptz)
		        / 86400)::int8 AS n FROM sage.snapshots x WHERE x.category = 'system') d
		WHERE {db:s} AND s.category = 'system' AND s.id = d.id`, fixFrom)
	ws := func(ages string) string {
		return `{"groups": [{"role": "app", "application": "web", "state": "idle",
			"sessions": 4}], "long": [], "roles": []` + ages + `}`
	}
	ages := `, "ages": [{"state": "idle", "bucket": "ge_1h", "sessions": 4}]`
	snap(day(0, 9*time.Hour), "workload_sessions", ws(ages))
	snap(day(7, 9*time.Hour+time.Minute), "workload_sessions", ws(ages))
	snap(day(0, 10*time.Hour), "workload_sessions", ws(""))
}

func sectionRequest() Request {
	req := fixRequest()
	req.Resource = "evid02b-resource"
	return req
}

func decodeSection(t *testing.T, p *Pack, name string, dst any) {
	t.Helper()
	if err := json.Unmarshal(p.File(name), dst); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, p.File(name))
	}
}

func TestSectionsDB_FixtureGivesEverySectionInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	hashes := map[histstore.Mode]string{}
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		clearPacks(t, p)
		seedFixture(t, p.Monitored)
		seedMaintenance(t, p.Monitored)
		seedCatalog(t, p)
		if mode == histstore.ModeMeta {
			noise(t, p)
			noiseMaintenance(t, p)
		}
		pack, err := Build(ctx, p.Monitored, sectionRequest())
		if err != nil {
			t.Fatalf("%s: build: %v", mode, err)
		}
		checkIndexes(t, pack)
		checkSettings(t, pack)
		checkAutovacuum(t, pack)
		checkSchema(t, pack)
		checkPlansAndContext(t, pack)
		checkConnectionsProviderStats(t, pack)
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

// noiseMaintenance writes another database's config and sessions into the
// meta database: a reader that forgets its database id reports them.
func noiseMaintenance(t *testing.T, p *histfixture.Pair) {
	t.Helper()
	other, err := histstore.NewMeta(p.Meta, histfixture.OtherDatabaseID)
	if err != nil {
		t.Fatal(err)
	}
	execHist(t, other, `INSERT INTO sage.snapshots (collected_at, category, data{dbcol})
		VALUES ($1, 'config_data', '{"pg_settings": [{"name": "work_mem",
		"setting": "65536", "unit": "kB", "source": "noise"}]}'::jsonb{dbval}),
		($1, 'workload_sessions', '{"groups": [{"state": "idle", "sessions": 999}],
		"ages": [{"state": "idle", "bucket": "lt_1m", "sessions": 999}]}'::jsonb{dbval})`,
		fixFrom.AddDate(0, 0, 20).Add(9*time.Hour))
}

func indexByName(t *testing.T, ix Indexes, name string) IndexFact {
	t.Helper()
	for _, f := range ix.Indexes {
		if f.Index == name {
			return f
		}
	}
	t.Fatalf("no index %s in %d", name, len(ix.Indexes))
	return IndexFact{}
}

func checkIndexes(t *testing.T, pack *Pack) {
	t.Helper()
	var ix Indexes
	decodeSection(t, pack, fileIndexes, &ix)
	c := indexByName(t, ix, "orders_c")
	if ix.ScansStatus != relStatusOK || c.ID != "index/idx/public.orders_c" ||
		c.Scans != 20.0 || c.Table != "orders" || c.SizeBytes <= 0 ||
		!strings.Contains(c.Definition, "(c)") || strings.Join(c.DuplicateOf, ",") !=
		"public.orders_c_dup" || strings.Join(c.PrefixOf, ",") != "public.orders_c_status" {
		t.Fatalf("orders_c %+v (status %s)", c, ix.ScansStatus)
	}
	w, _ := c.TableWrites.(map[string]any)
	if w["inserts"] != 300.0 || w["updates"] != 30.0 || w["deletes"] != 5.0 {
		t.Fatalf("orders writes %v", c.TableWrites)
	}
	if dup := indexByName(t, ix, "orders_c_dup"); dup.Scans != 0.0 {
		t.Fatalf("orders_c_dup scans %v, want 0 (no reset)", dup.Scans)
	}
	open := indexByName(t, ix, "orders_open")
	if !open.Partial || open.Scans != valueUnknown || len(open.DuplicateOf) != 0 {
		t.Fatalf("orders_open %+v, want partial, scans unknown", open)
	}
	pk := indexByName(t, ix, "orders_pkey")
	if !pk.Primary || !pk.Unique || !pk.ConstraintBacking || !pk.Valid {
		t.Fatalf("orders_pkey %+v", pk)
	}
	for _, f := range ix.Indexes {
		if strings.HasPrefix(f.Schema, "pg_") || f.Schema == "sage" ||
			f.Schema == "information_schema" {
			t.Fatalf("index of %s listed: %+v", f.Schema, f)
		}
	}
}

func settingByName(t *testing.T, s Settings, name string) SettingFact {
	t.Helper()
	for _, f := range s.Settings {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no setting %s", name)
	return SettingFact{}
}

func checkSettings(t *testing.T, pack *Pack) {
	t.Helper()
	var s Settings
	decodeSection(t, pack, fileSettings, &s)
	wm := settingByName(t, s, "work_mem")
	if s.Status != "ok" || wm.ID != "setting/work_mem" || wm.Value != "8192" ||
		wm.Unit != "kB" || len(wm.Changes) != 1 || wm.Changes[0] != (SettingChange{
		Day: "2026-10-15", From: "4096", To: "8192"}) || wm.Measure != "temp_bytes_per_hour" {
		t.Fatalf("work_mem %+v", wm)
	}
	m := s.Measures
	if m.TempBytesPerHour != 1000.0 || m.WALBytesPerHour != 100000.0 ||
		m.CheckpointsTimed != 270.0 || m.CheckpointsRequested != 54.0 ||
		m.PeakClientBackends != 40.0 || m.UsableConnections != 97.0 {
		t.Fatalf("measures %+v", m)
	}
	if s.Hardware.VCPUs != 2.0 || s.Hardware.MemoryBytes != 17179869184.0 {
		t.Fatalf("hardware %+v", s.Hardware)
	}
}

func checkAutovacuum(t *testing.T, pack *Pack) {
	t.Helper()
	var av Autovacuum
	decodeSection(t, pack, fileAutovacuum, &av)
	var orders *AutovacuumFact
	for i := range av.Tables {
		if av.Tables[i].Table == "public.orders" {
			orders = &av.Tables[i]
		}
	}
	if orders == nil || orders.ID != "table/av/public.orders" || orders.Autovacuums != 2.0 ||
		orders.Autoanalyzes != 1.0 || orders.LastAutovacuum != "2026-11-01T03:00:00Z" ||
		orders.Reloptions["fillfactor"] != "90" ||
		strings.Join(orders.ReloptionsText, ",") != "bogus" {
		t.Fatalf("orders autovacuum %+v", orders)
	}
	if len(orders.DeadTuples) != len(av.Days) || *orders.DeadTuples[0] != 50 ||
		*orders.DeadTuples[27] != 7 || orders.DeadTuples[1] != nil {
		t.Fatalf("dead tuple series %v over %d days", orders.DeadTuples, len(av.Days))
	}
}

func checkSchema(t *testing.T, pack *Pack) {
	t.Helper()
	var s Schema
	decodeSection(t, pack, fileSchema, &s)
	byName := map[string]TableFact{}
	for _, tf := range s.Tables {
		byName[tf.Table] = tf
		if strings.HasPrefix(tf.Table, "sage.") || strings.HasPrefix(tf.Table, "app.events_p") {
			t.Fatalf("schema lists %s", tf.Table)
		}
	}
	ev := byName["app.events"]
	if ev.ID != "schema/table/app.events" || ev.Kind != "partitioned" ||
		ev.PartitionCount != 3 || !strings.Contains(ev.PartitionKey, "RANGE (at)") {
		t.Fatalf("app.events %+v", ev)
	}
	o := byName["public.orders"]
	if o.Kind != "table" || len(o.Columns) != 4 || o.Columns[0] != (ColumnFact{Name: "id",
		Type: "integer", NotNull: true}) || len(o.Indexes) != 5 || len(o.Constraints) != 2 {
		t.Fatalf("public.orders %+v", o)
	}
}

func checkPlansAndContext(t *testing.T, pack *Pack) {
	t.Helper()
	var pl Plans
	decodeSection(t, pack, filePlans, &pl)
	byQ := map[string]PlanFact{}
	for _, f := range pl.Plans {
		byQ[f.QueryID] = f
	}
	if byQ["1"].Plan != "ok" || byQ["1"].Source != "auto_explain" || byQ["2"].Plan !=
		valueUnreadable || byQ["3"].Plan != valueAbsent || len(pl.Plans) != 5 {
		t.Fatalf("plans %+v", pl.Plans)
	}
	if strings.Contains(string(pack.File(filePlans)), "id = 42") {
		t.Fatal("plans.json holds a plan condition")
	}
	var c Context
	decodeSection(t, pack, fileContext, &c)
	b, _ := c.Brief.(map[string]any)
	if b["id"] != "context/brief" || b["confirmed_by"] != "ops@example.com" ||
		len(c.Facts) != 1 || c.Facts[0].Subject != "public.orders" {
		t.Fatalf("context %+v", c)
	}
}

func checkConnectionsProviderStats(t *testing.T, pack *Pack) {
	t.Helper()
	var cn Connections
	decodeSection(t, pack, fileConnections, &cn)
	h9, h10 := cn.Hours[9], cn.Hours[10]
	st, _ := h9.States.(map[string]any)
	ages, _ := h9.Ages.(map[string]any)
	idle, _ := ages["idle"].(map[string]any)
	if h9.Snapshots != 2 || st["idle"] != 4.0 || idle["ge_1h"] != 4.0 {
		t.Fatalf("hour 9 %+v", h9)
	}
	if h10.Snapshots != 1 || h10.Ages != valueUnknown || cn.Hours[11].States != valueUnknown {
		t.Fatalf("hour 10 %+v, hour 11 %+v", h10, cn.Hours[11])
	}
	var pv Provider
	decodeSection(t, pack, fileProvider, &pv)
	if pv.Status != "ok" || len(pv.Facts) < 5 {
		t.Fatalf("provider %+v", pv)
	}
	var st2 Stats
	decodeSection(t, pack, fileStats, &st2)
	status := map[string]string{}
	for _, tb := range st2.Tables {
		status[tb.Table] = tb.Status
	}
	if status["public.customers"] != "ok" || status["app.events"] != valueUnknown {
		t.Fatalf("stats tables %v", st2.Tables)
	}
	for _, col := range st2.Columns {
		if col.ID == "stats/column/public.customers.email" && col.NullFrac == 0 &&
			col.AvgWidth > 0 && col.MostCommonVals == nil {
			return
		}
	}
	t.Fatalf("no readable email statistics in %+v", st2.Columns)
}

func TestSectionsDB_ValueSamplesOnlyWhenAskedAndOnlyInStats(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	clearPacks(t, p)
	seedFixture(t, p.Monitored)
	seedCatalog(t, p)
	ctx := context.Background()
	off, err := Build(ctx, p.Monitored, sectionRequest())
	if err != nil {
		t.Fatal(err)
	}
	unpacked, err := untarGz(off.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range unpacked {
		if strings.Contains(string(f.Data), privacyMarker) {
			t.Fatalf("value samples off: %s holds the marker", f.Name)
		}
	}
	req := sectionRequest()
	req.IncludeValueSamples = true
	on, err := Build(ctx, p.Monitored, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range on.Files {
		has := strings.Contains(string(f.Data), privacyMarker)
		if has != (f.Name == fileStats) {
			t.Fatalf("value samples on: %s holds the marker: %v", f.Name, has)
		}
	}
}

func TestSectionsDB_ACatalogTimeoutNamesTheSectionAndTheBound(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	clearPacks(t, p)
	seedFixture(t, p.Monitored)
	seedCatalog(t, p)
	ctx := context.Background()
	conn, err := p.Monitored.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE public.orders IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	req := sectionRequest()
	req.Timeouts = catalogread.Timeouts{Statement: 10 * time.Second,
		Lock: 200 * time.Millisecond}
	_, err = Build(ctx, p.Monitored, req)
	if err == nil || !strings.Contains(err.Error(), "schema section") ||
		!strings.Contains(err.Error(), "lock_timeout 200ms") {
		t.Fatalf("build under a lock: %v, want the schema section and its lock_timeout", err)
	}
}

func TestSectionsDB_BriefConfirmedThenExpiredChangesTheHash(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	clearPacks(t, p)
	seedFixture(t, p.Monitored)
	seedCatalog(t, p)
	ctx := context.Background()
	with, err := Build(ctx, p.Monitored, sectionRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Monitored.Exec(ctx, `UPDATE sage.facts SET status = 'expired',
		expires_at = $1 WHERE fact_type = 'owner_brief'`, fixFrom); err != nil {
		t.Fatal(err)
	}
	without, err := Build(ctx, p.Monitored, sectionRequest())
	if err != nil {
		t.Fatal(err)
	}
	var c Context
	decodeSection(t, without, fileContext, &c)
	b, _ := c.Brief.(map[string]any)
	if b["brief"] != valueAbsent || without.ContentSHA256 == with.ContentSHA256 {
		t.Fatalf("expired brief: %v, hash moved %v", c.Brief,
			without.ContentSHA256 != with.ContentSHA256)
	}
}

func TestSectionsDB_NothingCapturedGivesUnknownNotOmitted(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	clearPacks(t, p)
	seedFixture(t, p.Monitored)
	seedCatalog(t, p)
	ctx := context.Background()
	if _, err := p.Monitored.Exec(ctx, `DELETE FROM sage.explain_cache WHERE queryid
		BETWEEN 1 AND 5; DELETE FROM sage.facts`); err != nil {
		t.Fatal(err)
	}
	req := fixRequest() // no provider resource
	pack, err := Build(ctx, p.Monitored, req)
	if err != nil {
		t.Fatal(err)
	}
	var pl Plans
	decodeSection(t, pack, filePlans, &pl)
	for _, f := range pl.Plans {
		if f.Plan != valueAbsent {
			t.Fatalf("plan %+v, want absent", f)
		}
	}
	var pv Provider
	decodeSection(t, pack, fileProvider, &pv)
	var s Settings
	decodeSection(t, pack, fileSettings, &s)
	var cn Connections
	decodeSection(t, pack, fileConnections, &cn)
	if pv.Status != valueUnknown || s.Status != valueUnknown ||
		s.Hardware.VCPUs != valueUnknown || cn.Hours[0].States != valueUnknown {
		t.Fatalf("provider %+v settings %+v hour 0 %+v", pv, s.Status, cn.Hours[0])
	}
	if pack.Manifest.Completeness == nil || pack.Manifest.Completeness.Sections[fileProvider] {
		t.Fatalf("completeness %+v, want provider incomplete", pack.Manifest.Completeness)
	}
}

func TestSectionsDB_ConcurrentMetaBuildsReadTheirOwnSnapshots(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMeta)
	clearPacks(t, p)
	seedFixture(t, p.Monitored)
	seedMaintenance(t, p.Monitored)
	seedCatalog(t, p)
	noise(t, p)
	noiseMaintenance(t, p)
	var wg sync.WaitGroup
	hashes := make([]string, 4)
	errs := make([]error, 4)
	for i := range hashes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pack, err := Build(context.Background(), p.Monitored, sectionRequest())
			if err == nil {
				hashes[i] = pack.ContentSHA256
				var s Settings
				err = json.Unmarshal(pack.File(fileSettings), &s)
				for _, f := range s.Settings {
					if f.Name == "work_mem" && f.Value != "8192" {
						hashes[i] = "work_mem " + f.Value
					}
				}
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i := range hashes {
		if errs[i] != nil || hashes[i] != hashes[0] || len(hashes[i]) != 64 {
			t.Fatalf("build %d: %v %s, want %s", i, errs[i], hashes[i], hashes[0])
		}
	}
}
