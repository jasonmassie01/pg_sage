//go:build integration

package advisor

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

func TestTenantRollbackRestoresDatabaseOverride(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	database := conn.Config().Database
	query := "ALTER DATABASE " + sanitize.QuoteIdentifier(database) + " SET work_mem = '7MB'"
	if _, err := conn.Exec(ctx, query); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := conn.Exec(ctx, "ALTER DATABASE "+sanitize.QuoteIdentifier(database)+" RESET work_mem")
		if err != nil {
			t.Error(err)
		}
	})
	got := TransformForCloud([]analyzer.Finding{{RecommendedSQL: "ALTER SYSTEM SET work_mem = '16MB'",
		RollbackSQL: "ALTER SYSTEM SET work_mem = '7MB'"}}, "neon", database,
		[]collector.PGSetting{{Name: "work_mem", Context: "user"}})[0]
	for _, sql := range []string{got.RecommendedSQL, got.RollbackSQL} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	var restored string
	if err := conn.QueryRow(ctx, `SELECT unnest(setconfig) FROM pg_db_role_setting
		WHERE setdatabase = (SELECT oid FROM pg_database WHERE datname=$1)
		AND setrole=0`, database).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if restored != "work_mem=7MB" {
		t.Fatalf("previous database override lost: %s", restored)
	}
}
