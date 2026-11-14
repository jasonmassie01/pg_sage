package runway

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// ci-17: the XID runway is cluster-wide. TestMonitorTick_LowAgeBurn...
// measured its slope on the shared test server, where other packages'
// databases consume XIDs between its samples; their XIDs, compressed into
// its 100 ms sample spacing, doubled the slope and moved the projection
// inside the 72 h critical horizon ("past the gate: severity critical").
// The reproduction burns XIDs in another database of the shared server
// while the test runs.
//
// No state-transition or invalid-input cases: this pins the fixture's
// isolation; the gate's own cases are in monitor_gate_db_test.go.

// xidBurners consume XIDs on dsn from n sessions until stop is called.
func xidBurners(t *testing.T, dsn string, n int) (stop func()) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect the XID burners: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var mu sync.Mutex
	var burnErr error
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_, err := pool.Exec(ctx, `SELECT pg_catalog.pg_current_xact_id()`)
				if err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
					mu.Lock()
					burnErr = err
					mu.Unlock()
					return
				}
			}
		}()
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			wg.Wait()
			pool.Close()
			if burnErr != nil {
				t.Errorf("XID burner: %v", burnErr)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// Integration: XIDs consumed in another database of the shared server
// while the low-age burn is sampled leave its severity a warning.
func TestMonitorTick_LowAgeBurnIgnoresOtherDatabasesXIDs(t *testing.T) {
	other := testdb.CreateDatabase(t, "runway_xid_burner")
	stop := xidBurners(t, other, 6)
	t.Run("low_age_burn", TestMonitorTick_LowAgeBurnOpensNoWraparoundFinding)
	stop()
}
