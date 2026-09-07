package vectorlab

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func restrictedPool(t *testing.T, owner *pgxpool.Pool) (*pgxpool.Pool, string) {
	t.Helper()
	role := fmt.Sprintf("vectorlab_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{role}.Sanitize()
	if _, err := owner.Exec(t.Context(), "CREATE ROLE "+quoted); err != nil {
		t.Fatal(err)
	}
	cfg := owner.Config().Copy()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+quoted)
		return err
	}
	p, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Close()
		if _, err := owner.Exec(context.Background(), "DROP OWNED BY "+quoted); err != nil {
			t.Error(err)
		}
		if _, err := owner.Exec(context.Background(), "DROP ROLE "+quoted); err != nil {
			t.Error(err)
		}
	})
	return p, quoted
}

func TestPostgresPermissionDenialAndRLS(t *testing.T) {
	owner := testPool(t)
	m := seedVectors(t, owner)
	p, role := restrictedPool(t, owner)
	if _, err := Run(t.Context(), p, m); err == nil || !strings.Contains(err.Error(), "42501") {
		t.Fatalf("permission denied was not distinguishable: %v", err)
	}
	table := pgx.Identifier{m.Schema, m.Table}.Sanitize()
	for _, sql := range []string{
		"GRANT SELECT ON " + table + " TO " + role,
		"ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY hidden ON " + table + " USING (tenant = 999)",
	} {
		if _, err := owner.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Run(t.Context(), p, m)
	if err != nil || r.Recommendation != "" || r.Variants[0].Queries[0].TruthCount != 0 {
		t.Fatalf("RLS not preserved: %#v %v", r, err)
	}
}

func TestPostgresRejectsUnsafeIdentityAndView(t *testing.T) {
	p := testPool(t)
	m := seedVectors(t, p)
	base := pgx.Identifier{m.Schema, m.Table}.Sanitize()
	view := pgx.Identifier{m.Schema, m.Table + "_view"}.Sanitize()
	if _, err := p.Exec(t.Context(), "CREATE VIEW "+view+" AS SELECT * FROM "+base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := p.Exec(context.Background(), "DROP VIEW "+view); err != nil {
			t.Error(err)
		}
	})
	m.Table += "_view"
	if _, err := Run(t.Context(), p, m); err == nil ||
		!strings.Contains(err.Error(), "ordinary table") {
		t.Fatalf("view accepted: %v", err)
	}
	m.Table = strings.TrimSuffix(m.Table, "_view")
	m.IDColumn = "tenant"
	if _, err := Run(t.Context(), p, m); err == nil || !strings.Contains(err.Error(), "unique") {
		t.Fatalf("unsafe identity accepted: %v", err)
	}
}

func TestPostgresReadOnlyTransactionRejectsMutation(t *testing.T) {
	p := testPool(t)
	tx, err := p.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(t.Context(), "CREATE TABLE forbidden (id int)"); err == nil ||
		!strings.Contains(safeError("mutation", err).Error(), "25006") {
		t.Fatalf("read-only transaction did not reject mutation with 25006: %v", err)
	}
}
