package autonomy

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

func preflightEnforcer(t *testing.T, partitioned bool) (
	*pgxpool.Pool, *postgresRetentionEnforcer, schemaguard.Remediation,
) {
	t.Helper()
	p := requireAutonomyDB(t)
	ctx := context.Background()
	table := fmt.Sprintf("preflight_retention_%d", time.Now().UnixNano())
	ddl := "CREATE TABLE " + table + " (id int, created_at timestamptz)"
	if partitioned {
		ddl += " PARTITION BY RANGE(created_at); CREATE TABLE " + table + "_old PARTITION OF " +
			table + " FOR VALUES FROM ('2000-01-01') TO ('2030-01-01'); CREATE TABLE " + table +
			"_new PARTITION OF " + table + " FOR VALUES FROM ('2030-01-01') TO ('2100-01-01')"
	}
	if _, err := p.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "INSERT INTO "+table+" VALUES (1,'2020-01-01'),(2,'2099-01-01')"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, "DELETE FROM sage.retention_run WHERE table_name=$1", table)
		_, _ = p.Exec(ctx, "DROP TABLE IF EXISTS "+table+" CASCADE")
	})
	if _, err := policy.NewStore(p).Bootstrap(ctx, policy.Scope{}, "unattended", "preflight"); err != nil {
		t.Fatal(err)
	}
	item := schemaguard.Remediation{
		Invariant: schemaguard.Invariant{Schema: "public", Table: table, RetentionColumn: "created_at"},
		Contract:  schemaguard.TableContract{RetentionWindow: 30 * 24 * time.Hour},
		Decision:  schemaguard.Decision{Disposition: schemaguard.DispositionDryRun},
	}
	e := &postgresRetentionEnforcer{pool: p, batchLimit: 1}
	if err := e.Apply(ctx, item); err != nil {
		t.Fatal(err)
	}
	item.Decision.Disposition = schemaguard.DispositionApply
	return p, e, item
}

func TestPreflightRetentionPartitionIdentity(t *testing.T) {
	p, e, item := preflightEnforcer(t, true)
	if err := e.Apply(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	var future, deleted int
	if err := p.QueryRow(context.Background(), "SELECT count(*) FROM "+item.Invariant.Table+
		" WHERE id=2").Scan(&future); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(context.Background(), `SELECT deleted_rows FROM sage.retention_run
		WHERE table_name=$1 AND disposition='applied'`, item.Invariant.Table).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if future != 1 || deleted > 1 {
		t.Fatalf("partition identity/batch limit violated: future rows=%d want=1, deleted=%d limit=1", future, deleted)
	}
}

func TestPreflightRetentionInvalidatesChangedContract(t *testing.T) {
	p, e, item := preflightEnforcer(t, false)
	ctx := context.Background()
	_, err := p.Exec(ctx, "INSERT INTO "+item.Invariant.Table+" VALUES (3,now()-interval '10 days')")
	if err != nil {
		t.Fatal(err)
	}
	item.Contract.RetentionWindow = 24 * time.Hour
	err = e.Apply(ctx, item)
	var remaining int
	if scanErr := p.QueryRow(ctx, "SELECT count(*) FROM "+item.Invariant.Table).Scan(&remaining); scanErr != nil {
		t.Fatal(scanErr)
	}
	if err == nil || remaining != 3 {
		t.Fatalf("30-day dry run reused for 1-day contract: err=%v remaining=%d want=3", err, remaining)
	}
}

func TestPreflightRetentionAuditFailureDoesNotSilentlyDelete(t *testing.T) {
	p, e, item := preflightEnforcer(t, false)
	ctx := context.Background()
	// Real PostgreSQL trigger failure after DELETE, not a mocked repository error.
	_, err := p.Exec(ctx, `CREATE FUNCTION public.preflight_deny_audit() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.disposition='applied' THEN RAISE EXCEPTION 'preflight injected audit failure'; END IF;
		RETURN NEW; END $$;
		CREATE TRIGGER preflight_deny_audit BEFORE INSERT ON sage.retention_run
		FOR EACH ROW EXECUTE FUNCTION public.preflight_deny_audit()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, `DROP TRIGGER preflight_deny_audit ON sage.retention_run;
		DROP FUNCTION public.preflight_deny_audit()`)
	})
	err = e.Apply(ctx, item)
	if err == nil || !strings.Contains(err.Error(), "preflight injected audit failure") {
		t.Fatalf("expected injected failure, got %v", err)
	}
	var remaining, applied int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM "+item.Invariant.Table).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FROM sage.retention_run
		WHERE table_name=$1 AND disposition='applied'`, item.Invariant.Table).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 || applied != 0 {
		t.Fatalf("mutation committed without durable audit: remaining=%d want=2 applied=%d", remaining, applied)
	}
}

func TestPreflightRetentionNonpartitionedControl(t *testing.T) {
	p, e, item := preflightEnforcer(t, false)
	if err := e.Apply(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	var ids []int
	rows, err := p.Query(context.Background(), "SELECT id FROM "+item.Invariant.Table+" ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("eligible deletion control: ids=%v want=[2]", ids)
	}
}
