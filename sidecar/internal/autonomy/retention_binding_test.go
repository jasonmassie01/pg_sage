package autonomy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// D5 (memo scenarios 3, 4, 6 and candidate drift): an aged dry run must not
// authorize deletion once the relation, the declared column's identity or
// the contract version it was recorded against has changed.

const retentionTestWindow = 30 * 24 * time.Hour

func requirePendingDryRun(t *testing.T, pool *pgxpool.Pool, err error, table string,
	rows int64,
) {
	t.Helper()
	if !errors.Is(err, ErrRetentionDryRunPending) {
		t.Fatalf("apply = %v, want ErrRetentionDryRunPending", err)
	}
	requireNothingDeleted(t, pool, table, rows)
}

func applyAfterAgedDryRun(t *testing.T, pool *pgxpool.Pool, table string,
	change func(), pipeline RetentionPipeline,
) error {
	t.Helper()
	enforcer := &postgresRetentionEnforcer{pool: pool, batchLimit: 10, pipeline: pipeline}
	agedDryRun(t, pool, enforcer, retentionItem(table, retentionTestWindow,
		schemaguard.DispositionDryRun))
	change()
	return enforcer.Apply(context.Background(), retentionItem(table, retentionTestWindow,
		schemaguard.DispositionApply))
}

func execAll(t *testing.T, pool *pgxpool.Pool, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
}

func renameSwapStatements(table string) []string {
	return []string{
		"ALTER TABLE " + table + " RENAME COLUMN created_at TO ingested_at",
		"ALTER TABLE " + table + " RENAME COLUMN source_ts TO created_at",
	}
}

func TestRetentionDryRunInvalidatedByColumnRenameSwap(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)

	err := applyAfterAgedDryRun(t, pool, table, func() {
		execAll(t, pool, renameSwapStatements(table)...)
	}, allowRetention)

	requirePendingDryRun(t, pool, err, table, 3)
}

func TestRetentionDryRunInvalidatedByTableRecreate(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)

	err := applyAfterAgedDryRun(t, pool, table, func() {
		execAll(t, pool,
			"CREATE TABLE "+table+"_copy AS TABLE "+table,
			"DROP TABLE "+table,
			"ALTER TABLE "+table+"_copy RENAME TO "+table)
	}, allowRetention)

	requirePendingDryRun(t, pool, err, table, 3)
}

func TestRetentionDryRunInvalidatedByContractRedeclare(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)

	err := applyAfterAgedDryRun(t, pool, table, func() {
		execAll(t, pool, fmt.Sprintf(`UPDATE sage.table_contract
			SET expected_pk='id', updated_at=now() + interval '1 second'
			WHERE table_name='%s'`, table))
	}, allowRetention)

	requirePendingDryRun(t, pool, err, table, 3)
}

func TestRetentionCandidateDriftRequiresNewDryRun(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)

	err := applyAfterAgedDryRun(t, pool, table, func() {
		execAll(t, pool, "INSERT INTO "+table+" SELECT g, now()-interval '50 days', "+
			"now() FROM generate_series(100, 599) g")
	}, allowRetention)

	requirePendingDryRun(t, pool, err, table, 503)
	fresh := countRows(t, pool, fmt.Sprintf(`SELECT count(*) FROM sage.retention_run
		WHERE table_name='%s' AND disposition='dry_run' AND candidate_rows=502
		AND created_at > now() - interval '1 hour'`, table))
	if fresh != 1 {
		t.Fatalf("fresh dry runs describing the drifted population = %d, want 1", fresh)
	}
}

// Candidates within the drift bound still delete: the guard must not block
// ordinary growth between the dry run and the apply.
func TestRetentionSmallCandidateGrowthStillApplies(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)

	err := applyAfterAgedDryRun(t, pool, table, func() {
		execAll(t, pool, "INSERT INTO "+table+" VALUES (4, now()-interval '50 days', now())")
	}, allowRetention)

	if err != nil {
		t.Fatalf("apply within drift bound: %v", err)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM "+table); n != 1 {
		t.Fatalf("rows = %d, want only the fresh row", n)
	}
}

// The pipeline authorizes after the dry-run check and before the delete
// transaction; a rename swap there must be caught inside the transaction.
func TestRetentionDeleteRechecksColumnIdentityInTx(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)
	swapAtAuthorization := func(ctx context.Context, _ RetentionIntent,
		batch RetentionBatch,
	) error {
		execAll(t, pool, renameSwapStatements(table)...)
		_, err := batch(ctx, RetentionRun{})
		return err
	}

	err := applyAfterAgedDryRun(t, pool, table, func() {}, swapAtAuthorization)

	if err == nil || !strings.Contains(err.Error(), "changed identity") {
		t.Fatalf("apply = %v, want the in-transaction identity recheck to refuse", err)
	}
	requireNothingDeleted(t, pool, table, 3)
}
