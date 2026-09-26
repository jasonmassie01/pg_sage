package collector

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// No concurrent-access cases: this probe reads deterministic SQL in an isolated DB.
func TestAuditCollectorCacheRatioUsesFractionContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// Keep the real collector expression; replace only the system catalog input.
	source := `(SELECT current_database() AS datname, 800::bigint AS blks_hit,
		200::bigint AS blks_read, 0::bigint AS deadlocks,
		0::float8 AS blk_read_time, 0::float8 AS blk_write_time) AS pg_stat_database`
	query := strings.ReplaceAll(systemStatsSQLBase, "FROM pg_stat_database", "FROM "+source)
	query += "0::bigint AS total_checkpoints, false AS is_replica, 0::bigint AS db_size_bytes"
	var s SystemStats
	err = pool.QueryRow(ctx, query).Scan(&s.ActiveBackends, &s.IdleInTransaction,
		&s.TotalBackends, &s.MaxConnections, &s.CacheHitRatio, &s.Deadlocks,
		&s.BlkReadTime, &s.BlkWriteTime, &s.TotalCheckpoints, &s.IsReplica, &s.DBSizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	if s.CacheHitRatio != 0.8 {
		t.Fatalf("collector cache ratio=%v; analyzer threshold 0.95 requires 0.8 for 800/1000 hits",
			s.CacheHitRatio)
	}
}
