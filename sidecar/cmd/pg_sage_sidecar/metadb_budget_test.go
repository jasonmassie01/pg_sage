package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

// No concurrent access tests: initMetaDB runs once, at startup, on one pool.

// useMetaSchemaBootstrap swaps the meta schema boot for the test.
func useMetaSchemaBootstrap(
	t *testing.T, fn func(context.Context, *pgxpool.Pool) error,
) {
	t.Helper()
	saved := metaSchemaBootstrap
	metaSchemaBootstrap = fn
	t.Cleanup(func() { metaSchemaBootstrap = saved })
}

// Error propagation and boundary: the boot gets a deadline of at most
// metaDBTimeout, and its error comes back naming the meta-db schema.
func TestBootstrapMetaSchemaHasItsOwnBudgetAndNamesItsError(t *testing.T) {
	boom := errors.New("boom")
	var left time.Duration
	useMetaSchemaBootstrap(t, func(ctx context.Context, _ *pgxpool.Pool) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("the meta schema boot has no deadline")
		}
		left = time.Until(deadline)
		return boom
	})
	err := bootstrapMetaSchema(nil)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "bootstrapping meta-db schema") {
		t.Fatalf("err = %v, want the boot's error wrapped as the meta-db schema", err)
	}
	if left <= metaDBTimeout-time.Second || left > metaDBTimeout {
		t.Fatalf("boot deadline %s away, want just under %s", left, metaDBTimeout)
	}
	useMetaSchemaBootstrap(t, func(context.Context, *pgxpool.Pool) error { return nil })
	if err := bootstrapMetaSchema(nil); err != nil {
		t.Fatalf("a boot that succeeds returned %v", err)
	}
}

// Happy path on a real server (T7 test round 1): a schema boot that spends
// nearly all of its budget still leaves the admin bootstrap and the steps
// after it a full metaDBTimeout. Before, one 10 s budget covered both, and a
// slow first boot under load failed with "creating admin: context deadline
// exceeded".
func TestInitMetaDBSlowSchemaBootLeavesTheRestTheirBudget(t *testing.T) {
	state, _, _ := metaLifecycleFixture(t)
	useMetaSchemaBootstrap(t, func(ctx context.Context, p *pgxpool.Pool) error {
		if err := schema.Bootstrap(ctx, p); err != nil {
			return err
		}
		deadline, _ := ctx.Deadline()
		time.Sleep(time.Until(deadline) - 5*time.Millisecond) // done just in time
		return nil
	})
	start := time.Now()
	got, err := initMetaDB(state.Pool, "fixture-encryption-passphrase")
	if err != nil || got == nil || len(got.EncryptKey) != 32 || got.Store == nil {
		t.Fatalf("init after a slow boot: err=%v state present=%v", err, got != nil)
	}
	if took := time.Since(start); took < metaDBTimeout-time.Second {
		t.Fatalf("init took %s; the stand-in boot did not use its budget", took)
	}
}
