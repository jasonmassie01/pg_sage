package budget

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// market-08 on a live server: the shadow ledger's upsert semantics, the
// per-role evidence and the role catalog read.

func sagePool(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

// Would-cancels add up within a day; would-rejects and the worst
// statement keep the day's largest; first_seen keeps the first write; a
// new day is a new row; databases are kept apart.
func TestStore_RecordUpsertsPerDay(t *testing.T) {
	pool := sagePool(t, "budgetstore")
	ctx := context.Background()
	s, other := NewStore(pool, "shop"), NewStore(pool, "billing")
	day := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	steps := [][]Tally{
		{{Budget: "app", WouldCancel: 1, WouldReject: 3, WorstMs: 7000}},
		{{Budget: "app", WouldCancel: 2, WouldReject: 1, ProtectedCancels: 1, WorstMs: 4000}},
	}
	for _, ts := range steps {
		if err := s.Record(ctx, day, ts); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if err := s.Record(ctx, day.Add(24*time.Hour), []Tally{{Budget: "app"}}); err != nil {
		t.Fatal(err)
	}
	if err := other.Record(ctx, day, []Tally{{Budget: "app", WouldCancel: 50}}); err != nil {
		t.Fatal(err)
	}
	h, err := s.History(ctx, "app", "", day.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 {
		t.Fatalf("history = %+v, want two days", h)
	}
	d := h[0]
	if !d.Day.Equal(day) || d.WouldCancel != 3 || d.WouldReject != 3 ||
		d.ProtectedCancels != 1 || d.WorstMs != 7000 || d.FirstSeen.IsZero() {
		t.Fatalf("day one = %+v, want cancels 3, rejects 3, protected 1, worst 7000", d)
	}
	if h[1].WouldCancel != 0 || h[1].FirstSeen.Before(d.FirstSeen) {
		t.Fatalf("day two = %+v", h[1])
	}
	since, _ := s.History(ctx, "app", "", day.Add(24*time.Hour))
	if len(since) != 1 {
		t.Fatalf("history since day two = %+v, want one row", since)
	}
	assertDefinitionsApart(t, s, day)
}

// A changed budget definition is a separate ledger on the same day.
func assertDefinitionsApart(t *testing.T, s *Store, day time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := s.Record(ctx, day, []Tally{{Budget: "app", Definition: "tight",
		WouldCancel: 9}}); err != nil {
		t.Fatalf("record the tight definition: %v", err)
	}
	tight, err := s.History(ctx, "app", "tight", day.Add(-24*time.Hour))
	if err != nil || len(tight) != 1 || tight[0].WouldCancel != 9 {
		t.Fatalf("tight history = %+v, %v, want its own row of 9", tight, err)
	}
	loose, _ := s.History(ctx, "app", "", day)
	if len(loose) != 2 || loose[0].WouldCancel != 3 {
		t.Fatalf("loose history = %+v, want its two rows unchanged", loose)
	}
}

// Per-role evidence keeps the largest daily p99 and longest statement
// over the window, ignores roles with no statements, and is per database.
func TestStore_RoleEvidence(t *testing.T) {
	pool := sagePool(t, "budgetroles")
	ctx := context.Background()
	s := NewStore(pool, "shop")
	day := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	rows := []collector.WorkloadRoleStats{{Role: "app", Calls: 100, P99EstimateMs: 2000,
		MaxExecMs: 2500}, {Role: "idle", ClusterSessions: 4}}
	if err := s.RecordRoles(ctx, day, rows); err != nil {
		t.Fatal(err)
	}
	rows[0].P99EstimateMs, rows[0].MaxExecMs, rows[0].Calls = 1500, 9000, 150
	if err := s.RecordRoles(ctx, day.Add(-20*24*time.Hour), rows); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRoles(ctx, day.Add(-24*time.Hour), rows); err != nil {
		t.Fatal(err)
	}
	ev, err := s.RoleEvidence(ctx, day.Add(-14*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev["app"] != (RoleEvidence{P99Ms: 2000, MaxMs: 9000, Calls: 150}) {
		t.Fatalf("evidence = %+v, want app p99 2000 max 9000 calls 150", ev)
	}
	if err := s.RecordRoles(ctx, day, nil); err != nil {
		t.Fatalf("no roles: %v", err)
	}
}

// The role catalog reads superuser, connection limit and the role's
// settings in this database only.
func TestCatalog_RoleFacts(t *testing.T) {
	pool := sagePool(t, "budgetcatalog")
	ctx := context.Background()
	testdb.LockAgentRoles(t)
	for _, sql := range []string{
		`DROP ROLE IF EXISTS budget_cat_app`,
		`CREATE ROLE budget_cat_app LOGIN CONNECTION LIMIT 7`,
		`ALTER ROLE budget_cat_app SET work_mem = '8MB'`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP ROLE budget_cat_app`) })
	var db string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, RoleSettingSQL("budget_cat_app", db,
		SettingStatementTimeout, 6000)); err != nil {
		t.Fatalf("set role timeout: %v", err)
	}
	c := NewCatalog(pool, catalogread.Default())
	facts, err := c.RoleFacts(ctx, []string{"budget_cat_app", "no_such_role"})
	if err != nil {
		t.Fatal(err)
	}
	f, ok := facts["budget_cat_app"]
	if !ok || len(facts) != 1 || f.Superuser || f.ConnLimit != 7 ||
		f.Settings[SettingStatementTimeout] != "6000ms" || f.Settings["work_mem"] != "" {
		t.Fatalf("facts = %+v, want limit 7 and only this database's 6000ms", facts)
	}
	self, err := c.SelfRole(ctx)
	if err != nil || self == "" {
		t.Fatalf("self role %q: %v", self, err)
	}
	got, err := c.CurrentDatabase(ctx)
	if err != nil || got != db {
		t.Fatalf("current database %q (%v), want %s", got, err, db)
	}
}
