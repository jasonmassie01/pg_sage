package clone_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/clone/contracttest"
)

// lab-05g: the lab-05a clone contract and receipt lifecycle on the ARM
// fake, against a real receipt database.

const azDeployment = "00000000-0000-4000-8000-0000000005a9"

var azProdIdentity = clone.ServerIdentity{Host: azProdFQDN, Port: 5432,
	StartedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}

func azReceiptPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := rdsReceiptPool(t)
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.clone_instances WHERE deployment_id = $1::uuid", azDeployment)
	}
	clean()
	t.Cleanup(clean)
	return pool
}

func TestAzureRestoreContract_FakeMeetsTheContract(t *testing.T) {
	pool := azReceiptPool(t)
	contracttest.Run(t, func(t *testing.T) contracttest.Subject {
		f, base := newAzureFake(t)
		return contracttest.Subject{Adapter: clone.AzureRestoreAdapter,
			Provider: newAzureProvider(t, base, nil), Production: azProdIdentity,
			Exists: func(_ context.Context, ref string) (bool, error) {
				return f.exists(ref), nil
			}}
	}, contracttest.Options{Pool: pool, DeploymentID: azDeployment})
}

func azReceipting(t *testing.T, pool *pgxpool.Pool, p clone.Provider,
	allowFullCopy bool) *clone.ReceiptingProvider {
	t.Helper()
	life, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	r, err := clone.NewReceiptingProvider(p, clone.NewReceiptStore(pool),
		clone.ReceiptingConfig{Adapter: clone.AzureRestoreAdapter,
			Scope:      "azure/" + azSub + "/" + azGroup,
			Deployment: func(context.Context) (string, error) { return azDeployment, nil },
			Purpose:    clone.PurposeRehearsal, TTL: 3*time.Hour + 15*time.Minute,
			AllowFullCopy: allowFullCopy, Holder: "az-test-holder", Lifetime: life,
			Backoff: clone.NewCreateBackoff(nil)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func azReceipts(t *testing.T, pool *pgxpool.Pool) []rdsReceiptRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT status, COALESCE(ref, ''),
		COALESCE(estimate->>'kind', ''), COALESCE((estimate->>'instance_hours')::float8, 0)
		FROM sage.clone_instances WHERE deployment_id = $1::uuid AND adapter = $2
		ORDER BY id`, azDeployment, clone.AzureRestoreAdapter)
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

func azReap(t *testing.T, pool *pgxpool.Pool, inner clone.Provider,
	api *clone.AzureRestoreAPI) clone.ReapReport {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE sage.clone_instances
		SET heartbeat_at = now() - interval '1 hour', expires_at = now() - interval '1 hour'
		WHERE deployment_id = $1::uuid`, azDeployment); err != nil {
		t.Fatal(err)
	}
	rep, err := clone.Reaper{Store: clone.NewReceiptStore(pool), Provider: inner,
		Scope: clone.ReceiptScope{DeploymentID: azDeployment,
			Adapter: clone.AzureRestoreAdapter, Scope: api.TargetScope()},
		Purpose: clone.PurposeRehearsal, Holder: "az-next-process"}.Reap(
		context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// State transitions: creating -> ready -> destroying -> destroyed, with
// the full_copy cost label of the whole receipt TTL (instance-hours).
func TestAzureRestoreReceipting_ReadyThenDestroyed(t *testing.T) {
	pool := azReceiptPool(t)
	f, base := newAzureFake(t)
	p := azReceipting(t, pool, newAzureProvider(t, base, nil), true)
	c, err := p.Create(context.Background(), clone.CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	got := azReceipts(t, pool)
	if len(got) != 1 || got[0] != (rdsReceiptRow{clone.StatusReady, c.ID, "full_copy",
		3.25}) {
		t.Fatalf("receipts after create = %+v, want one ready full_copy naming %s", got, c.ID)
	}
	if f.server(c.ID).Tags[clone.AzureReceiptTag] != c.ID {
		t.Fatalf("server %s tags %v", c.ID, f.server(c.ID).Tags)
	}
	if err := p.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := azReceipts(t, pool); got[0].status != clone.StatusDestroyed || f.exists(c.ID) {
		t.Fatalf("receipts after destroy = %+v (server exists %v)", got, f.exists(c.ID))
	}
}

// Invalid configuration: no opt-in, no receipt and no ARM call.
func TestAzureRestoreReceipting_NoOptInWritesNoReceipt(t *testing.T) {
	pool := azReceiptPool(t)
	f, base := newAzureFake(t)
	p := azReceipting(t, pool, newAzureProvider(t, base, nil), false)
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); err == nil {
		t.Fatal("a full copy was created without clone.allow_full_copy")
	}
	if got := azReceipts(t, pool); len(got) != 0 || f.callCount() != 0 {
		t.Fatalf("receipts = %+v, ARM calls %d; want none", got, f.callCount())
	}
}

// Error propagation: a failed delete keeps the receipt destroying, so the
// reaper tries again.
func TestAzureRestoreReceipting_DeleteFailureLeavesTheReceiptDestroying(t *testing.T) {
	pool := azReceiptPool(t)
	f, base := newAzureFake(t)
	p := azReceipting(t, pool, newAzureProvider(t, base, nil), true)
	c, err := p.Create(context.Background(), clone.CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	f.deleteStatus = http.StatusInternalServerError
	if err := p.Destroy(context.Background(), c); err == nil {
		t.Fatal("destroy succeeded while the server delete failed")
	}
	if got := azReceipts(t, pool); len(got) != 1 ||
		got[0].status != clone.StatusDestroying || !f.exists(c.ID) {
		t.Fatalf("receipts = %+v (server exists %v), want destroying", got, f.exists(c.ID))
	}
}

// Boundary: a restore still running at the restore deadline is
// create_uncertain; the next process's reaper finds the tagged server by
// its name and deletes it.
func TestAzureRestoreReceipting_PastTheDeadlineIsReapedByName(t *testing.T) {
	pool := azReceiptPool(t)
	f, base := newAzureFake(t)
	f.putNever = true
	api := newAzureAPI(t, base, nil)
	clone.SetAzureRestoreDeadlineForTest(api, 50*time.Millisecond)
	inner, err := clone.NewSnapshotProvider(api, func() time.Time { return azNow })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := azReceipting(t, pool, inner, true).Create(context.Background(),
		clone.CloneSpec{}); err == nil {
		t.Fatal("a restore that never finished was handed out")
	}
	got := azReceipts(t, pool)
	if len(got) != 1 || got[0].status != clone.StatusCreateUncertain || got[0].ref != "" {
		t.Fatalf("receipts = %+v, want one create_uncertain", got)
	}
	if rep := azReap(t, pool, inner, api); len(rep.Destroyed) != 1 {
		t.Fatalf("reap = %+v: want the uncertain server destroyed", rep)
	}
	if got := azReceipts(t, pool); got[0].status != clone.StatusDestroyed ||
		got[0].ref == "" || f.exists(got[0].ref) {
		t.Fatalf("receipts = %+v", got)
	}
}

// A restored server without its receipt tag is left behind and never
// deleted, and the reaper must not close its receipt as gone: the
// billable copy of production would no longer be tracked.
func TestAzureRestoreReceipting_UntaggedServerKeepsItsReceiptOpen(t *testing.T) {
	pool := azReceiptPool(t)
	f, base := newAzureFake(t)
	f.dropTags = true
	api := newAzureAPI(t, base, nil)
	inner, err := clone.NewSnapshotProvider(api, func() time.Time { return azNow })
	if err != nil {
		t.Fatal(err)
	}
	_, err = azReceipting(t, pool, inner, true).Create(context.Background(), clone.CloneSpec{})
	if !errors.Is(err, clone.ErrCloneLeftBehind) {
		t.Fatalf("create = %v, want left behind", err)
	}
	rep := azReap(t, pool, inner, api)
	if len(rep.Unresolved) != 1 || len(rep.Destroyed) != 0 || len(rep.FindFailed) != 1 {
		t.Fatalf("reap = %+v: want the receipt unresolved", rep)
	}
	for _, ferr := range rep.FindFailed {
		if !errors.Is(ferr, clone.ErrCloneLeftBehind) {
			t.Fatalf("find error = %v, want left behind", ferr)
		}
	}
	if got := azReceipts(t, pool); len(got) != 1 ||
		got[0].status != clone.StatusCreateUncertain {
		t.Fatalf("receipts = %+v, want one still create_uncertain", got)
	}
	if name := clone.ResourceName(rep.Unresolved[0]); !f.exists(name) {
		t.Fatalf("untagged server %s was deleted", name)
	}
}
