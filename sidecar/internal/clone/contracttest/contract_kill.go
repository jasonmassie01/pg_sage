package contracttest

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
)

// The killed-process checks: a process records and creates a clone, dies
// (its context ends, nothing is destroyed), and the next process's first
// reaper pass reaps the clone before its TTL.

const (
	holderA = "contract-holder-a"
	holderB = "contract-holder-b"
)

func contractScope(t *testing.T, s Subject, opts Options) (clone.ReceiptScope,
	*pgxpool.Pool) {
	t.Helper()
	if opts.Pool == nil {
		t.Fatal("the clone contract needs Options.Pool, a bootstrapped receipt database")
	}
	scope := clone.ReceiptScope{DeploymentID: opts.DeploymentID, Adapter: s.Adapter,
		Scope: "contract/" + t.Name()}
	t.Cleanup(func() {
		_, _ = opts.Pool.Exec(context.Background(), `DELETE FROM sage.clone_instances
			WHERE deployment_id = $1::uuid AND scope = $2`, scope.DeploymentID, scope.Scope)
	})
	return scope, opts.Pool
}

func checkKillAfterReady(t *testing.T, s Subject, opts Options) {
	scope, pool := contractScope(t, s, opts)
	ctx := context.Background()
	life, kill := context.WithCancel(ctx)
	defer kill()
	p, err := clone.NewReceiptingProvider(s.Provider, clone.NewReceiptStore(pool),
		clone.ReceiptingConfig{Adapter: scope.Adapter, Scope: scope.Scope,
			Deployment: func(context.Context) (string, error) { return scope.DeploymentID, nil },
			Purpose:    clone.PurposeRehearsal, TTL: time.Hour, AllowFullCopy: true,
			MaxCreatesPerHour: clone.DefaultMaxCreatesPerHour, Holder: holderA,
			Lifetime: life, Backoff: clone.NewCreateBackoff(nil)})
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Create(ctx, clone.CloneSpec{})
	if err != nil {
		t.Fatalf("%s create: %v", s.Adapter, err)
	}
	kill()
	id, ok := p.ReceiptOf(c.ID)
	if !ok {
		t.Fatalf("%s clone %s has no receipt", s.Adapter, c.ID)
	}
	staleHeartbeat(t, pool, id)
	rep := reapAsNextProcess(t, s, scope, pool)
	if len(rep.Destroyed) != 1 || rep.Destroyed[0].ID != id {
		t.Fatalf("the next process did not reap receipt %d: %+v", id, rep)
	}
	assertGone(t, s, c.ID)
}

// killBetweenCreateAndReady leaves a creating receipt whose clone exists
// and returns the next process's first reaper pass.
func killBetweenCreateAndReady(t *testing.T, s Subject, opts Options) clone.ReapReport {
	t.Helper()
	scope, pool := contractScope(t, s, opts)
	ctx := context.Background()
	store := clone.NewReceiptStore(pool)
	r, err := store.RecordLimited(ctx, clone.Receipt{DeploymentID: scope.DeploymentID,
		Adapter: scope.Adapter, Scope: scope.Scope, Name: receiptName(t),
		Purpose: clone.PurposeRehearsal, HolderID: holderA,
		ExpiresAt: time.Now().Add(time.Hour)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Provider.Create(ctx, clone.CloneSpec{ResourceName: clone.ResourceName(r)})
	if err != nil {
		t.Fatalf("%s create: %v", s.Adapter, err)
	}
	t.Cleanup(func() { _ = s.Provider.Destroy(context.Background(), c) })
	staleHeartbeat(t, pool, r.ID)
	rep := reapAsNextProcess(t, s, scope, pool)
	if len(rep.Destroyed) == 1 {
		assertGone(t, s, c.ID)
	}
	return rep
}

func reapAsNextProcess(t *testing.T, s Subject, scope clone.ReceiptScope,
	pool *pgxpool.Pool) clone.ReapReport {
	t.Helper()
	rep, err := clone.Reaper{Store: clone.NewReceiptStore(pool), Provider: s.Provider,
		Scope: scope, Purpose: clone.PurposeRehearsal, Holder: holderB}.Reap(
		context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func staleHeartbeat(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE sage.clone_instances
		SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

func receiptName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%s-%x-%x-%x-%x-%x", clone.PurposeRehearsal, b[0:4], b[4:6],
		b[6:8], b[8:10], b[10:16])
}
