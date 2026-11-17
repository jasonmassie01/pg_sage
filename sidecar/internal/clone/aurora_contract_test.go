package clone_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/clone/contracttest"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// lab-05c: the lab-05a clone contract and receipt lifecycle on the Aurora
// fake, against a real receipt database.

const auroraDeployment = "00000000-0000-4000-8000-00000000ae5c"

var auroraProdIdentity = clone.ServerIdentity{Host: auroraProdHost, Port: 5432,
	StartedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}

func auroraReceiptPool(t *testing.T) *pgxpool.Pool {
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
			"DELETE FROM sage.clone_instances WHERE deployment_id = $1::uuid", auroraDeployment)
	}
	clean()
	t.Cleanup(clean)
	return pool
}

func TestAuroraContract_FakeMeetsTheContract(t *testing.T) {
	pool := auroraReceiptPool(t)
	contracttest.Run(t, func(t *testing.T) contracttest.Subject {
		f := newAuroraFake(t)
		// pgx reads an sslrootcert file when it parses a DSN, so the
		// contract's DSN names none.
		p := newAuroraProvider(t, f, func(c *clone.AuroraConfig) {
			c.Production = func(context.Context) (clone.AuroraProduction, error) {
				return clone.AuroraProduction{DSN: "postgres://app:" + auroraPassword + "@" +
					auroraProdHost + ":5432/shop?sslmode=require"}, nil
			}
		})
		return contracttest.Subject{Adapter: clone.AuroraAdapter,
			Provider: p, Production: auroraProdIdentity,
			Exists: func(_ context.Context, ref string) (bool, error) {
				return f.exists(ref), nil
			}}
	}, contracttest.Options{Pool: pool, DeploymentID: auroraDeployment})
}

func auroraReceipting(t *testing.T, pool *pgxpool.Pool,
	p *clone.AuroraCloneProvider) *clone.ReceiptingProvider {
	t.Helper()
	life, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	r, err := clone.NewReceiptingProvider(p, clone.NewReceiptStore(pool),
		clone.ReceiptingConfig{Adapter: clone.AuroraAdapter, Scope: p.TargetScope(),
			Deployment: func(context.Context) (string, error) { return auroraDeployment, nil },
			Purpose:    clone.PurposeRehearsal, TTL: time.Hour, Holder: "aurora-test-holder",
			Lifetime: life, Backoff: clone.NewCreateBackoff(nil)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type auroraReceiptRow struct{ status, ref, kind string }

func auroraReceipts(t *testing.T, pool *pgxpool.Pool) []auroraReceiptRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT status, COALESCE(ref, ''),
		COALESCE(estimate->>'kind', '') FROM sage.clone_instances
		WHERE deployment_id = $1::uuid AND adapter = $2 ORDER BY id`,
		auroraDeployment, clone.AuroraAdapter)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auroraReceiptRow
	for rows.Next() {
		var r auroraReceiptRow
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

// State transitions: creating -> ready -> destroying -> destroyed, with
// the copy_on_write cost label, and no opt-in needed.
func TestAuroraReceipting_ReadyThenDestroyed(t *testing.T) {
	pool := auroraReceiptPool(t)
	f := newAuroraFake(t)
	p := auroraReceipting(t, pool, newAuroraProvider(t, f, nil))
	c, err := p.Create(context.Background(), clone.CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	got := auroraReceipts(t, pool)
	if len(got) != 1 || got[0] != (auroraReceiptRow{clone.StatusReady, c.ID,
		"copy_on_write"}) {
		t.Fatalf("receipts after create = %+v, want one ready naming %s", got, c.ID)
	}
	if err := p.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := auroraReceipts(t, pool); got[0].status != clone.StatusDestroyed ||
		f.exists(c.ID) {
		t.Fatalf("receipts after destroy = %+v (cluster exists %v)", got, f.exists(c.ID))
	}
}

func TestAuroraReceipting_DeleteFailureLeavesTheReceiptDestroying(t *testing.T) {
	pool := auroraReceiptPool(t)
	f := newAuroraFake(t)
	p := auroraReceipting(t, pool, newAuroraProvider(t, f, nil))
	c, err := p.Create(context.Background(), clone.CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	f.deleteClusterErr = errors.New("cluster delete refused")
	if err := p.Destroy(context.Background(), c); err == nil {
		t.Fatal("destroy succeeded while the cluster delete failed")
	}
	if got := auroraReceipts(t, pool); len(got) != 1 ||
		got[0].status != clone.StatusDestroying || !f.exists(c.ID) {
		t.Fatalf("receipts = %+v (cluster exists %v), want destroying", got, f.exists(c.ID))
	}
}

func TestAuroraReceipting_CloneLimitIsAFailedReceipt(t *testing.T) {
	pool := auroraReceiptPool(t)
	f := newAuroraFake(t)
	f.addClones(15)
	p := auroraReceipting(t, pool, newAuroraProvider(t, f, nil))
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); !errors.Is(err,
		clone.ErrAuroraCloneLimit) {
		t.Fatalf("err = %v, want the clone limit", err)
	}
	if got := auroraReceipts(t, pool); len(got) != 1 || got[0].status != clone.StatusFailed {
		t.Fatalf("receipts = %+v, want one failed", got)
	}
}

func TestAuroraReceipting_UncertainCreateIsReapedByTag(t *testing.T) {
	pool := auroraReceiptPool(t)
	f := newAuroraFake(t)
	f.neverReady = true
	inner := newAuroraProvider(t, f, func(c *clone.AuroraConfig) {
		c.WaitTimeout = 50 * time.Millisecond
	})
	p := auroraReceipting(t, pool, inner)
	if _, err := p.Create(context.Background(), clone.CloneSpec{}); !errors.Is(err,
		context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the wait deadline", err)
	}
	got := auroraReceipts(t, pool)
	if len(got) != 1 || got[0].status != clone.StatusCreateUncertain || got[0].ref != "" {
		t.Fatalf("receipts = %+v, want one create_uncertain", got)
	}
	f.neverReady = false
	if _, err := pool.Exec(context.Background(), `UPDATE sage.clone_instances
		SET heartbeat_at = now() - interval '1 hour' WHERE deployment_id = $1::uuid`,
		auroraDeployment); err != nil {
		t.Fatal(err)
	}
	rep, err := clone.Reaper{Store: clone.NewReceiptStore(pool), Provider: inner,
		Scope: clone.ReceiptScope{DeploymentID: auroraDeployment,
			Adapter: clone.AuroraAdapter, Scope: inner.TargetScope()},
		Purpose: clone.PurposeRehearsal, Holder: "aurora-next-process"}.Reap(
		context.Background())
	if err != nil || len(rep.Destroyed) != 1 {
		t.Fatalf("reap = %+v, %v: want the uncertain clone destroyed", rep, err)
	}
	if got := auroraReceipts(t, pool); got[0].status != clone.StatusDestroyed ||
		got[0].ref == "" || f.exists(got[0].ref) {
		t.Fatalf("receipts = %+v", got)
	}
}
