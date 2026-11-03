package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

func TestDatabaseSettingCapturesExactPriorAndFreshSessionRoundtrip(t *testing.T) {
	p, ctx := requireDB(t)
	var database string
	if err := p.QueryRow(ctx, "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	quoted := pgx.Identifier{database}.Sanitize()
	base := pgconf.DatabaseStmt{Database: quoted, SystemStmt: pgconf.SystemStmt{Name: "work_mem"}}
	original, err := captureDatabaseSetting(ctx, p, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), databaseRollbackSQL(base, original)) })
	cfg := config.DefaultConfig()
	cfg.CloudEnvironment = "lakebase"
	e := New(p, cfg, time.Time{}, nopLog)
	for _, prior := range []string{"", "12MB"} {
		t.Run("prior_"+prior, func(t *testing.T) {
			setup := "ALTER DATABASE " + quoted + " RESET work_mem"
			if prior != "" {
				setup = "ALTER DATABASE " + quoted + " SET work_mem='" + prior + "'"
			}
			if _, err := p.Exec(ctx, setup); err != nil {
				t.Fatal(err)
			}
			sql := "ALTER DATABASE " + quoted + " SET work_mem='48MB'"
			change, err := e.prepareConfigChange(ctx, sql)
			if err != nil {
				t.Fatal(err)
			}
			if change == nil || change.kind != configKindDatabaseGUC {
				t.Fatal("missing production database config capture")
			}
			want := "ALTER DATABASE " + quoted + " RESET work_mem"
			if prior != "" {
				want = "ALTER DATABASE " + quoted + " SET work_mem = '" + prior + "'"
			}
			if change.rollbackSQL != want {
				t.Fatalf("derived rollback=%q want=%q", change.rollbackSQL, want)
			}
			if _, err := p.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
			state, note, detail := e.readBackConfig(ctx, change)
			if state != readbackInEffect || detail["fresh_session"] != true {
				t.Fatalf("forward state=%s note=%s detail=%v", state, note, detail)
			}
			if _, err := executeRollbackSQL(ctx, p, change.rollbackSQL, RollbackMonitorConfig{}); err != nil {
				t.Fatal(err)
			}
			after, err := captureDatabaseSetting(ctx, p, base)
			if err != nil {
				t.Fatal(err)
			}
			if prior == "" && after.Value != nil || prior != "" && (after.Value == nil || *after.Value != prior) {
				t.Fatalf("catalog prior was not restored: %+v", after)
			}
		})
	}
	if _, err := e.prepareConfigChange(ctx, "ALTER DATABASE "+quoted+" SET max_wal_size='4GB'"); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("instance setting must be refused: %v", err)
	}
}

func TestDatabaseSettingDoesNotWidenRoleOverride(t *testing.T) {
	p, ctx := requireDB(t)
	var database, user string
	if err := p.QueryRow(ctx, "SELECT current_database(),current_user").Scan(&database, &user); err != nil {
		t.Fatal(err)
	}
	quoted := pgx.Identifier{database}.Sanitize()
	stmt := pgconf.DatabaseStmt{Database: quoted, SystemStmt: pgconf.SystemStmt{Name: "work_mem"}}
	original, err := captureDatabaseSetting(ctx, p, stmt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), databaseRollbackSQL(stmt, original))
		_, _ = p.Exec(context.Background(), "ALTER ROLE "+pgx.Identifier{user}.Sanitize()+" IN DATABASE "+quoted+" RESET work_mem")
	})
	if _, err := p.Exec(ctx, "ALTER DATABASE "+quoted+" RESET work_mem"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "ALTER ROLE "+pgx.Identifier{user}.Sanitize()+" IN DATABASE "+quoted+" SET work_mem='20MB'"); err != nil {
		t.Fatal(err)
	}
	prior, err := captureDatabaseSetting(ctx, p, stmt)
	if err != nil || prior.Value != nil {
		t.Fatalf("role override leaked into database-wide prior: %+v err=%v", prior, err)
	}
	e := New(p, config.DefaultConfig(), time.Time{}, nopLog)
	if _, err := e.prepareConfigChange(ctx, "ALTER DATABASE "+quoted+" SET work_mem='48MB'"); err == nil || !strings.Contains(err.Error(), "masked") {
		t.Fatalf("masked database setting must be refused before SQL: %v", err)
	}
}

func TestDatabaseSettingRefusesAnotherDatabaseWithoutMutation(t *testing.T) {
	p, ctx := requireDB(t)
	// The testdb admin database exists separately from this package fixture.
	stmt := pgconf.DatabaseStmt{Database: "postgres", SystemStmt: pgconf.SystemStmt{Name: "work_mem"}}
	before, err := captureDatabaseSetting(ctx, p, stmt)
	if err != nil {
		t.Fatal(err)
	}
	e := New(p, config.DefaultConfig(), time.Time{}, nopLog)
	if _, err := e.prepareConfigChange(ctx, "ALTER DATABASE postgres SET work_mem='48MB'"); err == nil || !strings.Contains(err.Error(), "connected database") {
		t.Fatalf("cross-database change must be refused: %v", err)
	}
	after, err := captureDatabaseSetting(ctx, p, stmt)
	if err != nil {
		t.Fatal(err)
	}
	if (before.Value == nil) != (after.Value == nil) || before.Value != nil && *before.Value != *after.Value {
		t.Fatal("cross-database setting changed despite refusal")
	}
}
