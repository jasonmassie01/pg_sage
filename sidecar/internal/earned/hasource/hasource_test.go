package hasource

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/ha"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/earned/hasource"))
}

// The HA adapter probes the role on every read, so a downgrade sees the
// role at authorization time, not a cached one.
func TestHAMonitorSourceProbesTheLiveRole(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	src := New(ha.New(pool, func(string, string, ...any) {}))
	st, err := src.HAStatus(context.Background())
	if err != nil || st.Role != earned.RolePrimary || st.SafeMode ||
		!st.LastRoleChange.IsZero() {
		t.Fatalf("live primary = %+v (%v)", st, err)
	}
	if _, err := New(nil).HAStatus(context.Background()); err == nil {
		t.Fatal("a nil monitor must be an error (fail closed)")
	}
}
