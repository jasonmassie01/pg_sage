package hasource

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/ha"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/earned/hasource"))
}

func noopLog(string, string, ...any) {}

// The HA adapter reads the database's one monitor (fleet-04): its last
// check, kept current by the HA loop and refreshed by the executor just
// before each cycle. It reports the live primary after a check.
func TestHAMonitorSourceReadsTheLiveRole(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	m := ha.New(pool, noopLog)
	m.Check(context.Background())
	st, err := FromSnapshot(m).HAStatus(context.Background())
	if err != nil || st.Role != earned.RolePrimary || st.SafeMode ||
		!st.LastRoleChange.IsZero() {
		t.Fatalf("live primary = %+v (%v)", st, err)
	}
}

// Nil: no monitor is an error, which the limiter turns into a downgrade.
func TestFromSnapshotWithoutAMonitorFailsClosed(t *testing.T) {
	if _, err := FromSnapshot(nil).HAStatus(context.Background()); err == nil {
		t.Fatal("a nil monitor must be an error (fail closed)")
	}
	var s *Source
	if _, err := s.HAStatus(context.Background()); err == nil {
		t.Fatal("a nil source must be an error (fail closed)")
	}
}

// Footprint: reading the source never probes the monitored database.
func TestFromSnapshotNeverProbes(t *testing.T) {
	var calls atomic.Int64
	m := ha.NewFromProbe(func(context.Context) (bool, error) {
		calls.Add(1)
		return false, nil
	}, noopLog)
	src := FromSnapshot(m)
	st, err := src.HAStatus(context.Background())
	if err != nil || st.Role != earned.RoleUnknown {
		t.Fatalf("before any check = %+v (%v), want unknown", st, err)
	}
	m.Check(context.Background())
	for i := 0; i < 5; i++ {
		st, err = src.HAStatus(context.Background())
		if err != nil || st.Role != earned.RolePrimary {
			t.Fatalf("after a check = %+v (%v), want primary", st, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("one check and six reads probed %d times, want 1", calls.Load())
	}
}

// Error propagation and state: a failed probe reads as unknown, and the
// monitor's safe mode and last change pass through.
func TestFromSnapshotCarriesFailureAndSafeMode(t *testing.T) {
	answers := []error{nil, nil, nil, errors.New("connection refused")}
	roles := []bool{false, true, false, false}
	i := 0
	m := ha.NewFromProbe(func(context.Context) (bool, error) {
		r, e := roles[i], answers[i]
		i++
		return r, e
	}, noopLog)
	ctx := context.Background()
	for n := 0; n < 3; n++ {
		m.Check(ctx)
	}
	st, err := FromSnapshot(m).HAStatus(ctx)
	if err != nil || st.Role != earned.RolePrimary || !st.SafeMode ||
		st.LastRoleChange.IsZero() {
		t.Fatalf("after two flips = %+v (%v), want primary in safe mode", st, err)
	}
	m.Check(ctx)
	st, err = FromSnapshot(m).HAStatus(ctx)
	if err != nil || st.Role != earned.RoleUnknown || !st.SafeMode {
		t.Fatalf("after a failed probe = %+v (%v), want unknown, still in safe mode", st, err)
	}
}
