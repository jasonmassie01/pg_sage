//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// A create holds one meta connection for its cap transaction; it must not
// need a second one, or a pool whose connections are all held by creates
// waiting on the cap lock deadlocks until the create times out.
func TestCreate_NeedsOnlyOneMetaConnection(t *testing.T) {
	_, ctx := requireDBWithClean(t)
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("one-connection pool: %v", err)
	}
	defer one.Close()
	ds := NewDatabaseStore(one, testKey)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, create := range []func() (int, error){
		func() (int, error) { return ds.Create(cctx, tierInput("one-full"), 1) },
		func() (int, error) { return ds.CreateObserveOnly(cctx, tierInput("one-obs"), 1) },
	} {
		id, err := create()
		if err != nil {
			t.Fatalf("create on a one-connection pool: %v", err)
		}
		rec, err := ds.Get(ctx, id)
		if err != nil || (rec.Name != "one-full" && rec.Name != "one-obs") {
			t.Fatalf("member %d: %+v (%v)", id, rec, err)
		}
		if _, err := ds.GetConnectionString(ctx, id); err != nil {
			t.Fatalf("member %d's sealed password does not open: %v", id, err)
		}
	}
}
