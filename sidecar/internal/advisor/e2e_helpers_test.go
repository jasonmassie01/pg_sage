//go:build e2e

package advisor

import (
	"context"
	"fmt"
	"os"
	"strings"
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
	seedAuditWorkload(t, pool)
	// Exceed the bloat advisor's 1,000-tuple eligibility threshold using only
	// this disposable synthetic fixture. The shared integration fixture is
	// deliberately smaller and otherwise lets this E2E skip the model call.
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.advisor_fixture
		SELECT i, repeat('fixture',30) FROM generate_series(501,3000) i;
		DELETE FROM public.advisor_fixture WHERE id>2000;
		ANALYZE public.advisor_fixture;
		DO $$ BEGIN
		 IF to_regproc('pg_catalog.pg_stat_force_next_flush') IS NOT NULL THEN
		  PERFORM pg_catalog.pg_stat_force_next_flush();
		 END IF;
		END $$`); err != nil {
		t.Fatalf("expand synthetic advisor fixture: %v", err)
	}
	return pool
}

func e2eLLMConfig(getenv func(string) string) (*config.LLMConfig, error) {
	cfg := &config.LLMConfig{
		Enabled:          true,
		TimeoutSeconds:   60,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	if getenv("PG_SAGE_LIVE_LLM") == "1" && getenv("SAGE_LLM_AUTH") == "databricks" {
		cfg.Auth = "databricks"
		cfg.Endpoint, cfg.Model = getenv("SAGE_LLM_ENDPOINT"), getenv("SAGE_LLM_MODEL")
		if strings.TrimSpace(cfg.Endpoint) == "" || strings.TrimSpace(cfg.Model) == "" {
			return nil, fmt.Errorf("native advisor E2E requires explicit SAGE_LLM_ENDPOINT and SAGE_LLM_MODEL")
		}
		return cfg, nil
	}
	if cfg.APIKey = getenv("SAGE_GEMINI_API_KEY"); cfg.APIKey == "" {
		return nil, nil
	}
	cfg.Model = "gemini-2.5-flash"
	cfg.Endpoint = "https://generativelanguage.googleapis.com/v1beta/openai"
	return cfg, nil
}

func e2eLLMManager(t *testing.T) *llm.Manager {
	t.Helper()
	llmCfg, err := e2eLLMConfig(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if llmCfg == nil {
		t.Skip("set SAGE_GEMINI_API_KEY, or PG_SAGE_LIVE_LLM=1 with explicit native Databricks auth/endpoint/model")
	}
	client := llm.New(llmCfg, func(string, string, ...any) {})
	t.Cleanup(func() {
		if client.TokensUsedToday() == 0 {
			t.Error("advisor E2E completed without a successful billed model call")
		}
	})
	return llm.NewManager(client, nil, false)
}

func requireAdvisorModelCall(t *testing.T, mgr *llm.Manager, before int64) {
	t.Helper()
	after := mgr.General.TokensUsedToday()
	if after <= before {
		t.Error("advisor did not complete a successful billed model call")
	}
	t.Logf("advisor model usage: %d tokens", after-before)
}

func e2eAdvisorLog(t *testing.T) func(string, string, ...any) {
	t.Helper()
	return func(level, message string, args ...any) {
		if strings.Contains(message, "parse error:") {
			t.Errorf("[%s] %s", level, fmt.Sprintf(message, args...))
		} else {
			t.Logf("[%s] %s", level, fmt.Sprintf(message, args...))
		}
	}
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
		WHERE schemaname='public' AND relname='advisor_fixture'`)
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
		WHERE calls > 0 AND queryid != 0 AND query LIKE '%advisor_fixture%'
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
	// Controlled synthetic workload measurements exercise rewrite selection
	// without running intentionally slow queries or sampling user statements.
	// These are scenario inputs, not claimed measurements of this database.
	snap.Queries = []collector.QueryStats{{QueryID: 760001,
		Query: "SELECT * FROM public.advisor_fixture WHERE id IN (SELECT id FROM public.advisor_fixture WHERE payload LIKE '%fixture%') ORDER BY id OFFSET 1000 LIMIT 20",
		Calls: 120, TotalExecTime: 9000, MeanExecTime: 75, Rows: 2400, TempBlksWritten: 128}}
	var live, dead int64
	if err := pool.QueryRow(t.Context(), `SELECT n_live_tup,n_dead_tup FROM pg_stat_user_tables
		WHERE relid='public.advisor_fixture'::regclass`).Scan(&live, &dead); err != nil || live+dead < 1000 || dead*10 < live+dead {
		t.Fatalf("synthetic fixture does not exercise bloat advisor: live=%d dead=%d error=%v", live, dead, err)
	}

	return snap
}
