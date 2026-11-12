package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// market-12 on real PostgreSQL: on a CloudNativePG Cluster the operator
// owns postgresql.conf and disables ALTER SYSTEM, so the executor never
// issues one. Every path that could is driven against the live server and
// the server's own record of ALTER SYSTEM (postgresql.auto.conf, read
// through pg_file_settings) must stay untouched.

// autoConfEntries takes the test's pool: requireDB holds a session lock,
// so a second requireDB in the same test would wait on it forever.
func autoConfEntries(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	name string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_file_settings
		WHERE name = $1 AND sourcefile LIKE '%postgresql.auto.conf'`, name).Scan(&n); err != nil {
		t.Fatalf("read pg_file_settings: %v", err)
	}
	return n
}

func TestCNPGNeverIssuesAlterSystem(t *testing.T) {
	ctx := context.Background()
	pool := lockAlterSystem(t, ctx, "work_mem", "max_slot_wal_keep_size")
	before := mustSetting(t, ctx, pool, "work_mem")
	if autoConfEntries(t, ctx, pool, "work_mem") != 0 {
		t.Fatal("fixture: postgresql.auto.conf already sets work_mem")
	}
	cfg := config.DefaultConfig()
	cfg.CloudEnvironment = "cnpg"
	e := New(pool, cfg, time.Time{}, nopLog)
	sql := "ALTER SYSTEM SET work_mem = '77MB'"
	refusal := e.findingRefusal(analyzer.Finding{Title: "raise work_mem",
		Category: "memory_tuning", RecommendedSQL: sql})
	if refusal == nil || !strings.Contains(refusal.Error(), "kubectl patch cluster") {
		t.Fatalf("finding refusal = %v, want the CNPG Cluster guidance", refusal)
	}
	out := applyConfigChangeWithin(ctx, pool, sql, "cnpg", nopLog, time.Second)
	if out.InEffect || out.State != readbackUnconfirmed ||
		!strings.Contains(out.Note, "CloudNativePG") {
		t.Fatalf("config apply outcome = %+v", out)
	}
	_, handled, err := e.applyManagedCustodianConfig(ctx, CustodianProposal{
		SQL: "ALTER SYSTEM SET max_slot_wal_keep_size = '10GB'"})
	if !handled || !errors.Is(err, ErrManagedConfigUnsupported) {
		t.Fatalf("custodian config: handled %v err %v, want unsupported", handled, err)
	}
	for _, name := range []string{"work_mem", "max_slot_wal_keep_size"} {
		if n := autoConfEntries(t, ctx, pool, name); n != 0 {
			t.Fatalf("%s: postgresql.auto.conf gained %d entries", name, n)
		}
	}
	if after := mustSetting(t, ctx, pool, "work_mem"); after.Setting != before.Setting {
		t.Fatalf("work_mem changed from %s to %s", before.Setting, after.Setting)
	}
}
