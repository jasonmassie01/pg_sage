//go:build integration

package logwatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func statusTestPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// Integration: a stock server logs to stderr, and detection says which
// setting to change rather than "need jsonlog or csvlog".
func TestDetectLogSettings_StderrServerNamesTheSetting(t *testing.T) {
	pool, ctx := statusTestPool(t)
	var dest, collector string
	if err := pool.QueryRow(ctx, `SELECT current_setting('log_destination'),
		current_setting('logging_collector')`).Scan(&dest, &collector); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if strings.Contains(dest, "jsonlog") || strings.Contains(dest, "csvlog") {
		t.Skipf("server logs %s: this test needs a stderr-only server", dest)
	}
	_, _, err := DetectLogSettings(ctx, pool)
	if err == nil || !errors.Is(err, ErrLogSourceUnsupported) ||
		!strings.Contains(err.Error(), "log_destination") {
		t.Fatalf("DetectLogSettings = %v, want an unsupported error naming log_destination", err)
	}
}

// Integration: the signal hints read pg_settings and agree with the server.
func TestDetectSignalHints_ReadsTheServerSettings(t *testing.T) {
	pool, ctx := statusTestPool(t)
	var tempFiles string
	if err := pool.QueryRow(ctx, "SHOW log_temp_files").Scan(&tempFiles); err != nil {
		t.Fatalf("SHOW log_temp_files: %v", err)
	}
	hints, err := DetectSignalHints(ctx, pool, false)
	if err != nil {
		t.Fatalf("DetectSignalHints: %v", err)
	}
	hinted := strings.Contains(strings.Join(hints, " "), "log_temp_files")
	if hinted != (tempFiles == "-1") {
		t.Fatalf("log_temp_files=%s but hints = %q", tempFiles, hints)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := DetectSignalHints(cancelled, pool, false); err == nil {
		t.Fatal("cancelled context gave no error")
	}
}
