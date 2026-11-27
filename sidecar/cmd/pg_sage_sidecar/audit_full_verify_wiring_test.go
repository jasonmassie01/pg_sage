package main

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// Verification sources carry audit.full_verify_interval_hours as read when
// the sources are listed; without a config every pass is full.
func TestAuditSourcesCarryTheFullVerifyInterval(t *testing.T) {
	prev := cfg
	t.Cleanup(func() { cfg = prev })
	control := &pgxpool.Pool{}
	cfg = config.DefaultConfig()
	sources := auditSources(control, nil)
	if got := sources(); len(got) != 1 || got[0].FullVerifyHours != 168 {
		t.Fatalf("default config: %+v, want a 168 h full interval", got)
	}
	cfg.Audit.FullVerifyIntervalHours = 0
	if got := sources(); len(got) != 1 || got[0].FullVerifyHours != 0 {
		t.Fatalf("interval 0: %+v, want every pass full", got)
	}
	cfg = nil
	if got := sources(); len(got) != 1 || got[0].FullVerifyHours != 0 {
		t.Fatalf("no config: %+v, want every pass full", got)
	}
}
