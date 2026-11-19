//go:build integration

package discovery

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/fleet/discovery"))
}

var dbTestKey = crypto.DeriveKey("discovery-test-key", []byte("discovery-test-salt"))

type liveEnv struct {
	pool     *pgxpool.Pool
	ctx      context.Context
	dsn      *url.URL
	ds       *store.DatabaseStore
	disc     *store.DiscoveryStore
	memberID int
}

func requireLive(t *testing.T) liveEnv {
	t.Helper()
	return requireLiveOn(t, testdb.SkipUnlessLive(t))
}

// requireExclusiveLive is requireLive on the exclusive server, for a test
// whose cycle lists every database of the cluster: on the shared server
// other packages' databases fill the per-cycle enrolment caps.
func requireExclusiveLive(t *testing.T, reason string) liveEnv {
	t.Helper()
	return requireLiveOn(t, testdb.Exclusive(t, reason))
}

func requireLiveOn(t *testing.T, raw string) liveEnv {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, raw)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.discovered_databases;
		DELETE FROM sage.discovery_sources; DELETE FROM sage.databases`); err != nil {
		t.Fatalf("clean: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	env := liveEnv{pool: pool, ctx: ctx, dsn: u,
		ds: store.NewDatabaseStore(pool, dbTestKey), disc: store.NewDiscoveryStore(pool)}
	env.memberID = env.createMember(t)
	return env
}

// createMember registers the fixture database itself as the cluster member.
func (e liveEnv) createMember(t *testing.T) int {
	t.Helper()
	port, _ := strconv.Atoi(e.dsn.Port())
	if port == 0 {
		port = 5432
	}
	pw, _ := e.dsn.User.Password()
	ssl := e.dsn.Query().Get("sslmode")
	if ssl == "" {
		ssl = "prefer"
	}
	id, err := e.ds.Create(e.ctx, store.DatabaseInput{Name: "main", Host: e.dsn.Hostname(),
		Port: port, DatabaseName: strings.TrimPrefix(e.dsn.Path, "/"),
		Username: e.dsn.User.Username(), Password: pw, SSLMode: ssl, MaxConnections: 2,
		TrustLevel: "observation", ExecutionMode: "approval"}, 1)
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	return id
}

// liveMembers enrols through the real store, as the sidecar does.
type liveMembers struct {
	env      liveEnv
	mu       sync.Mutex
	disabled []int
}

func (m *liveMembers) Enrol(ctx context.Context, from string, row store.DiscoveredRow,
	expiresAt time.Time) (int, error) {
	return m.env.ds.CreateDiscovered(ctx, m.env.memberID, store.DiscoveredMember{
		Name: MemberName(row), DatabaseName: row.DBName, Source: row.Source,
		Owner: row.Owner, ExpiresAt: expiresAt, Tags: row.Tags})
}

func (m *liveMembers) Disable(ctx context.Context, databaseID int) error {
	m.mu.Lock()
	m.disabled = append(m.disabled, databaseID)
	m.mu.Unlock()
	return m.env.ds.DisableDiscovered(ctx, databaseID)
}

func (e liveEnv) clusterRunner(t *testing.T, members Members) *Runner {
	t.Helper()
	port, _ := strconv.Atoi(e.dsn.Port())
	src := NewClusterSource("cluster", e.dsn.Hostname(), port,
		func() (Beginner, error) { return e.pool, nil }, CycleCap)
	r, err := NewRunner(config.FleetDiscoveryConfig{Enabled: true, MaxEnrolled: 10,
		TTLHours: 72}, []Binding{{Source: src, Owner: "team-db", EnrolFrom: "main"}},
		e.disc, members)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

func dbName(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return strings.TrimPrefix(u.Path, "/")
}

func ownCandidates(c []Candidate, names ...string) []string {
	var out []string
	for _, x := range c {
		for _, n := range names {
			if x.DBName == n {
				out = append(out, n)
			}
		}
	}
	return out
}

// Happy path: a cluster with three databases (the member and two more)
// yields two candidates; templates and the member itself are excluded.
func TestClusterSource_ExcludesTemplatesAndTheMember(t *testing.T) {
	env := requireLive(t)
	a := dbName(t, testdb.CreateDatabase(t, "disc_a"))
	b := dbName(t, testdb.CreateDatabase(t, "disc_b"))
	port, _ := strconv.Atoi(env.dsn.Port())
	src := NewClusterSource("cluster", env.dsn.Hostname(), port,
		func() (Beginner, error) { return env.pool, nil }, CycleCap)
	got, err := src.List(env.ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	member := strings.TrimPrefix(env.dsn.Path, "/")
	if own := ownCandidates(got, a, b, member, "template0", "template1"); len(own) != 2 {
		t.Fatalf("candidates among {a, b, member, templates} = %v, want [a b]", own)
	}
	for _, c := range got {
		if !strings.HasPrefix(c.ExternalID, env.dsn.Hostname()+":") ||
			!strings.HasSuffix(c.ExternalID, "/"+c.DBName) || c.Port != port {
			t.Fatalf("candidate identity = %+v", c)
		}
	}
	capped, err := NewClusterSource("cluster", env.dsn.Hostname(), port,
		func() (Beginner, error) { return env.pool, nil }, 1).List(env.ctx)
	if err != nil || len(capped) != 1 {
		t.Fatalf("cap 1 listing = %d %v", len(capped), err)
	}
}

// Integration + state transitions: a CREATE DATABASE appears on the next
// cycle and is enrolled at observation; it then goes gone when dropped.
// Trust is still observation after 10 cycles.
func TestRunner_CreateDatabaseAppearsOnNextCycle(t *testing.T) {
	env := requireLive(t)
	members := &liveMembers{env: env}
	r := env.clusterRunner(t, members)
	first := r.Cycle(env.ctx)
	if first.Sources["cluster"].Err != nil {
		t.Fatalf("first cycle: %v", first.Sources["cluster"].Err)
	}
	name := dbName(t, testdb.CreateDatabase(t, "disc_new"))
	rep := r.Cycle(env.ctx)
	if rep.Sources["cluster"].New < 1 {
		t.Fatalf("new database not discovered: %+v", rep.Sources["cluster"])
	}
	row := findRow(t, env, name)
	if row.Status != store.DiscoveryEnrolled || row.DatabaseID == nil {
		t.Fatalf("row = %+v, want enrolled", row)
	}
	for range 10 {
		r.Cycle(env.ctx)
	}
	rec, err := env.ds.Get(env.ctx, *row.DatabaseID)
	if err != nil || rec.TrustLevel != "observation" || rec.ExecutionMode != "approval" ||
		rec.Owner != "team-db" || rec.DiscoveredSource != "cluster" {
		t.Fatalf("member after 10 cycles = %+v %v", rec, err)
	}
	if _, err := env.pool.Exec(env.ctx, `DROP DATABASE `+quoteIdent(name)+
		` WITH (FORCE)`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	r.Cycle(env.ctx)
	if got := findRow(t, env, name); got.Status != store.DiscoveryGone {
		t.Fatalf("dropped database status = %q, want gone", got.Status)
	}
	rec, _ = env.ds.Get(env.ctx, *row.DatabaseID)
	if rec.Enabled {
		t.Fatal("gone member still enabled")
	}
}

// Concurrency: two sidecars running cycles at once make no duplicate rows
// and enrol each database once.
func TestRunner_TwoSidecarsNoDuplicates(t *testing.T) {
	env := requireLive(t)
	name := dbName(t, testdb.CreateDatabase(t, "disc_dup"))
	r1 := env.clusterRunner(t, &liveMembers{env: env})
	r2 := env.clusterRunner(t, &liveMembers{env: env})
	var wg sync.WaitGroup
	for _, r := range []*Runner{r1, r2, r1, r2} {
		wg.Add(1)
		go func(r *Runner) { defer wg.Done(); r.Cycle(env.ctx) }(r)
	}
	wg.Wait()
	var rows, members int
	_ = env.pool.QueryRow(env.ctx, `SELECT count(*) FROM sage.discovered_databases
		WHERE dbname = $1`, name).Scan(&rows)
	_ = env.pool.QueryRow(env.ctx, `SELECT count(*) FROM sage.databases
		WHERE database_name = $1`, name).Scan(&members)
	if rows != 1 || members != 1 {
		t.Fatalf("rows=%d members=%d for %s, want 1 and 1", rows, members, name)
	}
}

func findRow(t *testing.T, env liveEnv, dbname string) store.DiscoveredRow {
	t.Helper()
	rows, err := env.disc.List(env.ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range rows {
		if r.DBName == dbname {
			return r
		}
	}
	t.Fatalf("no discovered row for %s", dbname)
	return store.DiscoveredRow{}
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Bootstrap runs twice cleanly: one guard trigger, both tables present.
func TestBootstrapTwice_DiscoverySchema(t *testing.T) {
	env := requireLive(t) // the first Bootstrap ran here
	if err := schema.Bootstrap(env.ctx, env.pool); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	var triggers int
	var tables int
	_ = env.pool.QueryRow(env.ctx, `SELECT count(*) FROM pg_trigger
		WHERE tgrelid = 'sage.databases'::regclass
		  AND tgname = 'databases_discovered_trust_guard'`).Scan(&triggers)
	_ = env.pool.QueryRow(env.ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'sage'
		AND tablename IN ('discovered_databases', 'discovery_sources')`).Scan(&tables)
	if triggers != 1 || tables != 2 {
		t.Fatalf("after two boots: triggers=%d tables=%d", triggers, tables)
	}
}
