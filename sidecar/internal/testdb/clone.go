package testdb

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// sqlStateObjectInUse is CREATE DATABASE ... TEMPLATE refusing a template
// another session (often an autovacuum worker) is connected to.
const sqlStateObjectInUse = "55006"

// CloneDatabase creates one extra disposable database as a copy of
// template, a fixture database of this package process (CREATE DATABASE
// ... TEMPLATE), and force-drops it when the test ends. Copying a built
// fixture is much faster than building it again for every test. Nothing
// may stay connected to template while it is copied: a copy refused
// because another session is connected is retried for up to 30 s. It
// skips when no live server is configured.
func CloneDatabase(t testing.TB, label, template string) string {
	t.Helper()
	base := SkipUnlessLive(t)
	if err := validateFixtureDatabaseName(template); err != nil {
		t.Fatalf("clone test database: %v", err)
	}
	name, err := extraDatabaseName(label)
	if err != nil {
		t.Fatalf("name cloned test database: %v", err)
	}
	admin, err := adminDSN(base)
	if err != nil {
		t.Fatalf("clone test database: %v", err)
	}
	if err := cloneFixture(admin, template, name); err != nil {
		t.Fatalf("clone test database %s from %s: %v", name, template, err)
	}
	t.Cleanup(func() {
		if err := dropFixture(admin, name); err != nil {
			t.Errorf("drop cloned test database: %v", err)
		}
	})
	dsn, err := fixtureDSN(base, name)
	if err != nil {
		t.Fatalf("cloned test database DSN: %v", err)
	}
	return dsn
}

// DatabaseName is the database a DSN names.
func DatabaseName(dsn string) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	return cfg.Database, nil
}

// adminDSN is base pointed at the server's postgres database, so the
// admin session is never connected to the template it copies.
func adminDSN(base string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse designated test DSN: %w", err)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return "", errors.New("designated test DSN must be a postgres URL")
	}
	parsed.Path = "/postgres"
	return parsed.String(), nil
}

func cloneFixture(admin, template, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return fmt.Errorf("connect designated test server: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	sql := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize() + " TEMPLATE " +
		pgx.Identifier{template}.Sanitize()
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err = conn.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != sqlStateObjectInUse ||
			time.Now().After(deadline) {
			return err
		}
		if err := sleepCtx(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}
