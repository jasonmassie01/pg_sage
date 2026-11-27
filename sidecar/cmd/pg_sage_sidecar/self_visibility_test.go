package main

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
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
	// The tagger wraps the frontend's writer, not the network connection:
	// pgx keeps SCRAM channel binding only on a connection it holds as a
	// *tls.Conn, so a connection hook here would mean binding is lost.
	conn := &pool.Config().ConnConfig.Config
	if conn.AfterNetConnect != nil {
		t.Errorf("%s: a network connection hook is set: channel binding is lost", label)
	}
	for _, sent := range cliPoolSent(t, conn, "SELECT 1") {
		if sent != "SELECT /* pg_sage */ 1" {
			t.Errorf("%s: sent %q, want the statement tagged after its keyword", label, sent)
		}
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

// cliPoolSent returns the statement texts cfg's protocol frontend sends
// for queries, each as a Parse (extended protocol) then as a simple Query,
// after the startup message the tagger forwards untouched.
func cliPoolSent(t *testing.T, cfg *pgconn.Config, queries ...string) []string {
	t.Helper()
	if cfg.BuildFrontend == nil {
		t.Fatal("no BuildFrontend: the statements go out untagged")
	}
	var wire bytes.Buffer
	fe := cfg.BuildFrontend(bytes.NewReader(nil), &wire)
	fe.Send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters: map[string]string{"user": "sage"}})
	for _, q := range queries {
		fe.Send(&pgproto3.Parse{Query: q})
		fe.Send(&pgproto3.Query{String: q})
	}
	if err := fe.Flush(); err != nil {
		t.Fatalf("flush the frontend: %v", err)
	}
	be := pgproto3.NewBackend(&wire, io.Discard)
	if _, err := be.ReceiveStartupMessage(); err != nil {
		t.Fatalf("read the startup message back: %v", err)
	}
	var sent []string
	for range 2 * len(queries) {
		msg, err := be.Receive()
		if err != nil {
			t.Fatalf("read a sent message back: %v", err)
		}
		switch m := msg.(type) {
		case *pgproto3.Parse:
			sent = append(sent, m.Query)
		case *pgproto3.Query:
			sent = append(sent, m.String)
		default:
			t.Fatalf("sent %T, want Parse and Query only", msg)
		}
	}
	return sent
}
