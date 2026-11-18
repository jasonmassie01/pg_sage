package clone_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/clone/contracttest"
)

// lab-05f: the lab-05a clone contract and receipt lifecycle on the AlloyDB
// fake, against a real receipt database.

const adbDeployment = "00000000-0000-4000-8000-0000000005f0"

var adbProdIdentity = clone.ServerIdentity{Host: adbProdIP, Port: 5432,
	StartedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}

func adbReceiptPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := rdsReceiptPool(t)
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.clone_instances WHERE deployment_id = $1::uuid", adbDeployment)
	}
	clean()
	t.Cleanup(clean)
	return pool
}

func TestAlloyDBContract_FakeMeetsTheContract(t *testing.T) {
	pool := adbReceiptPool(t)
	contracttest.Run(t, func(t *testing.T) contracttest.Subject {
		f, base := newAlloyDBFake(t)
		return contracttest.Subject{Adapter: clone.AlloyDBRestoreAdapter,
			Provider: newAlloyDBProvider(t, base, nil), Production: adbProdIdentity,
			Exists: func(_ context.Context, ref string) (bool, error) {
				return f.exists(ref), nil
			}}
	}, contracttest.Options{Pool: pool, DeploymentID: adbDeployment})
}

func adbReceipting(t *testing.T, pool *pgxpool.Pool, p clone.Provider,
	allowFullCopy bool) *clone.ReceiptingProvider {
	t.Helper()
	life, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	r, err := clone.NewReceiptingProvider(p, clone.NewReceiptStore(pool),
		clone.ReceiptingConfig{Adapter: clone.AlloyDBRestoreAdapter,
			Scope:      "gcp/" + adbProject + "/" + adbLocation,
			Deployment: func(context.Context) (string, error) { return adbDeployment, nil },
			Purpose:    clone.PurposeRehearsal, TTL: 3*time.Hour + 15*time.Minute,
			AllowFullCopy: allowFullCopy, Holder: "adb-test-holder", Lifetime: life,
			Backoff: clone.NewCreateBackoff(nil)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func adbReceipts(t *testing.T, pool *pgxpool.Pool) []rdsReceiptRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT status, COALESCE(ref, ''),
		COALESCE(estimate->>'kind', ''), COALESCE((estimate->>'instance_hours')::float8, 0)
		FROM sage.clone_instances WHERE deployment_id = $1::uuid AND adapter = $2
		ORDER BY id`, adbDeployment, clone.AlloyDBRestoreAdapter)
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
// the full_copy cost label of the whole receipt TTL (instance-hours).
func TestAlloyDBReceipting_ReadyThenDestroyed(t *testing.T) {
	pool := adbReceiptPool(t)
	f, base := newAlloyDBFake(t)
	p := adbReceipting(t, pool, newAlloyDBProvider(t, base, nil), true)
	c, err := p.Create(context.Background(), clone.CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	got := adbReceipts(t, pool)
	if len(got) != 1 || got[0] != (rdsReceiptRow{clone.StatusReady, c.ID, "full_copy",
		3.25}) {
		t.Fatalf("receipts after create = %+v, want one ready full_copy naming %s", got, c.ID)
	}
	if f.cluster(c.ID).Labels[clone.AlloyDBReceiptLabel] != c.ID {
		t.Fatalf("cluster %s labels %v", c.ID, f.cluster(c.ID).Labels)
	}
	if err := p.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := adbReceipts(t, pool); got[0].status != clone.StatusDestroyed || f.exists(c.ID) {
		t.Fatalf("receipts after destroy = %+v (cluster exists %v)", got, f.exists(c.ID))
	}
}

// Invalid configuration: no opt-in, no receipt and no AlloyDB call.
func TestAlloyDBReceipting_NoOptInWritesNoReceipt(t *testing.T) {
	pool := adbReceiptPool(t)
	f, base := newAlloyDBFake(t)
	p := adbReceipting(t, pool, newAlloyDBProvider(t, base, nil), false)
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); err == nil {
		t.Fatal("a full copy was created without clone.allow_full_copy")
	}
	if got := adbReceipts(t, pool); len(got) != 0 || f.callCount() != 0 {
		t.Fatalf("receipts = %+v, AlloyDB calls %d; want none", got, f.callCount())
	}
}

// Boundary: a restore still running at the restore deadline is
// create_uncertain; the next process's reaper finds the labelled cluster
// by its name and deletes it.
func TestAlloyDBReceipting_PastTheDeadlineIsReapedByName(t *testing.T) {
	pool := adbReceiptPool(t)
	f, base := newAlloyDBFake(t)
	f.restoreNever = true
	api := newAlloyDBAPI(t, base, nil)
	clone.SetAlloyDBDeadlineForTest(api, 50*time.Millisecond)
	inner, err := clone.NewSnapshotProvider(api, func() time.Time { return adbNow })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adbReceipting(t, pool, inner, true).Create(context.Background(),
		clone.CloneSpec{}); err == nil {
		t.Fatal("a restore that never finished was handed out")
	}
	got := adbReceipts(t, pool)
	if len(got) != 1 || got[0].status != clone.StatusCreateUncertain || got[0].ref != "" {
		t.Fatalf("receipts = %+v, want one create_uncertain", got)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE sage.clone_instances
		SET heartbeat_at = now() - interval '1 hour' WHERE deployment_id = $1::uuid`,
		adbDeployment); err != nil {
		t.Fatal(err)
	}
	rep, err := clone.Reaper{Store: clone.NewReceiptStore(pool), Provider: inner,
		Scope: clone.ReceiptScope{DeploymentID: adbDeployment,
			Adapter: clone.AlloyDBRestoreAdapter, Scope: api.TargetScope()},
		Purpose: clone.PurposeRehearsal, Holder: "adb-next-process"}.Reap(
		context.Background())
	if err != nil || len(rep.Destroyed) != 1 {
		t.Fatalf("reap = %+v, %v: want the uncertain cluster destroyed", rep, err)
	}
	if got := adbReceipts(t, pool); got[0].status != clone.StatusDestroyed ||
		got[0].ref == "" || f.exists(got[0].ref) {
		t.Fatalf("receipts = %+v", got)
	}
}

// Error propagation: a restored cluster without its receipt label is left
// behind and never deleted; the reaper must not close its receipt as gone
// once it expires, or the billable copy of production would no longer be
// tracked. The receipt stays create_uncertain and the lookup error says why.
func TestAlloyDBReceipting_UnlabelledClusterKeepsItsReceiptOpen(t *testing.T) {
	pool := adbReceiptPool(t)
	f, base := newAlloyDBFake(t)
	f.dropLabels = true
	api := newAlloyDBAPI(t, base, nil)
	inner, err := clone.NewSnapshotProvider(api, func() time.Time { return adbNow })
	if err != nil {
		t.Fatal(err)
	}
	_, err = adbReceipting(t, pool, inner, true).Create(context.Background(),
		clone.CloneSpec{})
	if !errors.Is(err, clone.ErrCloneLeftBehind) {
		t.Fatalf("create = %v, want left behind", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE sage.clone_instances
		SET heartbeat_at = now() - interval '1 hour', expires_at = now() - interval '1 hour'
		WHERE deployment_id = $1::uuid`, adbDeployment); err != nil {
		t.Fatal(err)
	}
	rep, err := clone.Reaper{Store: clone.NewReceiptStore(pool), Provider: inner,
		Scope: clone.ReceiptScope{DeploymentID: adbDeployment,
			Adapter: clone.AlloyDBRestoreAdapter, Scope: api.TargetScope()},
		Purpose: clone.PurposeRehearsal, Holder: "adb-next-process"}.Reap(
		context.Background())
	if err != nil || len(rep.Unresolved) != 1 || len(rep.Destroyed) != 0 ||
		len(rep.FindFailed) != 1 {
		t.Fatalf("reap = %+v, %v: want the receipt unresolved", rep, err)
	}
	for _, ferr := range rep.FindFailed {
		if !errors.Is(ferr, clone.ErrCloneLeftBehind) {
			t.Fatalf("find error = %v, want left behind", ferr)
		}
	}
	got := adbReceipts(t, pool)
	if len(got) != 1 || got[0].status != clone.StatusCreateUncertain {
		t.Fatalf("receipts = %+v, want one still create_uncertain", got)
	}
	if name := clone.ResourceName(rep.Unresolved[0]); !f.exists(name) {
		t.Fatalf("unlabelled cluster %s was deleted", name)
	}
}
