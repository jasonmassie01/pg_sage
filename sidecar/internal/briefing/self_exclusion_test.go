package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/selfload"
)

// exclusiveBriefingDB is requireDB (one connection, bootstrapped) on a
// fresh database of the exclusive test server, held for t alone
// (testdb.Exclusive), and that database's DSN (ci-13).
func exclusiveBriefingDB(t *testing.T, reason string) (*pgxpool.Pool, context.Context, string) {
	t.Helper()
	dsn := testdb.Exclusive(t, reason)
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the exclusive test DSN: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect the exclusive test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap the exclusive test database: %v", err)
	}
	if err := schema.MigrateConfigSchema(ctx, pool); err != nil {
		t.Fatalf("config migration on the exclusive test database: %v", err)
	}
	return pool, ctx, dsn
}

// The briefing's system section reports the application's active
// sessions; pg_sage's own are not workload (perf v1.8.3, perf-selfexcl).
// The count is server-wide, so the test compares minimums of several
// samples, on the exclusive server. (Connections stay every slot in use:
// capacity, not workload.)
func TestGatherSystemActiveLeavesOutPgSage(t *testing.T) {
	pool, ctx, dsn := exclusiveBriefingDB(t, "the whole cluster's active session count")
	w := selfload.New(t, dsn)
	w.StartApp(t)
	worker := New(pool, &config.Config{}, nil, func(string, string, ...any) {})
	active := func() (float64, error) {
		raw, err := worker.gatherSystem(ctx)
		if err != nil {
			return 0, err
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			return 0, err
		}
		n, ok := doc["active"].(float64)
		if !ok {
			return 0, fmt.Errorf("briefing system active = %v", doc["active"])
		}
		return n, nil
	}
	before := selfload.MinOver(t, 8, active)
	w.StartSage(t, 6) // 8 active pg_sage sessions
	after := selfload.MinOver(t, 8, active)
	if before < 2 || after >= before+4 {
		t.Fatalf("briefing active %.0f -> %.0f after 8 active pg_sage sessions started, "+
			"want them left out", before, after)
	}
}
