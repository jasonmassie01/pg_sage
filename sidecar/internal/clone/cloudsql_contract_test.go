package clone_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/clone/contracttest"
)

// lab-05e: the lab-05a clone contract and receipt lifecycle on the Cloud
// SQL fake, against a real receipt database.

const sqlDeployment = "00000000-0000-4000-8000-0000000005e0"

var sqlProdIdentity = clone.ServerIdentity{Host: sqlProdIP, Port: 5432,
	StartedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}

func sqlReceiptPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := rdsReceiptPool(t)
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.clone_instances WHERE deployment_id = $1::uuid", sqlDeployment)
	}
	clean()
	t.Cleanup(clean)
	return pool
}

func TestCloudSQLContract_FakeMeetsTheContract(t *testing.T) {
	pool := sqlReceiptPool(t)
	contracttest.Run(t, func(t *testing.T) contracttest.Subject {
		f, base := newCloudSQLFake(t)
		return contracttest.Subject{Adapter: clone.CloudSQLCloneAdapter,
			Provider: newCloudSQLProvider(t, base, nil), Production: sqlProdIdentity,
			Exists: func(_ context.Context, ref string) (bool, error) {
				return f.exists(ref), nil
			}}
	}, contracttest.Options{Pool: pool, DeploymentID: sqlDeployment})
}

func sqlReceipting(t *testing.T, pool *pgxpool.Pool, p clone.Provider,
	allowFullCopy bool) *clone.ReceiptingProvider {
	t.Helper()
	life, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	r, err := clone.NewReceiptingProvider(p, clone.NewReceiptStore(pool),
		clone.ReceiptingConfig{Adapter: clone.CloudSQLCloneAdapter, Scope: "gcp/" + sqlProject,
			Deployment: func(context.Context) (string, error) { return sqlDeployment, nil },
			Purpose:    clone.PurposeRehearsal, TTL: 3*time.Hour + 15*time.Minute,
			AllowFullCopy: allowFullCopy, Holder: "sql-test-holder", Lifetime: life,
			Backoff: clone.NewCreateBackoff(nil)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func sqlReceipts(t *testing.T, pool *pgxpool.Pool) []rdsReceiptRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT status, COALESCE(ref, ''),
		COALESCE(estimate->>'kind', ''), COALESCE((estimate->>'instance_hours')::float8, 0)
		FROM sage.clone_instances WHERE deployment_id = $1::uuid AND adapter = $2
		ORDER BY id`, sqlDeployment, clone.CloudSQLCloneAdapter)
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
func TestCloudSQLReceipting_ReadyThenDestroyed(t *testing.T) {
	pool := sqlReceiptPool(t)
	f, base := newCloudSQLFake(t)
	p := sqlReceipting(t, pool, newCloudSQLProvider(t, base, nil), true)
	c, err := p.Create(context.Background(), clone.CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	got := sqlReceipts(t, pool)
	if len(got) != 1 || got[0] != (rdsReceiptRow{clone.StatusReady, c.ID, "full_copy",
		3.25}) {
		t.Fatalf("receipts after create = %+v, want one ready full_copy naming %s", got, c.ID)
	}
	if f.instance(c.ID).Labels[clone.CloudSQLReceiptLabel] != c.ID {
		t.Fatalf("clone %s labels %v", c.ID, f.instance(c.ID).Labels)
	}
	if err := p.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := sqlReceipts(t, pool); got[0].status != clone.StatusDestroyed || f.exists(c.ID) {
		t.Fatalf("receipts after destroy = %+v (instance exists %v)", got, f.exists(c.ID))
	}
}

// Invalid configuration: no opt-in, no receipt and no Cloud SQL call.
func TestCloudSQLReceipting_NoOptInWritesNoReceipt(t *testing.T) {
	pool := sqlReceiptPool(t)
	f, base := newCloudSQLFake(t)
	p := sqlReceipting(t, pool, newCloudSQLProvider(t, base, nil), false)
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); err == nil {
		t.Fatal("a full copy was created without clone.allow_full_copy")
	}
	if got := sqlReceipts(t, pool); len(got) != 0 || f.callCount() != 0 {
		t.Fatalf("receipts = %+v, Cloud SQL calls %d; want none", got, f.callCount())
	}
}

// Boundary: a clone still running at the restore deadline is
// create_uncertain; the next process's reaper finds the unlabelled clone
// by its name, lifts the copied deletion protection and deletes it.
func TestCloudSQLReceipting_PastTheDeadlineIsReapedByName(t *testing.T) {
	pool := sqlReceiptPool(t)
	f, base := newCloudSQLFake(t)
	f.neverDone = true
	api := newCloudSQLAPI(t, base, nil)
	clone.SetCloudSQLDeadlineForTest(api, 50*time.Millisecond)
	inner, err := clone.NewSnapshotProvider(api, func() time.Time { return sqlNow })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlReceipting(t, pool, inner, true).Create(context.Background(),
		clone.CloneSpec{}); err == nil {
		t.Fatal("a clone that never finished was handed out")
	}
	got := sqlReceipts(t, pool)
	if len(got) != 1 || got[0].status != clone.StatusCreateUncertain || got[0].ref != "" {
		t.Fatalf("receipts = %+v, want one create_uncertain", got)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE sage.clone_instances
		SET heartbeat_at = now() - interval '1 hour' WHERE deployment_id = $1::uuid`,
		sqlDeployment); err != nil {
		t.Fatal(err)
	}
	rep, err := clone.Reaper{Store: clone.NewReceiptStore(pool), Provider: inner,
		Scope: clone.ReceiptScope{DeploymentID: sqlDeployment,
			Adapter: clone.CloudSQLCloneAdapter, Scope: api.TargetScope()},
		Purpose: clone.PurposeRehearsal, Holder: "sql-next-process"}.Reap(
		context.Background())
	if err != nil || len(rep.Destroyed) != 1 {
		t.Fatalf("reap = %+v, %v: want the uncertain clone destroyed", rep, err)
	}
	if got := sqlReceipts(t, pool); got[0].status != clone.StatusDestroyed ||
		got[0].ref == "" || f.exists(got[0].ref) {
		t.Fatalf("receipts = %+v", got)
	}
}
