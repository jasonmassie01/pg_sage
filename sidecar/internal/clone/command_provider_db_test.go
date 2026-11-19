package clone

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/restorecmd"
)

// drill-01c integration: the restore command provider behind real
// receipts (purpose drill), with the clone on the second test cluster.

func commandReceipting(t *testing.T, inner Provider, store *ReceiptStore) *ReceiptingProvider {
	t.Helper()
	p, err := NewReceiptingProvider(inner, store, ReceiptingConfig{Adapter: CommandAdapter,
		Scope: "command/test", Purpose: PurposeDrill, TTL: time.Hour,
		Deployment:    func(context.Context) (string, error) { return testDeployment, nil },
		AllowFullCopy: true, MaxCreatesPerHour: 60, Backoff: NewCreateBackoff(time.Now)})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// secondClusterCommand is a command config whose program prints a
// database on the second test cluster.
func secondClusterCommand(t *testing.T, mode string) (CommandConfig, string) {
	t.Helper()
	dsn := testdb.RestoreTargetDatabase(t, "drill_cmd")
	cfg := commandConfig(t, mode)
	cfg.LookupEnv = func(name string) (string, bool) {
		if name == restorecmd.EnvDSN {
			return dsn, true
		}
		return "", false
	}
	return cfg, dsn
}

func receiptStatusOf(t *testing.T, pool *pgxpool.Pool, ref string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM sage.clone_instances
		WHERE ref = $1 AND purpose = 'drill'`, ref).Scan(&status); err != nil {
		t.Fatalf("receipt of %s: %v", ref, err)
	}
	return status
}

func TestCommandDB_CreateConnectDestroy(t *testing.T) {
	store, pool, ctx := receiptStore(t)
	cfg, dsn := secondClusterCommand(t, "ok")
	p := commandReceipting(t, newCommand(t, cfg), store)
	cl, err := p.Create(ctx, CloneSpec{IncludeData: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cl.DSN != dsn {
		t.Fatalf("DSN = %q, want the second-cluster database", cl.DSN)
	}
	if got := receiptStatusOf(t, pool, cl.ID); got != StatusReady {
		t.Fatalf("receipt = %s, want ready", got)
	}
	target, err := GuardedConnect(ctx, cl, PoolIdentity(pool))
	if err != nil {
		t.Fatalf("GuardedConnect to the restored clone: %v", err)
	}
	var one int
	if err := target.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1 on the clone = %d, %v", one, err)
	}
	target.Close()
	if err := p.Destroy(ctx, cl); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if got := receiptStatusOf(t, pool, cl.ID); got != StatusDestroyed {
		t.Fatalf("receipt = %s, want destroyed", got)
	}
}

func TestCommandDB_DestroyFailureKeepsTheReceiptForTheReaper(t *testing.T) {
	store, pool, ctx := receiptStore(t)
	cfg, _ := secondClusterCommand(t, "ok")
	cfg.DestroyArgv = restorecmd.Argv("destroy-fail")
	p := commandReceipting(t, newCommand(t, cfg), store)
	cl, err := p.Create(ctx, CloneSpec{IncludeData: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := p.Destroy(ctx, cl); err == nil {
		t.Fatal("Destroy succeeded, want the destroy program's failure")
	}
	if got := receiptStatusOf(t, pool, cl.ID); got != StatusDestroying {
		t.Fatalf("receipt = %s, want destroying (left for the reaper)", got)
	}
}

func TestCommandDB_TimeoutMarksTheReceiptCreateUncertain(t *testing.T) {
	store, pool, ctx := receiptStore(t)
	cfg := commandConfig(t, "sleep")
	cfg.MaxDuration = 700 * time.Millisecond
	p := commandReceipting(t, newCommand(t, cfg), store)
	if _, err := p.Create(ctx, CloneSpec{IncludeData: true}); err == nil {
		t.Fatal("Create of a program past its bound succeeded")
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM sage.clone_instances
		WHERE purpose = 'drill'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != StatusCreateUncertain {
		t.Fatalf("receipt = %s, want create_uncertain", status)
	}
}

func TestCommandDB_FailedProgramClosesTheReceiptFailed(t *testing.T) {
	store, pool, ctx := receiptStore(t)
	p := commandReceipting(t, newCommand(t, commandConfig(t, "fail")), store)
	if _, err := p.Create(ctx, CloneSpec{IncludeData: true}); err == nil {
		t.Fatal("Create of a failing program succeeded")
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM sage.clone_instances
		WHERE purpose = 'drill'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != StatusFailed {
		t.Fatalf("receipt = %s, want failed", status)
	}
}

// Two drills on two databases run two processes at once; each sees only
// its own receipt's SAGE_DRILL_ID (the program prints it back as the id).
func TestCommandDB_ConcurrentDrillsSeeTheirOwnDrillID(t *testing.T) {
	store, pool, ctx := receiptStore(t)
	cfg, _ := secondClusterCommand(t, "drill-id")
	p := commandReceipting(t, newCommand(t, cfg), store)
	var wg sync.WaitGroup
	clones := make([]Clone, 2)
	errs := make([]error, 2)
	for i := range clones {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			clones[i], errs[i] = p.Create(ctx, CloneSpec{IncludeData: true})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("drill %d: %v", i, err)
		}
	}
	if clones[0].ID == clones[1].ID {
		t.Fatalf("both drills got id %q", clones[0].ID)
	}
	rows, err := pool.Query(ctx, "SELECT "+receiptColumns+
		` FROM sage.clone_instances WHERE purpose = 'drill' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		r, err := scanReceipt(rows)
		if err != nil {
			t.Fatal(err)
		}
		n++
		if r.Ref != ResourceName(r) {
			t.Errorf("receipt %d: program saw drill id %q, want its own %q", r.ID,
				r.Ref, ResourceName(r))
		}
	}
	if n != 2 {
		t.Fatalf("%d drill receipts, want 2", n)
	}
	for _, cl := range clones {
		if err := p.Destroy(ctx, cl); err != nil {
			t.Errorf("Destroy %s: %v", cl.ID, err)
		}
	}
}
