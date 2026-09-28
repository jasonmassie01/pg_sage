package executor

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// Regression tests for G4-B21 / G4-B05: an allowlisted max_slot_wal_keep_size
// from any source must never undercut the WAL slots already retain.

func TestParseWALKeepSizeBytes(t *testing.T) {
	tests := map[string]int64{
		"ALTER SYSTEM SET max_slot_wal_keep_size = '10240MB'": 10240 << 20,
		"ALTER SYSTEM SET max_slot_wal_keep_size TO '2GB';":   2 << 30,
		"ALTER SYSTEM SET max_slot_wal_keep_size = 512":       512 << 20,
		"ALTER SYSTEM SET max_slot_wal_keep_size = -1":        -1,
	}
	for sql, want := range tests {
		got, ok, err := parseWALKeepSizeBytes(sql)
		if err != nil || !ok || got != want {
			t.Fatalf("parseWALKeepSizeBytes(%q) = %d, %v, %v; want %d", sql, got, ok, err, want)
		}
	}
	if _, ok, _ := parseWALKeepSizeBytes("ALTER SYSTEM SET work_mem = '64MB'"); ok {
		t.Fatal("work_mem treated as a WAL keep size")
	}
	if _, _, err := parseWALKeepSizeBytes(
		"ALTER SYSTEM SET max_slot_wal_keep_size = 'lots'"); !errors.Is(err, ErrUnsafeGUCValue) {
		t.Fatalf("unparsable keep size error = %v", err)
	}
}

func TestCustodianRefusesUnparsableWALKeepSize(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)

	err := exec.executeCustodianSQL(ctx,
		"ALTER SYSTEM SET max_slot_wal_keep_size = '1 zillion'", ActionPolicyDecision{})

	if !errors.Is(err, ErrUnsafeGUCValue) {
		t.Fatalf("executeCustodianSQL = %v, want ErrUnsafeGUCValue", err)
	}
}

func TestWALKeepSizeCheckAllowsHeadroom(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	var retained int64
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(max(
		pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)), 0)::bigint
		FROM pg_replication_slots`).Scan(&retained); err != nil {
		t.Fatalf("retained: %v", err)
	}
	safeMB := (retained*2)>>20 + 1024
	sql := "ALTER SYSTEM SET max_slot_wal_keep_size = '" +
		strconv.FormatInt(safeMB, 10) + "MB'"
	if err := exec.checkGUCValueSafety(ctx, sql); err != nil {
		t.Fatalf("headroom value refused: %v", err)
	}
}
