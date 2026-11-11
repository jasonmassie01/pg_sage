package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// advisors-03: sage.managed_change_proposals accepts AlloyDB and Azure
// proposals; any other provider is still refused.
//
// No concurrent access tests: Bootstrap serializes migrations under its
// advisory lock (covered by the bootstrap lock tests).

func managedChangeProviderCheck(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool) (string, bool) {
	t.Helper()
	var def string
	var validated bool
	err := pool.QueryRow(ctx, `SELECT COALESCE(max(pg_get_constraintdef(oid)), ''),
       COALESCE(bool_and(convalidated), false)
FROM pg_constraint
WHERE conrelid = 'sage.managed_change_proposals'::regclass
  AND conname = 'managed_change_proposals_provider_check'`).Scan(&def, &validated)
	if err != nil {
		t.Fatalf("read the managed change provider check: %v", err)
	}
	return def, validated
}

func TestManagedChangeProviders_BootstrapTwice(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	def, validated := managedChangeProviderCheck(t, ctx, pool)
	if !validated {
		t.Fatalf("provider check %q is not validated", def)
	}
	for _, p := range []string{"rds", "aurora", "cloud-sql", "alloydb", "azure"} {
		if !strings.Contains(def, "'"+p+"'") {
			t.Fatalf("provider check %q does not allow %q", def, p)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'sage.managed_change_proposals'::regclass AND contype = 'c'
		  AND pg_get_constraintdef(oid) LIKE '%provider%'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("provider checks = %d (%v), want exactly one", n, err)
	}
}

func insertManagedChange(ctx context.Context, pool *pgxpool.Pool, provider string) error {
	_, err := pool.Exec(ctx, `INSERT INTO sage.managed_change_proposals
		(fingerprint, provider, mechanism, target, parameter, value, reboot_required,
		 proposal, status)
		VALUES ('adv03-' || $1, $1, 'server_parameter', 'rg/srv', 'work_mem', '65536',
		        false, '{}'::jsonb, 'superseded')`, provider)
	return err
}

func TestManagedChangeProviders_AcceptsNewRefusesOthers(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	clean := func() {
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.managed_change_proposals WHERE fingerprint LIKE 'adv03-%'")
	}
	clean()
	t.Cleanup(clean)
	for _, p := range []string{"alloydb", "azure", "rds"} {
		if err := insertManagedChange(ctx, pool, p); err != nil {
			t.Fatalf("provider %s refused: %v", p, err)
		}
	}
	for _, p := range []string{"azure-cosmos", "neon", "", "AZURE"} {
		err := insertManagedChange(ctx, pool, p)
		if err == nil || !strings.Contains(err.Error(), "managed_change_proposals_provider_check") {
			t.Fatalf("provider %q accepted (err %v)", p, err)
		}
	}
}

// The step re-runs cleanly on a migrated schema (rows present) and bounds
// its lock wait.
func TestManagedChangeProviders_RerunIsCleanAndBounded(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for run := 0; run < 2; run++ {
		if _, err := pool.Exec(ctx, ddlManagedChangeProviders); err != nil {
			t.Fatalf("re-run %d: %v", run, err)
		}
	}
	if def, _ := managedChangeProviderCheck(t, ctx, pool); !strings.Contains(def, "'azure'") {
		t.Fatalf("re-run lost the check: %q", def)
	}
	if !strings.Contains(ddlManagedChangeProviders, "lock_timeout") {
		t.Fatal("ddlManagedChangeProviders must set a lock_timeout before its ALTER")
	}
}
