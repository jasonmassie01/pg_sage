// Package lakebaselive exercises native Lakebase with the production OAuth pool.
// It deliberately has no legacy testdb TestMain: those fixtures assume cluster
// superuser access and sometimes change global settings or statistics.
package lakebaselive

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/schema"
)

func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	if os.Getenv("PG_SAGE_LIVE_LAKEBASE_MUTATE") != "1" {
		t.Skip("set PG_SAGE_LIVE_LAKEBASE_MUTATE=1 for isolated native Lakebase fixtures")
	}
	endpoint := os.Getenv("SAGE_LAKEBASE_ENDPOINT")
	if !strings.Contains(endpoint, "/branches/pg-sage-test/endpoints/") || os.Getenv("SAGE_LAKEBASE_ENDPOINTS") != "" {
		t.Fatal("native fixtures require the explicitly isolated pg-sage-test branch")
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("SAGE_LAKEBASE_LIVE_DATABASE_URL"))
	if err != nil || cfg == nil || cfg.ConnConfig.Database != "pg_sage_test" || cfg.ConnConfig.Password != "" {
		t.Fatal("native fixtures require a passwordless pg_sage_test DSN")
	}
	if cfg.ConnConfig.TLSConfig == nil || cfg.ConnConfig.TLSConfig.InsecureSkipVerify {
		t.Fatal("native fixtures require verified TLS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	cfg.MinConns, cfg.MaxConns = 0, 6
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "20000"
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "5000"
	cfg.ConnConfig.RuntimeParams["application_name"] = "pg_sage_lakebase_live_tests"
	if err := databricksauth.ConfigurePool(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	return pool, ctx
}

func exec(t *testing.T, p *pgxpool.Pool, ctx context.Context, sql string, args ...any) {
	t.Helper()
	if _, err := p.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestLakebaseNative(t *testing.T) {
	p, ctx := livePool(t)
	t.Run("IdentityAndPG18", func(t *testing.T) {
		var version int
		var tls, super bool
		var db, user string
		if err := p.QueryRow(ctx, `SELECT current_setting('server_version_num')::int,
			current_database(), current_user, ssl, r.rolsuper FROM pg_stat_ssl s
			JOIN pg_roles r ON r.rolname=current_user WHERE s.pid=pg_backend_pid()`).Scan(&version, &db, &user, &tls, &super); err != nil {
			t.Fatal(err)
		}
		if version < 180000 || version >= 190000 || db != "pg_sage_test" || !tls || super {
			t.Fatalf("unexpected environment: version=%d db=%s tls=%v superuser=%v", version, db, tls, super)
		}
		t.Logf("PG%d, verified TLS, native role %s, no true superuser", version, user)
	})
	t.Run("DocumentedExtensions", func(t *testing.T) {
		for _, extension := range []string{"pg_stat_statements", "pgcrypto", "pg_hint_plan", "vector", "pgstattuple"} {
			t.Run(extension, func(t *testing.T) {
				exec(t, p, ctx, "CREATE EXTENSION IF NOT EXISTS "+extension)
				var version string
				if err := p.QueryRow(ctx, "SELECT extversion FROM pg_extension WHERE extname=$1", extension).Scan(&version); err != nil {
					t.Fatal(err)
				}
				t.Logf("installed version %s", version)
			})
		}
		var absent bool
		if err := p.QueryRow(ctx, "SELECT NOT EXISTS(SELECT 1 FROM pg_available_extensions WHERE name='hypopg')").Scan(&absent); err != nil {
			t.Fatal(err)
		}
		if !absent {
			t.Fatal("HypoPG became available: update the capability assessment and test hypothetical indexes")
		}
		t.Log("HypoPG unavailable: hypothetical-index features remain unsupported")
		var advertised bool
		if err := p.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_available_extensions WHERE name='pgaudit')").Scan(&advertised); err != nil {
			t.Fatal(err)
		}
		if advertised {
			_, err := p.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pgaudit")
			t.Logf("pgaudit catalog advertisement=true; CREATE EXTENSION result=%v; server audit log delivery is not established by extension installation", err)
		} else {
			t.Log("pgaudit not advertised")
		}
	})
	t.Run("BootstrapIdempotenceAndRestartPersistence", func(t *testing.T) {
		if err := schema.Bootstrap(ctx, p); err != nil {
			t.Fatal(err)
		}
		exec(t, p, ctx, `DELETE FROM sage.config WHERE key='lakebase_live_marker'`)
		exec(t, p, ctx, `INSERT INTO sage.config(key,value,updated_by) VALUES ('lakebase_live_marker','persistent','native-live-test')`)
		if err := schema.Bootstrap(ctx, p); err != nil {
			t.Fatal(err)
		}
		p.Reset()
		if err := schema.Bootstrap(ctx, p); err != nil {
			t.Fatal(err)
		}
		var marker string
		if err := p.QueryRow(ctx, "SELECT value FROM sage.config WHERE key='lakebase_live_marker'").Scan(&marker); err != nil || marker != "persistent" {
			t.Fatalf("persistent marker=%q err=%v", marker, err)
		}
		var count int
		if err := p.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema='sage'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		t.Logf("bootstrap/migrations repeated across physical-pool restart; %d durable sage tables", count)
	})
	if t.Failed() {
		t.Fatal("native environment/bootstrap prerequisites failed")
	}
	exec(t, p, ctx, "CREATE SCHEMA IF NOT EXISTS sage_live")
	exec(t, p, ctx, "DROP TABLE IF EXISTS sage_live.workload CASCADE")
	exec(t, p, ctx, `CREATE TABLE sage_live.workload(id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, a int, b int, payload text)`)
	exec(t, p, ctx, `INSERT INTO sage_live.workload(a,b,payload) SELECT g%100,g%100,repeat('x',80) FROM generate_series(1,12000) g`)
	t.Run("OwnedSQLFamiliesAndRollback", func(t *testing.T) {
		for _, sql := range []string{
			"ANALYZE sage_live.workload",
			"VACUUM (ANALYZE) sage_live.workload",
			"CREATE INDEX CONCURRENTLY workload_a_idx ON sage_live.workload(a)",
			"REINDEX INDEX CONCURRENTLY sage_live.workload_a_idx",
			"ALTER TABLE sage_live.workload ALTER COLUMN a SET STATISTICS 250",
			"CREATE STATISTICS sage_live.workload_ab_stats (dependencies,ndistinct,mcv) ON a,b FROM sage_live.workload",
			"ANALYZE sage_live.workload",
			"ALTER TABLE sage_live.workload SET (autovacuum_analyze_scale_factor=0.05)",
			"COMMENT ON TABLE sage_live.workload IS 'owned Lakebase live fixture'",
		} {
			exec(t, p, ctx, sql)
			t.Log(sql)
		}
		var statsTarget int
		if err := p.QueryRow(ctx, `SELECT attstattarget FROM pg_attribute WHERE attrelid='sage_live.workload'::regclass AND attname='a'`).Scan(&statsTarget); err != nil || statsTarget != 250 {
			t.Fatalf("stats target=%d err=%v", statsTarget, err)
		}
		for _, sql := range []string{
			"DROP STATISTICS sage_live.workload_ab_stats",
			"ALTER TABLE sage_live.workload ALTER COLUMN a SET STATISTICS -1",
			"ALTER TABLE sage_live.workload RESET (autovacuum_analyze_scale_factor)",
			"DROP INDEX CONCURRENTLY sage_live.workload_a_idx",
		} {
			exec(t, p, ctx, sql)
		}
		var restored bool
		if err := p.QueryRow(ctx, `SELECT to_regclass('sage_live.workload_a_idx') IS NULL AND NOT EXISTS(SELECT FROM pg_statistic_ext WHERE stxname='workload_ab_stats') AND (SELECT COALESCE(attstattarget,-1)=-1 FROM pg_attribute WHERE attrelid='sage_live.workload'::regclass AND attname='a') AND (SELECT reloptions IS NULL FROM pg_class WHERE oid='sage_live.workload'::regclass)`).Scan(&restored); err != nil || !restored {
			t.Fatalf("rollback restored=%v err=%v", restored, err)
		}
	})
	t.Run("ExtensionFunctions", func(t *testing.T) {
		for _, sql := range []string{
			"SELECT digest('pg_sage','sha256')",
			"SELECT '[1,2,3]'::vector <-> '[1,2,4]'::vector",
			"SELECT * FROM pgstattuple('sage_live.workload')",
			"EXPLAIN /*+ SeqScan(workload) */ SELECT * FROM sage_live.workload WHERE a=1",
			"SELECT count(*) FROM pg_stat_statements WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())",
		} {
			exec(t, p, ctx, sql)
			t.Log(sql)
		}
	})
	t.Run("DatabaseContextSettingAndRestore", func(t *testing.T) {
		var prior *string
		if err := p.QueryRow(ctx, `SELECT (SELECT substring(setting from 10) FROM pg_db_role_setting,
			LATERAL unnest(setconfig) setting WHERE setdatabase=(SELECT oid FROM pg_database WHERE datname=current_database()) AND setrole=0 AND setting LIKE 'work_mem=%')`).Scan(&prior); err != nil {
			t.Fatal(err)
		}
		restore := func() {
			if prior == nil {
				exec(t, p, ctx, "ALTER DATABASE pg_sage_test RESET work_mem")
			} else {
				exec(t, p, ctx, fmt.Sprintf("ALTER DATABASE pg_sage_test SET work_mem = '%s'", strings.ReplaceAll(*prior, "'", "''")))
			}
			p.Reset()
		}
		defer restore()
		exec(t, p, ctx, "ALTER DATABASE pg_sage_test SET work_mem='64MB'")
		p.Reset()
		var effective string
		if err := p.QueryRow(ctx, "SHOW work_mem").Scan(&effective); err != nil || effective != "64MB" {
			t.Fatalf("fresh-session work_mem=%q err=%v", effective, err)
		}
		restore()
		var restored *string
		if err := p.QueryRow(ctx, `SELECT (SELECT substring(setting from 10) FROM pg_db_role_setting,LATERAL unnest(setconfig) setting WHERE setdatabase=(SELECT oid FROM pg_database WHERE datname=current_database()) AND setrole=0 AND setting LIKE 'work_mem=%')`).Scan(&restored); err != nil {
			t.Fatal(err)
		}
		if (prior == nil) != (restored == nil) || prior != nil && *prior != *restored {
			t.Fatal("database setting was not restored exactly")
		}
		t.Log("database work_mem changed, observed on a fresh physical session, and restored exactly")
	})
	t.Run("CollectorAnalyzerAndDurableOutput", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.Collector.IntervalSeconds = 2
		cfg.Analyzer.IntervalSeconds = 2
		cfg.Analyzer.SlowQueryThresholdMs = 1
		cfg.Advisor.Enabled = false
		cfg.Safety.QueryTimeoutMs = 10000
		var mu sync.Mutex
		var errors []string
		runCtx, cancel := context.WithCancel(ctx)
		cycles := make(chan struct{}, 1)
		var baseline int
		var started time.Time
		if err := p.QueryRow(ctx, "SELECT (SELECT count(*) FROM sage.snapshots), now()").Scan(&baseline, &started); err != nil {
			t.Fatal(err)
		}
		logFn := func(level, format string, args ...any) {
			line := fmt.Sprintf(format, args...)
			if strings.HasPrefix(line, "analyzer cycle:") {
				select {
				case cycles <- struct{}{}:
				default:
				}
			}
			if level == "ERROR" && runCtx.Err() == nil {
				mu.Lock()
				errors = append(errors, line)
				mu.Unlock()
			}
			t.Logf("%s %s", level, line)
		}
		c := collector.New(p, cfg, 180000, logFn)
		a := analyzer.New(p, cfg, c, nil, nil, nil, nil, logFn)
		a.WithDatabaseName("pg_sage_test")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); c.Run(runCtx) }()
		go func() { defer wg.Done(); a.Run(runCtx) }()
		defer func() { cancel(); wg.Wait() }()
		deadline := time.Now().Add(70 * time.Second)
		var snapshots, findings int
		completed := false
		for time.Now().Before(deadline) {
			exec(t, p, ctx, "SELECT pg_sleep(0.025) FROM sage_live.workload LIMIT 1")
			if err := p.QueryRow(ctx, "SELECT count(*) FROM sage.snapshots").Scan(&snapshots); err != nil {
				t.Fatal(err)
			}
			if err := p.QueryRow(ctx, "SELECT count(*) FROM sage.findings WHERE category='slow_query' AND last_seen >= $1", started).Scan(&findings); err != nil {
				t.Fatal(err)
			}
			select {
			case <-cycles:
				completed = true
			default:
			}
			if snapshots > baseline && findings > 0 && completed {
				break
			}
			time.Sleep(time.Second)
		}
		observedSnapshot := c.LatestSnapshot()
		cancel()
		wg.Wait()
		if snapshots <= baseline || findings == 0 || !completed || observedSnapshot == nil {
			t.Fatalf("durable output snapshots=%d slow_findings=%d", snapshots, findings)
		}
		for _, category := range []string{"queries", "tables", "indexes", "sequences"} {
			if !observedSnapshot.Available(category) {
				t.Errorf("collector category %s unavailable: %s", category, observedSnapshot.Unavailable[category])
			}
		}
		mu.Lock()
		captured := append([]string(nil), errors...)
		mu.Unlock()
		if len(captured) > 0 {
			t.Errorf("collector/analyzer error logs: %v", captured)
		}
		p.Reset()
		var durable int
		if err := p.QueryRow(ctx, "SELECT count(*) FROM sage.findings WHERE category='slow_query'").Scan(&durable); err != nil || durable < findings {
			t.Fatalf("finding persistence after pool restart: %d err=%v", durable, err)
		}
		t.Logf("durable snapshots=%d, slow-query findings=%d, unavailable before shutdown=%v", snapshots, findings, observedSnapshot.Unavailable)
	})
}
