package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/clusterlock"
)

// ExclusiveEnvName names a second disposable PostgreSQL server on which
// only tests that measure or reset cluster-wide state run (WAL volume,
// checkpoints, XID consumption, shared statistics, pg_stat_statements,
// whole-cluster backend counts), one at a time. It must not be the server
// EnvName names: every other test package writes there concurrently.
const ExclusiveEnvName = "SAGE_TEST_EXCLUSIVE_URL"

// exclusiveLockName is the clusterlock every Exclusive caller holds on the
// exclusive server for the whole test.
const exclusiveLockName = "exclusive"

const (
	// exclusiveIdentityTimeout bounds reading one server's identity.
	exclusiveIdentityTimeout = 30 * time.Second
	// exclusiveLockWait bounds waiting for the measuring tests ahead.
	exclusiveLockWait = 20 * time.Minute
)

// Exclusive gives t a fresh database on the exclusive server and holds that
// server for t alone until t ends, so nothing else writes WAL, consumes
// XIDs or resets statistics there while t measures. When the test ends its
// database is dropped, then the server is released. It skips with an
// "exclusive-cluster:" reason (never allow-listed, so the CI skip budget
// fails) when ExclusiveEnvName is unset, and fails when that server cannot
// be proven to be a different cluster from the designated one.
func Exclusive(t testing.TB, reason string) string {
	t.Helper()
	raw := exclusiveURL(t, reason)
	ctx, cancel := context.WithTimeout(context.Background(), exclusiveIdentityTimeout)
	defer cancel()
	base, err := exclusiveTarget(ctx, raw, os.Getenv(EnvName))
	if err != nil {
		t.Fatalf("exclusive-cluster: %s: %v", reason, err)
	}
	lockCtx, cancelLock := context.WithTimeout(context.Background(), exclusiveLockWait)
	defer cancelLock()
	release, err := clusterlock.Lock(lockCtx, base, exclusiveLockName)
	if err != nil {
		t.Fatalf("exclusive-cluster: %s: wait for the exclusive test server: %v", reason, err)
	}
	t.Cleanup(release)
	name, err := extraDatabaseName("exclusive")
	if err != nil {
		t.Fatalf("exclusive-cluster: name the test database: %v", err)
	}
	dsn, err := createFixture(base, name, "exclusive")
	if err != nil {
		t.Fatalf("exclusive-cluster: %s: %v", reason, err)
	}
	t.Cleanup(func() {
		if err := dropFixture(base, name); err != nil {
			t.Errorf("exclusive-cluster: drop the exclusive test database: %v", err)
		}
	})
	exclusiveHolders.Store(t, base)
	t.Cleanup(func() { exclusiveHolders.Delete(t) })
	return dsn
}

// exclusiveURL returns ExclusiveEnvName, skipping t when it is unset.
func exclusiveURL(t testing.TB, reason string) string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(ExclusiveEnvName))
	if raw == "" {
		t.Skipf("exclusive-cluster: %s needs %s", reason, ExclusiveEnvName)
	}
	return raw
}

// exclusiveTarget checks the exclusive server URL and that it names a
// different cluster from the designated server, and returns it.
func exclusiveTarget(ctx context.Context, exclusiveRaw, designatedRaw string) (string, error) {
	exclusive, err := parseServerURL(ExclusiveEnvName, exclusiveRaw)
	if err != nil {
		return "", err
	}
	designatedRaw = strings.TrimSpace(designatedRaw)
	if designatedRaw == "" || designatedRaw == disabledDSN {
		return "", fmt.Errorf("%s is unset, so the same cluster check cannot prove %s "+
			"names another server; set both", EnvName, ExclusiveEnvName)
	}
	designated, err := parseServerURL(EnvName, designatedRaw)
	if err != nil {
		return "", err
	}
	exclusiveID, err := systemIdentifier(ctx, exclusive, "exclusive")
	if err != nil {
		return "", err
	}
	designatedID, err := systemIdentifier(ctx, designated, "designated")
	if err != nil {
		return "", err
	}
	if exclusiveID == designatedID {
		return "", fmt.Errorf("%s and %s name the same cluster (system_identifier %d); "+
			"the exclusive server must be a separate server that nothing else uses",
			ExclusiveEnvName, EnvName, exclusiveID)
	}
	return exclusive, nil
}

// parseServerURL accepts only a postgres:// or postgresql:// URL, which
// fixtureDSN can point at a fixture database. Errors never carry the URL
// (it may hold a password).
func parseServerURL(envName, raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse %s: not a URL", envName)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return "", fmt.Errorf("parse %s: want a postgres:// URL, got scheme %q",
			envName, parsed.Scheme)
	}
	if _, err := pgx.ParseConfig(raw); err != nil {
		return "", fmt.Errorf("parse %s: not a PostgreSQL connection URL", envName)
	}
	return raw, nil
}

// systemIdentifier reads the cluster identity that initdb chose: equal on
// every database of one cluster (and on its physical replicas).
func systemIdentifier(ctx context.Context, dsn, server string) (int64, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return 0, fmt.Errorf("parse %s test server URL: not a PostgreSQL connection URL",
			server)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return 0, fmt.Errorf("connect %s test server %s:%d: %w",
			server, cfg.Host, cfg.Port, err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var id int64
	err = conn.QueryRow(ctx, "SELECT system_identifier FROM pg_control_system()").Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("read system_identifier on the %s test server %s:%d: %w",
			server, cfg.Host, cfg.Port, err)
	}
	return id, nil
}
