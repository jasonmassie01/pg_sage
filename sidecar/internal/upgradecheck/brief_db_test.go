package upgradecheck

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// targetMember is the fleet member the brief reads target-side facts
// from, and the target major to plan for. With SAGE_TEST_SECOND_CLUSTER_URL
// naming a newer major it is the cross-version integration (a PG14 source
// with a PG17 member as the target-side fact source); without it the
// member is a second database on the same server, whose major is not the
// target, so the extension path must say so.
func targetMember(t *testing.T, src liveDB) (*pgxpool.Pool, string, int, bool) {
	t.Helper()
	if os.Getenv(testdb.SecondClusterEnvName) == "" {
		other := newDB(t, "")
		return other.pool, other.name, 18, false
	}
	pool, err := pgxpool.New(src.ctx, testdb.CreateDatabaseOnSecondCluster(t, "upgbrief"))
	if err != nil {
		t.Fatalf("connect the second cluster: %v", err)
	}
	t.Cleanup(pool.Close)
	var v int
	if err := pool.QueryRow(src.ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&v); err != nil {
		t.Fatalf("member version: %v", err)
	}
	if v/10000 <= src.major {
		t.Fatalf("%s runs PostgreSQL %d, not above the source %d",
			testdb.SecondClusterEnvName, v/10000, src.major)
	}
	return pool, "member", v / 10000, true
}

// briefFixture: pgvector installed, one index on a column whose ICU
// collation's recorded version differs from the library's, and one
// logical slot.
func briefFixture(t *testing.T) (liveDB, string) {
	d := newDB(t, "")
	requireBelow18(t, d)
	slot := "brief_" + randomSuffix(t)
	d.exec(t, "CREATE EXTENSION vector",
		`CREATE TABLE public.docs (id int PRIMARY KEY, title text COLLATE "en-US-x-icu")`,
		"CREATE INDEX docs_title ON public.docs (title)",
		`UPDATE pg_catalog.pg_collation SET collversion = '0.0'
		 WHERE collname = 'en-US-x-icu'`,
		"SELECT pg_create_logical_replication_slot('"+slot+"', 'test_decoding')")
	return d, slot
}

func TestBriefDB_HappyPathFixture(t *testing.T) {
	d, slot := briefFixture(t)
	member, memberName, major, cross := targetMember(t, d)
	b, err := BuildBrief(d.ctx, d.pool, Target{Major: major, Provider: "self-managed"},
		BriefOptions{Options: Options{TargetDB: member, TargetDatabase: memberName},
			SlotConsumers: map[string]string{slot: "debezium"}})
	if err != nil {
		t.Fatalf("BuildBrief: %v", err)
	}
	if b.Database != d.name || b.Source.Major != d.major || b.Target.Major != major {
		t.Fatalf("brief header = %s %+v %+v", b.Database, b.Source, b.Target)
	}
	assertBriefExtension(t, b, cross, major)
	re := b.CollationReindex
	if re.State != SectionKnown || re.Total != 1 || len(re.Items) != 1 ||
		re.Items[0].Index != "public.docs_title" || re.Items[0].Table != "public.docs" ||
		strings.Join(re.Items[0].Collations, ",") != "en-US-x-icu" ||
		re.Items[0].Action != ReindexAction {
		t.Fatalf("reindex = %+v, want public.docs_title on en-US-x-icu", re)
	}
	sp := b.SlotPlan
	if sp.State != SectionKnown || len(sp.Items) != 1 || sp.Items[0].Name != slot ||
		sp.Items[0].Consumer != "debezium" || sp.Items[0].Plugin != "test_decoding" {
		t.Fatalf("slot plan = %+v, want %s owned by debezium", sp, slot)
	}
	wantStep := "drop and recreate after"
	if d.major >= 17 {
		wantStep = "carried by pg_upgrade"
	}
	if !strings.Contains(sp.Items[0].Step, wantStep) {
		t.Fatalf("PG%d slot step %q, want %q", d.major, sp.Items[0].Step, wantStep)
	}
	pos := map[string]int{}
	for _, s := range b.Checklist {
		pos[s.Step] = s.Order
	}
	if !(pos["extension_path"] < pos["reindex_plan"] && pos["reindex_plan"] < pos["slot_plan"] &&
		pos["slot_plan"] < pos["rehearsal"] && pos["rehearsal"] < pos["cutover"]) {
		t.Fatalf("checklist order = %v", pos)
	}
	for step, name := range map[string]string{"extension_path": "vector",
		"reindex_plan": "public.docs_title", "slot_plan": slot} {
		if det := stepIndex(t, b, step).Detail; !strings.Contains(det, name) {
			t.Fatalf("%s detail %q does not name %s", step, det, name)
		}
	}
}

func assertBriefExtension(t *testing.T, b Brief, cross bool, major int) {
	t.Helper()
	e := b.ExtensionPath
	if len(e.Items) != 1 || e.Items[0].Extension != "vector" || e.Items[0].InstalledVersion == "" {
		t.Fatalf("extension path = %+v, want vector", e)
	}
	if cross {
		if e.State != SectionKnown || len(e.Items[0].TargetVersions) == 0 {
			t.Fatalf("PG%d member: extension path = %+v, want vector's target versions",
				major, e)
		}
		return
	}
	if e.State != SectionUnknown || e.Items[0].Path != "unknown" ||
		!strings.Contains(e.Reason, "not the target") {
		t.Fatalf("same-server member: extension path = %+v, want unknown naming the "+
			"major mismatch", e)
	}
}

func TestBriefDB_TargetEqualToSourceRefused(t *testing.T) {
	d := newDB(t, "")
	_, err := BuildBrief(d.ctx, d.pool, Target{Major: d.major}, BriefOptions{})
	if !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("target %d on PG%d = %v, want ErrInvalidTarget", d.major, d.major, err)
	}
}

func TestBriefDB_EmptyDatabaseNoBlockers(t *testing.T) {
	d := newDB(t, "")
	requireBelow18(t, d)
	b, err := BuildBrief(d.ctx, d.pool, Target{Method: BlueGreen, Provider: "self-managed"},
		BriefOptions{})
	if err != nil {
		t.Fatalf("BuildBrief: %v", err)
	}
	if b.Target.Major != DefaultTargetMajor(0) {
		t.Fatalf("default target = %d, want the newest community major", b.Target.Major)
	}
	if b.Blockers.Summary != NoBlockersFound || len(b.Blockers.Items) != 0 {
		t.Fatalf("blockers = %+v, want none", b.Blockers)
	}
	if len(b.Checklist) != 7 || len(b.SlotPlan.Items) != 0 || len(b.CollationReindex.Items) != 0 {
		t.Fatalf("brief = %+v, want an empty plan with the full checklist", b)
	}
}

func TestBriefDB_PermissionDeniedLeavesSlotPlanUnknown(t *testing.T) {
	d := newDB(t, "")
	requireBelow18(t, d)
	role := "pgsage_brf_" + randomSuffix(t)
	secret := randomSuffix(t) + randomSuffix(t)
	d.exec(t, "REVOKE SELECT ON pg_catalog.pg_replication_slots FROM PUBLIC",
		"CREATE ROLE "+role+" LOGIN PASSWORD '"+secret+"'")
	t.Cleanup(func() { _, _ = d.pool.Exec(context.Background(), "DROP ROLE "+role) })
	cfg := d.cfg.Copy()
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, secret
	limited, err := pgxpool.NewWithConfig(d.ctx, cfg)
	if err != nil {
		t.Fatalf("pool as %s: %v", role, err)
	}
	defer limited.Close()
	b, err := BuildBrief(d.ctx, limited, Target{Major: 18, Provider: "self-managed"},
		BriefOptions{})
	if err != nil {
		t.Fatalf("BuildBrief as %s: %v", role, err)
	}
	if b.SlotPlan.State != SectionUnknown || b.SlotPlan.SQLState != "42501" {
		t.Fatalf("slot plan = %+v, want unknown with SQLSTATE 42501", b.SlotPlan)
	}
	if b.ExtensionPath.State != SectionKnown || b.CollationReindex.State != SectionKnown {
		t.Fatalf("other sections = %+v / %+v, want them still read", b.ExtensionPath,
			b.CollationReindex)
	}
}

// bootstrapped is a fresh database with the sage schema, bootstrapped
// twice as a fresh server would be.
func bootstrapped(t *testing.T) liveDB {
	d := newDB(t, "")
	requireBelow18(t, d)
	for run := range 2 {
		if err := schema.Bootstrap(d.ctx, d.pool); err != nil {
			t.Fatalf("bootstrap %d: %v", run, err)
		}
	}
	return d
}

func briefRows(t *testing.T, d liveDB) int {
	t.Helper()
	var n int
	if err := d.pool.QueryRow(d.ctx, `SELECT count(*) FROM sage.upgrade_briefs
		WHERE database = $1`, d.name).Scan(&n); err != nil {
		t.Fatalf("count briefs: %v", err)
	}
	return n
}

// Concurrency without -race (no cgo on this machine): eight stores of one
// brief at once keep exactly one row.
func TestBriefDB_ConcurrentStoreKeepsOneRow(t *testing.T) {
	d := bootstrapped(t)
	b, err := BuildBrief(d.ctx, d.pool, Target{Major: 18, Method: Logical,
		Provider: "self-managed"}, BriefOptions{})
	if err != nil {
		t.Fatalf("BuildBrief: %v", err)
	}
	const n = 8
	var wg sync.WaitGroup
	results := make(chan StoredBrief, n)
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := StoreBrief(d.ctx, d.pool, b)
			if err != nil {
				errs <- err
				return
			}
			results <- got
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent StoreBrief: %v", err)
	}
	stored, ids := 0, map[int64]bool{}
	for r := range results {
		if r.Stored {
			stored++
		}
		ids[r.ID] = true
		if r.Hash != b.Hash || r.BuiltAt.IsZero() {
			t.Fatalf("result = %+v, want the brief's hash and a build time", r)
		}
	}
	if stored != 1 || len(ids) != 1 || briefRows(t, d) != 1 {
		t.Fatalf("stored %d, ids %v, rows %d; want one row", stored, ids, briefRows(t, d))
	}
}

func TestBriefDB_BlockerResolvedStoresNewKeepsOld(t *testing.T) {
	d := bootstrapped(t)
	d.exec(t, "CREATE TABLE public.nopk (a int)")
	target := Target{Major: 18, Method: Logical, Provider: "self-managed"}
	first, err := BuildBrief(d.ctx, d.pool, target, BriefOptions{})
	if err != nil {
		t.Fatalf("BuildBrief: %v", err)
	}
	if !hasCheckBlocker(first, "U1") {
		t.Fatalf("blockers = %+v, want U1 on public.nopk", first.Blockers)
	}
	s1, err := StoreBrief(d.ctx, d.pool, first)
	if err != nil || !s1.Stored {
		t.Fatalf("first store = %+v, %v; want stored", s1, err)
	}
	again, err := StoreBrief(d.ctx, d.pool, first)
	if err != nil || again.Stored || again.ID != s1.ID || !again.BuiltAt.Equal(s1.BuiltAt) {
		t.Fatalf("same brief again = %+v, %v; want not stored, the first row's age", again, err)
	}
	d.exec(t, "ALTER TABLE public.nopk ADD PRIMARY KEY (a)")
	second, err := BuildBrief(d.ctx, d.pool, target, BriefOptions{})
	if err != nil {
		t.Fatalf("BuildBrief after the fix: %v", err)
	}
	s2, err := StoreBrief(d.ctx, d.pool, second)
	if err != nil || !s2.Stored || s2.ID == s1.ID || second.Hash == first.Hash {
		t.Fatalf("resolved brief = %+v, %v; want a new row", s2, err)
	}
	if briefRows(t, d) != 2 {
		t.Fatalf("rows = %d, want the old brief kept beside the new", briefRows(t, d))
	}
	var hash string
	if err := d.pool.QueryRow(d.ctx, `SELECT hash FROM sage.upgrade_briefs WHERE id = $1`,
		s1.ID).Scan(&hash); err != nil || hash != first.Hash {
		t.Fatalf("old row hash = %q, %v; want %s", hash, err, first.Hash)
	}
	var latest, db string
	if err := d.pool.QueryRow(d.ctx, `SELECT hash, brief->>'database'
		FROM sage.upgrade_briefs WHERE database = $1 AND target_major = 18
		ORDER BY built_at DESC, id DESC LIMIT 1`, d.name).Scan(&latest, &db); err != nil ||
		latest != second.Hash || db != d.name {
		t.Fatalf("latest = %q of %q, %v; want the resolved brief", latest, db, err)
	}
}

func hasCheckBlocker(b Brief, id string) bool {
	for _, bl := range b.Blockers.Items {
		if bl.Check == id {
			return true
		}
	}
	return false
}

// The reindex list holds only indexes whose collation version changed: an
// index on an ICU collation whose recorded version matches stays out.
func TestBriefDB_ReindexListsOnlyChangedCollations(t *testing.T) {
	d := newDB(t, "")
	requireBelow18(t, d)
	d.exec(t, `CREATE TABLE public.notes (id int PRIMARY KEY,
		a text COLLATE "en-US-x-icu", b text COLLATE "de-x-icu", c text)`,
		"CREATE INDEX notes_a ON public.notes (a)",
		"CREATE INDEX notes_b ON public.notes (b)",
		"CREATE INDEX notes_c ON public.notes (c)",
		`UPDATE pg_catalog.pg_collation SET collversion = '0.0'
		 WHERE collname = 'en-US-x-icu'`)
	b, err := BuildBrief(d.ctx, d.pool, Target{Major: 18, Provider: "self-managed"},
		BriefOptions{})
	if err != nil {
		t.Fatalf("BuildBrief: %v", err)
	}
	re := b.CollationReindex
	if re.State != SectionKnown || re.Total != 1 || len(re.Items) != 1 ||
		re.Items[0].Index != "public.notes_a" {
		t.Fatalf("reindex = %+v, want only public.notes_a", re)
	}
}
