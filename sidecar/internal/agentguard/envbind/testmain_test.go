package envbind

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/agentguard/envbind"))
}

// controlPool connects to the package fixture (the control database),
// bootstrapped.
func controlPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return bootstrapped(t, ctx, dsn), ctx
}

func bootstrapped(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

// extraPool creates another database on the same cluster (same system
// identifier, another database OID), bootstrapped.
func extraPool(t *testing.T, ctx context.Context, label string) *pgxpool.Pool {
	t.Helper()
	return bootstrapped(t, ctx, testdb.CreateDatabase(t, label))
}

// newID is a fresh database_id; its label row and findings are removed
// when the test ends.
func newID(t *testing.T, control *pgxpool.Pool, monitored ...*pgxpool.Pool) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	id := fmt.Sprintf("%s-%s-%s-%s-%s", h[:8], h[8:12], h[12:16], h[16:20], h[20:])
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = control.Exec(ctx, `DELETE FROM sage.guard_environment_labels
			WHERE database_id = $1`, id)
		for _, p := range monitored {
			_, _ = p.Exec(ctx, `DELETE FROM sage.findings WHERE object_identifier = $1`, id)
		}
	})
	return id
}
