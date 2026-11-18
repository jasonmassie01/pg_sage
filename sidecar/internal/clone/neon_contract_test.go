package clone_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/clone/contracttest"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// lab-05b: the lab-05a clone contract and receipt lifecycle, on the Neon
// fake, against a real receipt database.

const neonDeployment = "00000000-0000-4000-8000-00000000ae05"

var neonProdIdentity = clone.ServerIdentity{Host: neonProdHost, Port: 5432,
	StartedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}

func neonReceiptPool(t *testing.T) *pgxpool.Pool {
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
			"DELETE FROM sage.clone_instances WHERE deployment_id = $1::uuid", neonDeployment)
	}
	clean()
	t.Cleanup(clean)
	return pool
}

func TestNeonContract_FakeMeetsTheContract(t *testing.T) {
	pool := neonReceiptPool(t)
	contracttest.Run(t, func(t *testing.T) contracttest.Subject {
		f := newNeonFake(t)
		return contracttest.Subject{Adapter: clone.NeonAdapter,
			Provider: newNeonProvider(t, f, nil), Production: neonProdIdentity,
			Exists: func(_ context.Context, ref string) (bool, error) {
				return f.exists(ref), nil
			}}
	}, contracttest.Options{Pool: pool, DeploymentID: neonDeployment})
}

func neonReceipting(t *testing.T, pool *pgxpool.Pool,
	p *clone.NeonBranchProvider) *clone.ReceiptingProvider {
	t.Helper()
	life, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	r, err := clone.NewReceiptingProvider(p, clone.NewReceiptStore(pool),
		clone.ReceiptingConfig{Adapter: clone.NeonAdapter, Scope: p.TargetScope(),
			Deployment: func(context.Context) (string, error) { return neonDeployment, nil },
			Purpose:    clone.PurposeRehearsal, TTL: time.Hour, Holder: "neon-test-holder",
			Lifetime: life, Backoff: clone.NewCreateBackoff(nil)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type neonReceiptRow struct {
	status, ref, kind string
}

func neonReceipts(t *testing.T, pool *pgxpool.Pool) []neonReceiptRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT status, COALESCE(ref, ''),
		COALESCE(estimate->>'kind', '') FROM sage.clone_instances
		WHERE deployment_id = $1::uuid AND adapter = $2 ORDER BY id`,
		neonDeployment, clone.NeonAdapter)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []neonReceiptRow
	for rows.Next() {
		var r neonReceiptRow
		if err := rows.Scan(&r.status, &r.ref, &r.kind); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNeonReceipting_ReadyThenDestroyed(t *testing.T) {
	pool := neonReceiptPool(t)
	f := newNeonFake(t)
	p := neonReceipting(t, pool, newNeonProvider(t, f, nil))
	c, err := p.Create(context.Background(), clone.CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	got := neonReceipts(t, pool)
	if len(got) != 1 || got[0] != (neonReceiptRow{clone.StatusReady, c.ID, "branch"}) {
		t.Fatalf("receipts after create = %+v, want one ready naming %s", got, c.ID)
	}
	if err := p.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := neonReceipts(t, pool); got[0].status != clone.StatusDestroyed || f.exists(c.ID) {
		t.Fatalf("receipts after destroy = %+v (branch exists %v)", got, f.exists(c.ID))
	}
}

func TestNeonReceipting_RefusedCreateIsFailed(t *testing.T) {
	pool := neonReceiptPool(t)
	f := newNeonFake(t)
	f.createStatus = http.StatusUnauthorized
	p := neonReceipting(t, pool, newNeonProvider(t, f, nil))
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); err == nil {
		t.Fatal("create succeeded on a 401")
	}
	if got := neonReceipts(t, pool); len(got) != 1 || got[0].status != clone.StatusFailed {
		t.Fatalf("receipts = %+v, want one failed", got)
	}
}

func TestNeonReceipting_UncertainCreateIsReapedByName(t *testing.T) {
	pool := neonReceiptPool(t)
	f := newNeonFake(t)
	f.opsNeverFinish = true
	inner := newNeonProvider(t, f, func(c *clone.NeonConfig) {
		c.WaitTimeout = 50 * time.Millisecond
	})
	p := neonReceipting(t, pool, inner)
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); !errors.Is(err,
		context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the poll deadline", err)
	}
	got := neonReceipts(t, pool)
	if len(got) != 1 || got[0].status != clone.StatusCreateUncertain || got[0].ref != "" {
		t.Fatalf("receipts = %+v, want one create_uncertain", got)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE sage.clone_instances
		SET heartbeat_at = now() - interval '1 hour' WHERE deployment_id = $1::uuid`,
		neonDeployment); err != nil {
		t.Fatal(err)
	}
	rep, err := clone.Reaper{Store: clone.NewReceiptStore(pool), Provider: inner,
		Scope: clone.ReceiptScope{DeploymentID: neonDeployment, Adapter: clone.NeonAdapter,
			Scope: inner.TargetScope()}, Purpose: clone.PurposeRehearsal,
		Holder: "neon-next-process"}.Reap(context.Background())
	if err != nil || len(rep.Destroyed) != 1 {
		t.Fatalf("reap = %+v, %v: want the uncertain branch destroyed", rep, err)
	}
	if got := neonReceipts(t, pool); got[0].status != clone.StatusDestroyed ||
		f.countRequests("DELETE ") != 1 {
		t.Fatalf("receipts = %+v, deletes %d", got, f.countRequests("DELETE "))
	}
}

// Concurrent access: two receipted creates at once give two receipts and
// two branches (run with -race where cgo is available).
func TestNeonReceipting_ConcurrentCreatesGiveTwoReceipts(t *testing.T) {
	pool := neonReceiptPool(t)
	f := newNeonFake(t)
	p := neonReceipting(t, pool, newNeonProvider(t, f, nil))
	var wg sync.WaitGroup
	clones := make([]clone.Clone, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			clones[i], errs[i] = p.Create(context.Background(), clone.CloneSpec{})
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("creates: %v, %v", errs[0], errs[1])
	}
	got := neonReceipts(t, pool)
	if len(got) != 2 || got[0].ref == got[1].ref || clones[0].ID == clones[1].ID {
		t.Fatalf("receipts = %+v, clones %+v: want two distinct", got, clones)
	}
	for _, r := range got {
		if r.status != clone.StatusReady || !f.exists(r.ref) {
			t.Fatalf("receipt %+v: want ready on a live branch", r)
		}
	}
}
