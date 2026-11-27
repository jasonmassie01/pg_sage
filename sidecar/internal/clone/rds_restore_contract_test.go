package clone_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/clone/contracttest"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// lab-05d: the lab-05a clone contract and receipt lifecycle on the RDS
// restore fake, against a real receipt database.

const rdsDeployment = "00000000-0000-4000-8000-0000000005d0"

var rdsProdIdentity = clone.ServerIdentity{Host: rdsProdHost, Port: 5432,
	StartedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}

func rdsReceiptPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.clone_instances WHERE deployment_id = $1::uuid", rdsDeployment)
	}
	clean()
	t.Cleanup(clean)
	return pool
}

func TestRDSRestoreContract_FakeMeetsTheContract(t *testing.T) {
	pool := rdsReceiptPool(t)
	contracttest.Run(t, func(t *testing.T) contracttest.Subject {
		f := newRDSFake(t)
		return contracttest.Subject{Adapter: clone.RDSRestoreAdapter,
			Provider: newRDSProvider(t, f, nil), Production: rdsProdIdentity,
			Exists: func(_ context.Context, ref string) (bool, error) {
				return f.exists(ref), nil
			}}
	}, contracttest.Options{Pool: pool, DeploymentID: rdsDeployment})
}

func rdsReceipting(t *testing.T, pool *pgxpool.Pool, p clone.Provider,
	allowFullCopy bool) *clone.ReceiptingProvider {
	t.Helper()
	life, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	r, err := clone.NewReceiptingProvider(p, clone.NewReceiptStore(pool),
		clone.ReceiptingConfig{Adapter: clone.RDSRestoreAdapter, Scope: "aws/" + rdsRegion,
			Deployment: func(context.Context) (string, error) { return rdsDeployment, nil },
			Purpose:    clone.PurposeRehearsal, TTL: 3*time.Hour + 15*time.Minute,
			AllowFullCopy: allowFullCopy, Holder: "rds-test-holder", Lifetime: life,
			Backoff: clone.NewCreateBackoff(nil)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type rdsReceiptRow struct {
	status, ref, kind string
	hours             float64
}

func rdsReceipts(t *testing.T, pool *pgxpool.Pool) []rdsReceiptRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT status, COALESCE(ref, ''),
		COALESCE(estimate->>'kind', ''), COALESCE((estimate->>'instance_hours')::float8, 0)
		FROM sage.clone_instances WHERE deployment_id = $1::uuid AND adapter = $2
		ORDER BY id`, rdsDeployment, clone.RDSRestoreAdapter)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []rdsReceiptRow
	for rows.Next() {
		var r rdsReceiptRow
		if err := rows.Scan(&r.status, &r.ref, &r.kind, &r.hours); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// State transitions: creating -> ready -> destroying -> destroyed, with
// the full_copy cost label of the whole receipt TTL.
func TestRDSRestoreReceipting_ReadyThenDestroyed(t *testing.T) {
	pool := rdsReceiptPool(t)
	f := newRDSFake(t)
	p := rdsReceipting(t, pool, newRDSProvider(t, f, nil), true)
	c, err := p.Create(context.Background(), clone.CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	got := rdsReceipts(t, pool)
	if len(got) != 1 || got[0] != (rdsReceiptRow{clone.StatusReady, c.ID, "full_copy",
		3.25}) {
		t.Fatalf("receipts after create = %+v, want one ready full_copy naming %s", got, c.ID)
	}
	if err := p.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := rdsReceipts(t, pool); got[0].status != clone.StatusDestroyed || f.exists(c.ID) {
		t.Fatalf("receipts after destroy = %+v (instance exists %v)", got, f.exists(c.ID))
	}
}

// Invalid configuration: no opt-in, no receipt and no RDS call.
func TestRDSRestoreReceipting_NoOptInWritesNoReceipt(t *testing.T) {
	pool := rdsReceiptPool(t)
	f := newRDSFake(t)
	p := rdsReceipting(t, pool, newRDSProvider(t, f, nil), false)
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); err == nil {
		t.Fatal("a full copy was created without clone.allow_full_copy")
	}
	if got := rdsReceipts(t, pool); len(got) != 0 || f.callCount() != 0 {
		t.Fatalf("receipts = %+v, RDS calls %d; want none", got, f.callCount())
	}
}

func TestRDSRestoreReceipting_DeleteFailureLeavesTheReceiptDestroying(t *testing.T) {
	pool := rdsReceiptPool(t)
	f := newRDSFake(t)
	p := rdsReceipting(t, pool, newRDSProvider(t, f, nil), true)
	c, err := p.Create(context.Background(), clone.CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	f.deleteErr = &clone.ProviderStatusError{Op: "delete", StatusCode: 500}
	if err := p.Destroy(context.Background(), c); err == nil {
		t.Fatal("destroy succeeded while the instance delete failed")
	}
	if got := rdsReceipts(t, pool); len(got) != 1 ||
		got[0].status != clone.StatusDestroying || !f.exists(c.ID) {
		t.Fatalf("receipts = %+v (instance exists %v), want destroying", got, f.exists(c.ID))
	}
}

// Boundary: a restore past clone.restore_deadline is create_uncertain, and
// the next process's reaper finds it by tag and deletes it.
func TestRDSRestoreReceipting_PastTheDeadlineIsReapedByTag(t *testing.T) {
	pool := rdsReceiptPool(t)
	f := newRDSFake(t)
	f.neverReady = true
	api := newRDSAPI(t, f, nil)
	clone.SetRDSRestoreDeadlineForTest(api, 50*time.Millisecond)
	inner, err := clone.NewSnapshotProvider(api, func() time.Time { return rdsNow })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rdsReceipting(t, pool, inner, true).Create(context.Background(),
		clone.CloneSpec{}); err == nil {
		t.Fatal("a restore that never became available was handed out")
	}
	got := rdsReceipts(t, pool)
	if len(got) != 1 || got[0].status != clone.StatusCreateUncertain || got[0].ref != "" {
		t.Fatalf("receipts = %+v, want one create_uncertain", got)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE sage.clone_instances
		SET heartbeat_at = now() - interval '1 hour' WHERE deployment_id = $1::uuid`,
		rdsDeployment); err != nil {
		t.Fatal(err)
	}
	rep, err := clone.Reaper{Store: clone.NewReceiptStore(pool), Provider: inner,
		Scope: clone.ReceiptScope{DeploymentID: rdsDeployment,
			Adapter: clone.RDSRestoreAdapter, Scope: api.TargetScope()},
		Purpose: clone.PurposeRehearsal, Holder: "rds-next-process"}.Reap(
		context.Background())
	if err != nil || len(rep.Destroyed) != 1 {
		t.Fatalf("reap = %+v, %v: want the uncertain restore destroyed", rep, err)
	}
	if got := rdsReceipts(t, pool); got[0].status != clone.StatusDestroyed ||
		got[0].ref == "" || f.exists(got[0].ref) {
		t.Fatalf("receipts = %+v", got)
	}
}
