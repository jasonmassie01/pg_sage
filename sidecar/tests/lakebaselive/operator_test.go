package lakebaselive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/schema"
)

func TestLakebaseOperatorActions(t *testing.T) {
	p, ctx := livePool(t)
	if err := schema.Bootstrap(ctx, p); err != nil {
		t.Fatal(err)
	}
	exec(t, p, ctx, "CREATE SCHEMA IF NOT EXISTS sage_live")
	newExecutor := func(t *testing.T) *executor.Executor {
		t.Helper()
		cfg := config.DefaultConfig()
		cfg.CloudEnvironment = "lakebase"
		cfg.Trust.Level = "advisory"
		cfg.Trust.RollbackWindowMinutes = 1
		cfg.Safety.QueryTimeoutMs = 10000
		e := executor.New(p, cfg, time.Time{}, func(level, format string, args ...any) { t.Logf("%s %s", level, fmt.Sprintf(format, args...)) })
		e.WithDatabaseName("pg_sage_test")
		e.WithEmergencyStopCheck(func(context.Context) bool { return false })
		if err := e.EnableStandingPolicy(ctx, "unattended", nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := e.Shutdown(shutdown); err != nil {
				t.Error(err)
			}
		})
		return e
	}
	fixture := func(t *testing.T, name string) string {
		t.Helper()
		table := "sage_live.operator_" + name
		exec(t, p, ctx, "DROP TABLE IF EXISTS "+table+" CASCADE")
		exec(t, p, ctx, "CREATE TABLE "+table+" (id int PRIMARY KEY,a int,b int,payload text)")
		exec(t, p, ctx, "INSERT INTO "+table+" SELECT g,g%100,g%100,repeat('x',80) FROM generate_series(1,5000)g")
		return table
	}
	run := func(t *testing.T, e *executor.Executor, table, sql, rollback string) int64 {
		t.Helper()
		finding := operatorFinding(t, p, ctx, table, sql)
		id, err := e.ExecuteManual(ctx, finding, sql, rollback, nil)
		if err != nil || id <= 0 {
			t.Fatalf("production ExecuteManual %s: id=%d error=%v", sql, id, err)
		}
		var decision int64
		var recordedSQL, outcome string
		if err := p.QueryRow(ctx, "SELECT COALESCE(decision_id,0),sql_executed,outcome FROM sage.action_log WHERE id=$1", id).Scan(&decision, &recordedSQL, &outcome); err != nil {
			t.Fatal(err)
		}
		if decision <= 0 || recordedSQL != sql || outcome == "failed" {
			t.Fatalf("invalid durable action: decision=%d sql=%q outcome=%s", decision, recordedSQL, outcome)
		}
		var verdict, reason string
		if err := p.QueryRow(ctx, "SELECT verdict,reason FROM sage.decision WHERE id=$1", decision).Scan(&verdict, &reason); err != nil {
			t.Fatal(err)
		}
		if verdict != "execute" {
			t.Fatalf("durable decision verdict=%q reason=%q", verdict, reason)
		}
		t.Logf("operator action=%d decision=%d outcome=%s SQL=%s", id, decision, outcome, sql)
		return id
	}
	t.Run("Analyze", func(t *testing.T) {
		e := newExecutor(t)
		table := fixture(t, "analyze")
		id := run(t, e, table, "ANALYZE "+table, "")
		var analyzed bool
		if err := p.QueryRow(ctx, "SELECT last_analyze IS NOT NULL FROM pg_stat_user_tables WHERE relid=$1::regclass", table).Scan(&analyzed); err != nil || !analyzed {
			t.Fatalf("analyze evidence=%v err=%v", analyzed, err)
		}
		assertVerification(t, p, ctx, id)
	})
	t.Run("VacuumNonFull", func(t *testing.T) {
		e := newExecutor(t)
		table := fixture(t, "vacuum")
		exec(t, p, ctx, "DELETE FROM "+table+" WHERE id>4000")
		run(t, e, table, "VACUUM (ANALYZE) "+table, "")
		var count int
		if err := p.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 4000 {
			t.Fatalf("vacuum rowcount=%d err=%v", count, err)
		}
	})
	t.Run("CreateIndexAndExactRollback", func(t *testing.T) {
		e := newExecutor(t)
		table := fixture(t, "index")
		index := "sage_live.operator_index_a_idx"
		id := run(t, e, table, "CREATE INDEX CONCURRENTLY operator_index_a_idx ON "+table+"(a)", "DROP INDEX CONCURRENTLY "+index)
		var valid bool
		if err := p.QueryRow(ctx, "SELECT indisvalid AND indisready FROM pg_index WHERE indexrelid=$1::regclass", index).Scan(&valid); err != nil || !valid {
			t.Fatalf("index valid=%v err=%v", valid, err)
		}
		assertNoInventedBenefit(t, p, ctx, id)
		if err := e.RollbackAction(ctx, id, "native Lakebase exact rollback test"); err != nil {
			t.Fatal(err)
		}
		assertAbsent(t, p, ctx, index)
		assertRolledBack(t, p, ctx, id)
	})
	t.Run("ReindexIndexConcurrently", func(t *testing.T) {
		e := newExecutor(t)
		table := fixture(t, "reindex")
		exec(t, p, ctx, "CREATE INDEX operator_reindex_a_idx ON "+table+"(a)")
		id := run(t, e, table, "REINDEX INDEX CONCURRENTLY sage_live.operator_reindex_a_idx", "")
		assertNoInventedBenefit(t, p, ctx, id)
	})
	t.Run("ReindexTableConcurrently", func(t *testing.T) {
		e := newExecutor(t)
		table := fixture(t, "reindex_table")
		exec(t, p, ctx, "CREATE INDEX operator_reindex_table_a_idx ON "+table+"(a)")
		run(t, e, table, "REINDEX TABLE CONCURRENTLY "+table, "")
	})
	t.Run("CreateStatisticsAnalyzeAndRollback", func(t *testing.T) {
		e := newExecutor(t)
		table := fixture(t, "statistics")
		name := "sage_live.sage_stx_operator_ab"
		id := run(t, e, table, "CREATE STATISTICS "+name+" (dependencies,ndistinct,mcv) ON a,b FROM "+table, "")
		var built bool
		if err := p.QueryRow(ctx, `SELECT d.stxdndistinct IS NOT NULL AND d.stxddependencies IS NOT NULL FROM pg_statistic_ext s JOIN pg_statistic_ext_data d ON d.stxoid=s.oid WHERE s.stxname='sage_stx_operator_ab' AND s.stxnamespace='sage_live'::regnamespace`).Scan(&built); err != nil || !built {
			t.Fatalf("statistics built=%v err=%v", built, err)
		}
		assertNoInventedBenefit(t, p, ctx, id)
		if err := e.RollbackAction(ctx, id, "native statistics rollback"); err != nil {
			t.Fatal(err)
		}
		var exists bool
		if err := p.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pg_statistic_ext WHERE stxname='sage_stx_operator_ab' AND stxnamespace='sage_live'::regnamespace)").Scan(&exists); err != nil || exists {
			t.Fatalf("stats rollback exists=%v err=%v", exists, err)
		}
		assertRolledBack(t, p, ctx, id)
	})
	t.Run("DatabaseUserSettingAndRestore", func(t *testing.T) {
		e := newExecutor(t)
		var prior *string
		if err := p.QueryRow(ctx, `SELECT (SELECT substring(setting from 10) FROM pg_db_role_setting,LATERAL unnest(setconfig) setting WHERE setdatabase=(SELECT oid FROM pg_database WHERE datname=current_database()) AND setrole=0 AND setting LIKE 'work_mem=%')`).Scan(&prior); err != nil {
			t.Fatal(err)
		}
		originalRollback := "ALTER DATABASE pg_sage_test RESET work_mem"
		if prior != nil {
			originalRollback = fmt.Sprintf("ALTER DATABASE pg_sage_test SET work_mem='%s'", strings.ReplaceAll(*prior, "'", "''"))
		}
		defer func() { exec(t, p, ctx, originalRollback); p.Reset() }()
		for _, initial := range []string{"", "12MB"} {
			t.Run("prior_"+initial, func(t *testing.T) {
				setup := "ALTER DATABASE pg_sage_test RESET work_mem"
				if initial != "" {
					setup = "ALTER DATABASE pg_sage_test SET work_mem='" + initial + "'"
				}
				exec(t, p, ctx, setup)
				id := run(t, e, "pg_sage_test", "ALTER DATABASE pg_sage_test SET work_mem='48MB'", "ALTER DATABASE pg_sage_test SET work_mem='999MB'")
				var derived, state string
				var fresh bool
				if err := p.QueryRow(ctx, `SELECT rollback_sql,after_state#>>'{config_readback,state}',(after_state#>>'{config_readback,fresh_session}')::boolean FROM sage.action_log WHERE id=$1`, id).Scan(&derived, &state, &fresh); err != nil {
					t.Fatal(err)
				}
				want := `ALTER DATABASE "pg_sage_test" RESET work_mem`
				if initial != "" {
					want = `ALTER DATABASE "pg_sage_test" SET work_mem = '` + initial + `'`
				}
				if derived != want || state != "in_effect" || !fresh {
					t.Fatalf("production capture/readback: derived=%q want=%q state=%s fresh=%v", derived, want, state, fresh)
				}
				p.Reset()
				var effective string
				if err := p.QueryRow(ctx, "SHOW work_mem").Scan(&effective); err != nil || effective != "48MB" {
					t.Fatalf("fresh session work_mem=%s err=%v", effective, err)
				}
				if err := e.RollbackAction(ctx, id, "restore production-derived original database setting"); err != nil {
					t.Fatal(err)
				}
				p.Reset()
				var restored *string
				if err := p.QueryRow(ctx, `SELECT (SELECT substring(setting from 10) FROM pg_db_role_setting,LATERAL unnest(setconfig) setting WHERE setdatabase=(SELECT oid FROM pg_database WHERE datname=current_database()) AND setrole=0 AND setting LIKE 'work_mem=%')`).Scan(&restored); err != nil {
					t.Fatal(err)
				}
				if initial == "" && restored != nil || initial != "" && (restored == nil || *restored != initial) {
					t.Fatal("production GUC inverse failed to restore exact prior")
				}
			})
		}
	})
	t.Run("UnsupportedOperationsRefused", func(t *testing.T) {
		e := newExecutor(t)
		table := fixture(t, "refusal")
		exec(t, p, ctx, "CREATE INDEX operator_refusal_a_idx ON "+table+"(a)")
		for _, sql := range []string{
			"ALTER SYSTEM SET work_mem='64MB'",
			"ALTER DATABASE pgsage_nonexistent_live_target SET work_mem='48MB'",
			"ALTER DATABASE pg_sage_test SET max_wal_size='4GB'",
			"ALTER DATABASE pg_sage_test SET shared_buffers='1GB'",
			"VACUUM FULL " + table,
			"DROP INDEX CONCURRENTLY sage_live.operator_refusal_a_idx",
			"REINDEX DATABASE CONCURRENTLY pg_sage_test",
		} {
			finding := operatorFinding(t, p, ctx, table, sql)
			id, err := e.ExecuteManual(ctx, finding, sql, "", nil)
			if err == nil || id != 0 {
				t.Errorf("unsupported SQL ran: %s id=%d error=%v", sql, id, err)
			} else {
				t.Logf("expected refusal: %s: %v", sql, err)
			}
			if strings.Contains(sql, "max_wal_size") && !errors.Is(err, executor.ErrManagedConfigUnsupported) {
				t.Errorf("database instance setting must be provider-refused: %v", err)
			}
			if strings.HasPrefix(sql, "ALTER DATABASE pgsage_nonexistent_live_target ") && (err == nil || !strings.Contains(err.Error(), "connected database")) {
				t.Errorf("cross-database target must be refused: %v", err)
			}
		}
		var exists bool
		if err := p.QueryRow(ctx, "SELECT to_regclass('sage_live.operator_refusal_a_idx') IS NOT NULL").Scan(&exists); err != nil || !exists {
			t.Fatalf("refused DROP altered index: exists=%v err=%v", exists, err)
		}
	})
}

func operatorFinding(t *testing.T, p *pgxpool.Pool, ctx context.Context, object, sql string) int {
	t.Helper()
	var id int
	if err := p.QueryRow(ctx, `INSERT INTO sage.findings(category,severity,object_type,object_identifier,title,detail,recommendation,recommended_sql) VALUES ('lakebase_live_operator','warning','table',$1,'native Lakebase operator fixture','{}','reviewed owned fixture',$2) ON CONFLICT(category,object_identifier) WHERE status='open' DO UPDATE SET recommended_sql=excluded.recommended_sql,last_seen=now() RETURNING id`, object, sql).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertAbsent(t *testing.T, p *pgxpool.Pool, ctx context.Context, name string) {
	t.Helper()
	var absent bool
	if err := p.QueryRow(ctx, "SELECT to_regclass($1) IS NULL", name).Scan(&absent); err != nil || !absent {
		t.Fatalf("rollback absent=%v err=%v", absent, err)
	}
}
func assertRolledBack(t *testing.T, p *pgxpool.Pool, ctx context.Context, id int64) {
	t.Helper()
	var outcome string
	if err := p.QueryRow(ctx, "SELECT outcome FROM sage.action_log WHERE id=$1", id).Scan(&outcome); err != nil || outcome != "rolled_back" {
		t.Fatalf("rollback outcome=%s err=%v", outcome, err)
	}
}
func assertVerification(t *testing.T, p *pgxpool.Pool, ctx context.Context, id int64) {
	t.Helper()
	var count int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM sage.verification WHERE action_log_id=$1", id).Scan(&count); err != nil || count == 0 {
		t.Fatalf("durable verification count=%d err=%v", count, err)
	}
}
func assertNoInventedBenefit(t *testing.T, p *pgxpool.Pool, ctx context.Context, id int64) {
	t.Helper()
	var improved bool
	if err := p.QueryRow(ctx, "SELECT EXISTS(SELECT FROM sage.action_outcome WHERE action_log_id=$1 AND verdict='improved')", id).Scan(&improved); err != nil || improved {
		t.Fatalf("unmeasured benefit credited=%v err=%v", improved, err)
	}
	t.Log("SQL execution established; workload benefit remains unverified without measured before/after evidence")
}
