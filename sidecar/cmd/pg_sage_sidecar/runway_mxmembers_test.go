package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/runway"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// guard-02a: production reads the multixact member space at most every
// five minutes, never more often than the runway interval allows, and the
// options it builds validate.
// No concurrent access tests: runwayOptions is a pure function of config.

// idleRunway is a runner NewMonitor accepts; no probe runs here.
type idleRunway struct{ noProbes }

func (idleRunway) RunBackground(context.Context, probes.ID, probes.Args) probes.Result {
	return probes.Result{}
}

func TestRunwayOptions_MultixactMemberInterval(t *testing.T) {
	// The pool never connects: NewMonitor only validates.
	pool, err := pgxpool.New(context.Background(), "postgres://sage@127.0.0.1:1/none")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, c := range []struct {
		intervalS int
		want      time.Duration
	}{
		{60, runway.DefaultMXMemberInterval},
		{300, 5 * time.Minute},
		{900, 15 * time.Minute},
	} {
		cfg := config.DefaultConfig()
		cfg.SRE.Runways.IntervalSeconds = c.intervalS
		o := runwayOptions(cfg, "orders")
		if o.MXMemberInterval != c.want {
			t.Errorf("interval %d s: member interval %s, want %s", c.intervalS,
				o.MXMemberInterval, c.want)
		}
		if _, err := runway.NewMonitor(pool, idleRunway{}, nil, o, nil); err != nil {
			t.Errorf("interval %d s: options rejected: %v", c.intervalS, err)
		}
	}
}
