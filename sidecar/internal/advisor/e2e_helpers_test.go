//go:build e2e

package advisor

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

func e2ePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAGE_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAGE_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func e2eLLMManager(t *testing.T) *llm.Manager {
	t.Helper()
	apiKey := os.Getenv("SAGE_GEMINI_API_KEY")
	if apiKey == "" {
		t.Skip("SAGE_GEMINI_API_KEY not set")
	}
	llmCfg := &config.LLMConfig{
		Enabled:          true,
		Model:            "gemini-2.5-flash",
		Endpoint:         "https://generativelanguage.googleapis.com/v1beta/openai",
		APIKey:           apiKey,
		TimeoutSeconds:   60,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	client := llm.New(llmCfg, func(string, string, ...any) {})
	return llm.NewManager(client, nil, false)
}

func e2eSnapshot(t *testing.T, pool *pgxpool.Pool) *collector.Snapshot {
	t.Helper()
	snap := &collector.Snapshot{
		CollectedAt: time.Now(),
		ConfigData:  &collector.ConfigSnapshot{},
	}

	// Collect pg_settings
	rows, err := pool.Query(context.Background(), `
		SELECT name, setting, COALESCE(unit,''), source,
		       COALESCE(pending_restart, false)
		FROM pg_settings
		WHERE name LIKE 'autovacuum%'
		   OR name IN (
		      'max_wal_size','min_wal_size','checkpoint_completion_target',
		      'wal_compression','wal_level','wal_buffers','checkpoint_timeout',
		      'full_page_writes','shared_buffers','work_mem',
		      'maintenance_work_mem','effective_cache_size','huge_pages',
		      'temp_buffers','max_connections',
		      'superuser_reserved_connections',
		      'idle_in_transaction_session_timeout',
		      'statement_timeout','tcp_keepalives_idle',
		      'tcp_keepalives_interval','hash_mem_multiplier')`)
	if err != nil {
		t.Fatalf("pg_settings: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s collector.PGSetting
		if err := rows.Scan(
			&s.Name, &s.Setting, &s.Unit, &s.Source, &s.PendingRestart,
		); err != nil {
			continue
		}
		snap.ConfigData.PGSettings = append(snap.ConfigData.PGSettings, s)
	}

	// Collect connection states
	rows2, err := pool.Query(context.Background(), `
		SELECT COALESCE(state,'unknown'), count(*)::int,
		       COALESCE(
		         avg(EXTRACT(EPOCH FROM (now() - state_change))), 0
		       )
		FROM pg_stat_activity
		WHERE backend_type = 'client backend'
		GROUP BY state`)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var c collector.ConnectionState
			if err := rows2.Scan(
				&c.State, &c.Count, &c.AvgDurationSeconds,
			); err != nil {
				continue
			}
			snap.ConfigData.ConnectionStates = append(
				snap.ConfigData.ConnectionStates, c,
			)
		}
	}

	// WAL position
	var walPos string
	err = pool.QueryRow(
		context.Background(), "SELECT pg_current_wal_lsn()::text",
	).Scan(&walPos)
	if err == nil {
		snap.ConfigData.WALPosition = walPos
	}

	// Extensions
	rows3, err := pool.Query(context.Background(),
		`SELECT name FROM pg_available_extensions
		 WHERE name IN ('pg_repack','pgstattuple','pg_buffercache','hypopg')`)
	if err == nil {
		defer rows3.Close()
		for rows3.Next() {
			var name string
			if err := rows3.Scan(&name); err == nil {
				snap.ConfigData.ExtensionsAvailable = append(
					snap.ConfigData.ExtensionsAvailable, name,
				)
			}
		}
	}

	// Table stats
	rows4, err := pool.Query(context.Background(), `
		SELECT schemaname, relname, seq_scan, idx_scan,
		       n_tup_ins, n_tup_upd, n_tup_del, n_live_tup, n_dead_tup,
		       last_autovacuum, autovacuum_count,
		       pg_table_size(schemaname || '.' || relname),
		       pg_indexes_size(schemaname || '.' || relname)
		FROM pg_stat_user_tables
		WHERE schemaname NOT IN ('sage','pg_catalog','information_schema')`)
	if err == nil {
		defer rows4.Close()
		for rows4.Next() {
			var ts collector.TableStats
			if err := rows4.Scan(
				&ts.SchemaName, &ts.RelName,
				&ts.SeqScan, &ts.IdxScan,
				&ts.NTupIns, &ts.NTupUpd, &ts.NTupDel,
				&ts.NLiveTup, &ts.NDeadTup,
				&ts.LastAutovacuum, &ts.AutovacuumCount,
				&ts.TableBytes, &ts.IndexBytes,
			); err != nil {
				continue
			}
			snap.Tables = append(snap.Tables, ts)
		}
	}

	// Query stats from pg_stat_statements
	rows5, err := pool.Query(context.Background(), `
		SELECT queryid, left(query, 500), calls, total_exec_time,
		       mean_exec_time, rows, shared_blks_hit, shared_blks_read,
		       temp_blks_written
		FROM pg_stat_statements
		WHERE calls > 0 AND queryid != 0
		ORDER BY total_exec_time DESC
		LIMIT 50`)
	if err == nil {
		defer rows5.Close()
		for rows5.Next() {
			var q collector.QueryStats
			if err := rows5.Scan(
				&q.QueryID, &q.Query, &q.Calls,
				&q.TotalExecTime, &q.MeanExecTime,
				&q.Rows, &q.SharedBlksHit, &q.SharedBlksRead,
				&q.TempBlksWritten,
			); err != nil {
				continue
			}
			snap.Queries = append(snap.Queries, q)
		}
	}

	// System stats
	err = pool.QueryRow(context.Background(), `
		SELECT
		  (SELECT count(*)
		   FROM pg_stat_activity WHERE state = 'active')::int,
		  (SELECT count(*)
		   FROM pg_stat_activity
		   WHERE backend_type = 'client backend')::int,
		  (SELECT setting::int
		   FROM pg_settings WHERE name = 'max_connections'),
		  pg_database_size(current_database())`).Scan(
		&snap.System.ActiveBackends, &snap.System.TotalBackends,
		&snap.System.MaxConnections, &snap.System.DBSizeBytes)
	if err != nil {
		t.Logf("system stats partial: %v", err)
	}

	return snap
}
