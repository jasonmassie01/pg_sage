package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Sequential by design: these probes change the disposable database's global stop flag.
// Unknown/nil dependency checks remain in the existing constructor tests.
func TestPreflightRetentionHonorsRuntimeControls(t *testing.T) {
	for _, control := range []string{"emergency_stop", "observation", "manual", "disabled"} {
		t.Run(control, func(t *testing.T) {
			p := preflightRuntimePool(t)
			ctx := context.Background()
			table := preflightRetentionTable(t, p)
			c := config.DefaultConfig()
			c.Trust.Level = "autonomous"
			if control == "observation" {
				c.Trust.Level = "observation"
			}
			e := executor.New(p, c, nil, time.Now().Add(-90*24*time.Hour), nil)
			e.SetExecutionMode("auto")
			if control == "manual" {
				e.SetExecutionMode("manual")
			}
			if control == "disabled" {
				e.SetExecutorEnabled(false)
			}
			if err := executor.SetEmergencyStop(ctx, p, control == "emergency_stop"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = executor.SetEmergencyStop(ctx, p, false) })
			supervisor, err := newDatabaseAutonomy(p, c, "preflight", e)
			if err != nil {
				t.Fatal(err)
			}
			for cycle := 1; cycle <= 2; cycle++ {
				if err := supervisor.TriggerSchemaGuard(ctx, "preflight"); err != nil {
					t.Fatal(err)
				}
			}
			var remaining, applied int
			if err := p.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if err := p.QueryRow(ctx, `SELECT count(*) FROM sage.retention_run
				WHERE table_name=$1 AND disposition='applied'`, table).Scan(&applied); err != nil {
				t.Fatal(err)
			}
			if remaining != 2 || applied != 0 {
				t.Fatalf("%s violated: remaining=%d want=2, applied=%d want=0", control, remaining, applied)
			}
		})
	}
}

func preflightRuntimePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if err := schema.Bootstrap(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func preflightRetentionTable(t *testing.T, p *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	table := fmt.Sprintf("preflight_retention_%d", time.Now().UnixNano())
	_, err := p.Exec(ctx, "CREATE TABLE "+table+" (id int, created_at timestamptz); "+
		"INSERT INTO "+table+" VALUES (1,'2020-01-01'),(2,'2099-01-01')")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, "DELETE FROM sage.table_contract WHERE table_name=$1", table)
		_, _ = p.Exec(ctx, "DELETE FROM sage.retention_run WHERE table_name=$1", table)
		_, _ = p.Exec(ctx, "DROP TABLE "+table)
	})
	_, err = p.Exec(ctx, `INSERT INTO sage.table_contract
		(schema_name,table_name,append_only,retention_interval,declared_by,evidence_id)
		VALUES ('public',$1,true,interval '30 days','preflight',$1)`, table)
	if err != nil {
		t.Fatal(err)
	}
	_, err = policy.NewStore(p).Bootstrap(ctx, policy.Scope{}, "unattended", "preflight")
	if err != nil {
		t.Fatal(err)
	}
	return table
}
