package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Product decision (perf v1.8.3): pg_sage stops hiding its own statements
// from pg_stat_statements. Every pool it opens is named pg_sage, tags its
// statements and leaves pg_stat_statements.track at the server's setting.
func assertSelfVisiblePool(t *testing.T, pool *pgxpool.Pool, label string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if pool.Config().ConnConfig.AfterNetConnect == nil {
		t.Errorf("%s: no statement tagging hook", label)
	}
	var app, track string
	err := pool.QueryRow(ctx, `SELECT current_setting('application_name'),
		COALESCE(current_setting('pg_stat_statements.track', true), 'unset')`).
		Scan(&app, &track)
	if err != nil {
		t.Fatalf("%s: read session settings: %v", label, err)
	}
	if app != selfmonitor.ApplicationName {
		t.Errorf("%s: application_name = %q, want %q", label, app,
			selfmonitor.ApplicationName)
	}
	if track == "none" {
		t.Errorf("%s: pg_stat_statements.track = none (pg_sage hides its cost)", label)
	}
}

func TestMonitoredPoolIsSelfVisible(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	pool, err := connectMonitoredDBContext(context.Background(), dsn, 2)
	if err != nil {
		t.Fatalf("connect monitored pool: %v", err)
	}
	defer pool.Close()
	assertSelfVisiblePool(t, pool, "monitored pool")
}

func TestMetaPoolIsSelfVisible(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	pool, err := connectMetaDB(dsn)
	if err != nil {
		t.Fatalf("connect meta pool: %v", err)
	}
	defer pool.Close()
	assertSelfVisiblePool(t, pool, "meta pool")
}

func TestFleetPoolIsSelfVisible(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	parsed, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	pool, err := openFleetPool(config.DatabaseConfig{
		Name: "selfvis", Host: parsed.Host, Port: int(parsed.Port),
		User: parsed.User, Password: parsed.Password, Database: parsed.Database,
		SSLMode: "disable", MaxConnections: 2,
	})
	if err != nil {
		t.Fatalf("open fleet pool: %v", err)
	}
	defer pool.Close()
	assertSelfVisiblePool(t, pool, "fleet pool")
}
