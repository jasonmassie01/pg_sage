//go:build perfgate

package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/shadow"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// perfShadowSeenEvery is how often the harness re-sees a pending shadow
// decision, as the executor does each cycle while pg_sage still wants the
// same change. The fixture's decisions are vacuums no finding of the
// synthetic database proposes, so without this the run would update
// sage.shadow_decision only by scoring it, and gate F would never see the
// recurring seen bump.
const perfShadowSeenEvery = 2 * time.Second

// startPerfShadowSeen re-sees the fixture's pending shadow decisions
// through the shipped store until stopped. The stop function fails the
// test when no bump found a pending decision.
func startPerfShadowSeen(t *testing.T, dsn string) func() {
	t.Helper()
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("shadow seen dsn: %v", err)
	}
	pc.MaxConns = 1
	pc.ConnConfig.RuntimeParams["application_name"] = "perfgate_shadow_seen"
	sp, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatalf("shadow seen pool: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var bumps atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPerfShadowSeen(ctx, t, shadow.NewStore(sp), &bumps)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
			sp.Close()
			if bumps.Load() == 0 {
				t.Errorf("no seen bump found a pending shadow decision: gate F did not " +
					"measure the recurring update path")
			}
			t.Logf("shadow decisions re-seen %d times", bumps.Load())
		})
	}
	t.Cleanup(stop)
	return stop
}

func runPerfShadowSeen(ctx context.Context, t *testing.T, store *shadow.Store,
	bumps *atomic.Int64) {
	fps := perfgate.ShadowSeenFingerprints()
	ticker := time.NewTicker(perfShadowSeenEvery)
	defer ticker.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		seen, err := store.Seen(ctx, nil, fps[i%len(fps)], 24*time.Hour)
		if err != nil {
			if ctx.Err() == nil {
				t.Errorf("re-see shadow decision %s: %v", fps[i%len(fps)], err)
			}
			return
		}
		if seen {
			bumps.Add(1)
		}
	}
}
