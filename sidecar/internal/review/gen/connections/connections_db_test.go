package connections

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/review/gen/connections"))
}

const (
	liveSync    = "review02e_sync"
	liveStagger = "review02e_stagger"
)

// livePool bootstraps the sage schema twice (the second run must be a
// clean no-op).
func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for i := 0; i < 2; i++ {
		if err := schema.Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap %d: %v", i+1, err)
		}
	}
	return pool, ctx
}

// openSessions opens n idle sessions named app.
func openSessions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, app string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		cfg := pool.Config().ConnConfig.Copy()
		cfg.RuntimeParams["application_name"] = app
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("connect as %s: %v", app, err)
		}
		t.Cleanup(func() { conn.Close(context.Background()) })
		if _, err := conn.Exec(ctx, "SELECT 1"); err != nil {
			t.Fatal(err)
		}
	}
}

// liveGroupsSQL and liveRolesSQL read what the collector's groups and
// cluster role counts hold, for the test's sessions.
const liveGroupsSQL = `SELECT COALESCE(usename, ''), COALESCE(application_name, ''),
       COALESCE(state, 'unknown'), count(*)::int,
       COALESCE(max(EXTRACT(EPOCH FROM (now() - state_change))), 0)::float8
  FROM pg_stat_activity
 WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()
   AND datname = current_database() AND application_name LIKE 'review02e%'
 GROUP BY 1, 2, 3`

const liveRolesSQL = `SELECT COALESCE(usename, ''), count(*)::int FROM pg_stat_activity
 WHERE backend_type = 'client backend' AND pid <> pg_backend_pid() GROUP BY 1`

// recordSample reads pg_stat_activity as the collector's workload_sessions
// step does (the app ages through the collector's own statement) and
// stores it as one workload_sessions snapshot.
func recordSample(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	ws := collector.WorkloadSessions{Long: []collector.WorkloadLongSession{}}
	rows, err := pool.Query(ctx, liveGroupsSQL)
	if err != nil {
		t.Fatal(err)
	}
	ws.Groups, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (
		collector.WorkloadGroup, error) {
		var g collector.WorkloadGroup
		err := r.Scan(&g.Role, &g.Application, &g.State, &g.Sessions, &g.MaxStateAgeSeconds)
		return g, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows, err = pool.Query(ctx, liveRolesSQL); err != nil {
		t.Fatal(err)
	}
	ws.Roles, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (
		collector.WorkloadRoleStats, error) {
		var s collector.WorkloadRoleStats
		return s, r.Scan(&s.Role, &s.ClusterSessions)
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows, err = pool.Query(ctx, collector.WorkloadAppAgesSQL); err != nil {
		t.Fatal(err)
	}
	if ws.AppAges, ws.AppAgesTruncated, err = collector.ScanWorkloadAppAges(rows); err != nil {
		t.Fatal(err)
	}
	doc, err := json.Marshal(ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := histstore.Resolve(pool).Exec(ctx, `INSERT INTO sage.snapshots
		(collected_at, category, data{dbcol}) VALUES (now(), 'workload_sessions',
		$1::jsonb{dbval})`, string(doc)); err != nil {
		t.Fatalf("store the sample: %v", err)
	}
}

// Integration: staggered and synchronized sessions opened by the test on a
// real server. The staggered application connects in 4 waves of 3 a
// little over a minute apart (25% a minute: no herd); the synchronized
// one connects 12 sessions at once (a herd in all 3 samples).
func TestDBSource_FindsTheSynchronizedHerdAndNotTheStaggeredSessions(t *testing.T) {
	pool, ctx := livePool(t)
	for wave := 0; wave < 4; wave++ {
		if wave > 0 {
			time.Sleep(61 * time.Second)
		}
		openSessions(t, ctx, pool, liveStagger, 3)
	}
	openSessions(t, ctx, pool, liveSync, 12)
	for i := 0; i < 3; i++ {
		recordSample(t, ctx, pool)
		time.Sleep(1100 * time.Millisecond)
	}
	now := time.Now().UTC()
	p, err := reviewtest.Build(reviewtest.Spec{Database: "db1", Connections: allHours(),
		Window: workpack.Window{From: now.AddDate(0, 0, -14), To: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	src := NewDBSource(histstore.Resolve(pool),
		budget.NewCatalog(pool, catalogread.Default()))
	in, err := src.Read(ctx, p.Window.From, p.Window.To)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	checkLiveInputs(t, ctx, pool, in)
	cands, _, err := Build(p, in, candidate.Env{Database: "db1", WindowDays: 14})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	herds := ofKind(cands, KindRecycleHerd)
	if len(herds) != 1 || herds[0].Target != "application:"+liveSync ||
		herds[0].Prediction.Quantities[QuantityHerdSessions] != 12 {
		t.Fatalf("herds %+v, want only %s with 12 sessions", herds, liveSync)
	}
}

// checkLiveInputs checks what the source read: 3 samples, this database,
// pg_sage's role, the test role's facts, its cluster peak and the mix.
func checkLiveInputs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, in Inputs) {
	t.Helper()
	var db, role string
	if err := pool.QueryRow(ctx, "SELECT current_database(), session_user::text").
		Scan(&db, &role); err != nil {
		t.Fatal(err)
	}
	if len(in.Samples) != 3 || in.WindowSamples != 3 || in.Database != db ||
		in.SelfRole != role {
		t.Fatalf("read %d samples of %d, database %q role %q; want 3, %q, %q",
			len(in.Samples), in.WindowSamples, in.Database, in.SelfRole, db, role)
	}
	if _, ok := in.Roles[role]; !ok {
		t.Fatalf("role facts %v lack %s", in.Roles, role)
	}
	i := slices.IndexFunc(in.RolePeaks, func(r RolePeak) bool { return r.Role == role })
	if i < 0 || in.RolePeaks[i].Sessions < 24 {
		t.Fatalf("role peaks %+v, want %s at 24 sessions or more", in.RolePeaks, role)
	}
	for _, m := range Mixes(in) {
		if m.Application == liveStagger && m.Idle != 12 {
			t.Fatalf("%s mix %+v, want 12 idle", liveStagger, m)
		}
	}
	if !slices.ContainsFunc(Mixes(in), func(m AppMix) bool {
		return m.Application == liveSync && m.Idle == 12
	}) {
		t.Fatalf("mixes %+v lack %s with 12 idle", Mixes(in), liveSync)
	}
}

// A window with no workload_sessions snapshots reads no samples: the
// generator says connections are missing.
func TestDBSource_EmptyWindowReadsNothing(t *testing.T) {
	pool, ctx := livePool(t)
	from := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	src := NewDBSource(histstore.Resolve(pool), nil)
	in, err := src.Read(ctx, from, from.AddDate(0, 0, 14))
	if err != nil || len(in.Samples) != 0 || in.WindowSamples != 0 || in.Roles != nil {
		t.Fatalf("empty window: %+v, %v", in, err)
	}
}

// Error propagation: a canceled context is the read's error.
func TestDBSource_CanceledContextIsAnError(t *testing.T) {
	pool, _ := livePool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := NewDBSource(histstore.Resolve(pool), nil)
	if _, err := src.Read(ctx, time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatal("canceled read: no error")
	}
}
