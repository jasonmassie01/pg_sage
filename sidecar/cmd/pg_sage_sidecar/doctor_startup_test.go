package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: the startup line is logged once per
// database from that database's own start-up path.

// Nil/empty: no pool logs nothing.
func TestLogDoctorCountsNilPool(t *testing.T) {
	var info, warn warnRecorder
	logDoctorCounts(context.Background(), nil, "app", info.warn, warn.warn)
	if len(info.lines)+len(warn.lines) != 0 {
		t.Fatalf("logged %q %q for no pool", info.lines, warn.lines)
	}
}

// Error propagation: an unreachable database logs one WARN naming the
// failed check and the command that lists the fixes.
func TestLogDoctorCountsUnreachableWarnsOnce(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://u:pw-not-logged@127.0.0.1:1/x?connect_timeout=2")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	var info, warn warnRecorder
	logDoctorCounts(context.Background(), pool, "app", info.warn, warn.warn)
	if len(info.lines) != 0 || len(warn.lines) != 1 {
		t.Fatalf("info %q warn %q, want exactly one warning", info.lines, warn.lines)
	}
	line := warn.lines[0]
	for _, want := range []string{`"app"`, "0 pass", "1 fail", "6 unknown",
		"failed: connection", "pg_sage doctor"} {
		if !strings.Contains(line, want) {
			t.Fatalf("warning %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "pw-not-logged") {
		t.Fatalf("warning carries the password: %q", line)
	}
}

// Happy path (DB): a superuser on the test server logs the counts at
// INFO, the model, notification and meta checks left out.
func TestLogDoctorCountsDBHealthyIsInfo(t *testing.T) {
	pool := testdb.PoolFor(t, testdb.CreateDatabase(t, "doctor_startup"))
	var info, warn warnRecorder
	logDoctorCounts(context.Background(), pool, "app", info.warn, warn.warn)
	if len(warn.lines) != 0 || len(info.lines) != 1 {
		t.Fatalf("info %q warn %q, want one INFO line", info.lines, warn.lines)
	}
	if !strings.Contains(info.lines[0], "0 fail") || !strings.Contains(info.lines[0],
		"0 unknown") || !strings.Contains(info.lines[0], "provider SM") {
		t.Fatalf("info line %q, want 0 fail, 0 unknown and provider SM", info.lines[0])
	}
}
