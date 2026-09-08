package autoexplain

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDetectAlreadyLoadedModuleWithoutLoadPrivilege(t *testing.T) {
	admin := acquireTestPool(t)
	ctx := context.Background()
	role := fmt.Sprintf("pgsage_ae_detect_%d", os.Getpid())
	name := pgx.Identifier{role}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE ROLE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(ctx, "DROP ROLE "+name)
		if err != nil {
			t.Error(err)
		}
	})
	settings, err := pgxpool.ParseConfig(os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	settings.MaxConns = 1
	settings.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "LOAD 'auto_explain'"); err != nil {
			return err
		}
		_, err := conn.Exec(ctx, "SET ROLE "+name)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "LOAD 'auto_explain'"); err == nil {
		t.Fatal("fixture role unexpectedly has LOAD privilege")
	}
	available, err := Detect(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !available.Available || available.SessionLoad || available.Method != "already_loaded" {
		t.Fatalf("provider-loaded module misclassified: %#v", available)
	}
}
