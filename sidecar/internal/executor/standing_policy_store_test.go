package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// G5-B11: fleet executors must read standing policy from the control pool
// the API writes to, not from each monitored database.
func TestEnableStandingPolicyWithStoreUsesPolicyPool(t *testing.T) {
	pool, ctx := requireDB(t)
	cfg := config.DefaultConfig()
	exec := New(pool, cfg, time.Time{}, nopLog)

	closedCfg, err := pgxpool.ParseConfig(pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	closed, err := pgxpool.NewWithConfig(context.Background(), closedCfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	closed.Close()

	err = exec.EnableStandingPolicyWithStore(ctx, closed, "staffed", nil)
	if err == nil || !strings.Contains(err.Error(), "bootstrap standing policy") {
		t.Fatalf("policy store not bound to the control pool: err=%v", err)
	}
	if err := exec.EnableStandingPolicyWithStore(ctx, nil, "staffed", nil); err != nil {
		t.Fatalf("nil control pool must fall back to the executor pool: %v", err)
	}
}
