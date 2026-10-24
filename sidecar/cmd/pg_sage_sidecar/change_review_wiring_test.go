package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Change Review runtime wiring (CHECK-RSN-10): every database runtime
// registers its own service (impact engine and reasoner on its own pool)
// in the process-wide registry the API and MCP route through, notes
// 'change_review', and unregisters it when the database is removed.

func wiringBaseConfig(t *testing.T, dsn string) *config.Config {
	t.Helper()
	conn, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	base := config.DefaultConfig()
	base.Trust.Level = "advisory"
	base.Collector.IntervalSeconds, base.Analyzer.IntervalSeconds = 3600, 3600
	base.LLM.Enabled = false
	base.Postgres = config.PostgresConfig{Host: conn.Host, Port: int(conn.Port),
		User: conn.User, Password: conn.Password, Database: conn.Database,
		SSLMode: "disable", MaxConnections: 6}
	return base
}

func TestChangeReviewWiring_StandaloneRegistersTheService(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	base := wiringBaseConfig(t, dsn)
	captureParityRuntimes(t)
	inst := buildStandaloneParity(t, base, dsn)
	if inst == nil {
		t.Fatal("standalone registered no runtime")
	}
	rt := parityRuntimes[inst.Name]
	if rt == nil || !hasFeature(rt.features, "change_review") {
		t.Fatalf("runtime features %v lack change_review", rt.features)
	}
	svc, err := changeReviewServices().Service(inst.Name)
	if err != nil || svc.Database() != inst.Name {
		t.Fatalf("registry service for %s: %v (%v)", inst.Name, svc, err)
	}
	if !svc.Config().Enabled {
		t.Fatal("the registered service does not carry change_review.* config")
	}
}

func hasFeature(features []string, want string) bool {
	for _, f := range features {
		if f == want {
			return true
		}
	}
	return false
}

// createWiringDatabase creates one extra database for the fleet test and
// force-drops it when the test ends.
func createWiringDatabase(t *testing.T, dsn, label string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("crw_%d_%s", os.Getpid(), label)
	admin := *u
	admin.Path = "/postgres"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		t.Fatalf("drop stale %s: %v", name, err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c, err := pgx.Connect(ctx, admin.String())
		if err != nil {
			t.Errorf("connect admin for cleanup: %v", err)
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	return name
}

func wiringPool(t *testing.T, dsn, database string) *pgxpool.Pool {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + database
	p, err := pgxpool.New(context.Background(), u.String())
	if err != nil {
		t.Fatalf("connect %s: %v", database, err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestChangeReviewWiring_FleetKeepsEachDatabaseApart(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	one, two := createWiringDatabase(t, dsn, "db1"), createWiringDatabase(t, dsn, "db2")
	p1, p2 := wiringPool(t, dsn, one), wiringPool(t, dsn, two)
	for p, table := range map[*pgxpool.Pool]string{p1: "crw_only_one", p2: "crw_only_two"} {
		if _, err := p.Exec(ctx, "CREATE TABLE public."+table+
			" (id bigint PRIMARY KEY, note text)"); err != nil {
			t.Fatal(err)
		}
	}
	base := wiringBaseConfig(t, dsn)
	preserveParityGlobals(t, base)
	cfg.Mode = "fleet"
	db := func(name, database string) config.DatabaseConfig {
		return config.DatabaseConfig{Name: name, Host: base.Postgres.Host,
			Port: base.Postgres.Port, User: base.Postgres.User,
			Password: base.Postgres.Password, Database: database, SSLMode: "disable",
			MaxConnections: 4}
	}
	cfg.Databases = []config.DatabaseConfig{db("db1", one), db("db2", two)}
	initFleetMultiDB()
	t.Cleanup(func() {
		for _, p := range []*pgxpool.Pool{p1, p2} {
			_, _ = p.Exec(context.Background(),
				"DELETE FROM sage.databases WHERE name IN ('db1', 'db2')")
		}
	})
	svc1, err := changeReviewServices().Service("db1")
	if err != nil {
		t.Fatalf("db1 service: %v", err)
	}
	if _, err := changeReviewServices().Service("db2"); err != nil {
		t.Fatalf("db2 service: %v", err)
	}
	got, _, err := svc1.Submit(ctx, changereview.Caller{Actor: "user:1",
		Source: changereview.SourceAPI}, changereview.Request{
		SQL: "ALTER TABLE public.crw_only_one ADD COLUMN flag boolean", WaitSeconds: 50})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got.State != changereview.StateComplete {
		if got, err = svc1.Wait(ctx, got.ID, time.Minute); err != nil ||
			got.State != changereview.StateComplete {
			t.Fatalf("review %s: %s (%v)", got.ID, got.State, err)
		}
	}
	var name, evidence string
	if err := p1.QueryRow(ctx, `SELECT database_name, evidence::text
		FROM sage.change_review WHERE id = $1`, got.ID).Scan(&name, &evidence); err != nil {
		t.Fatalf("db1 row: %v", err)
	}
	if name != "db1" || !strings.Contains(evidence, "public.crw_only_one") ||
		strings.Contains(evidence, "crw_only_two") {
		t.Fatalf("db1 stored database_name %q evidence %s", name, evidence)
	}
	var inTwo int
	if err := p2.QueryRow(ctx, `SELECT count(*) FROM sage.change_review`).Scan(&inTwo); err != nil ||
		inTwo != 0 {
		t.Fatalf("db2 holds %d reviews (%v), want 0", inTwo, err)
	}
	if !fleetMgr.RemoveInstance("db1") {
		t.Fatal("db1 was not registered in the fleet")
	}
	if _, err := changeReviewServices().Service("db1"); !errors.Is(err,
		changereview.ErrNotFound) {
		t.Fatalf("db1 still has a service after removal: %v", err)
	}
	if _, err := changeReviewServices().Service("db2"); err != nil {
		t.Fatalf("removing db1 unregistered db2: %v", err)
	}
}

func TestChangeReviewModel_ResolvesThePurposeClient(t *testing.T) {
	if m := changeReviewModel(nil); m != nil {
		t.Fatalf("no manager: %v, want no model", m)
	}
	if m := changeReviewModel(llm.NewManager(nil, nil, false)); m != nil {
		t.Fatalf("no general client: %#v, want no model", m)
	}
	general := llm.New(&config.LLMConfig{Enabled: true, Endpoint: "http://127.0.0.1:1",
		Model: "m"}, func(string, string, ...any) {})
	optimizer := llm.New(&config.LLMConfig{Enabled: true, Endpoint: "http://127.0.0.1:2",
		Model: "o"}, func(string, string, ...any) {})
	if m := changeReviewModel(llm.NewManager(general, optimizer, false)); m != general {
		t.Fatalf("change_review resolved to %v, want the general client", m)
	}
}
