package main

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// Fleet WAL-runway dedupe wiring: every database runtime of the process
// gets the same size share, so runtimes on one cluster measure the
// databases' total size once per pass between them.
func TestRunwayOptions_ShareOneSizeShareAcrossRuntimes(t *testing.T) {
	cfg := config.DefaultConfig()
	a, b := runwayOptions(cfg, "orders"), runwayOptions(cfg, "billing")
	if a.Sizes == nil || a.Sizes != b.Sizes {
		t.Fatalf("size shares %p and %p, want one process-wide share", a.Sizes, b.Sizes)
	}
	if a.Database != "orders" || b.Database != "billing" {
		t.Fatalf("databases %q %q", a.Database, b.Database)
	}
}

// The size sampling cadence reaches the monitor: the configured period,
// clamped like the sequences'.
func TestRunwayOptions_SizeInterval(t *testing.T) {
	cfg := config.DefaultConfig()
	if o := runwayOptions(cfg, "orders"); o.SizeInterval != 10*time.Minute {
		t.Fatalf("default size interval %s, want 10m", o.SizeInterval)
	}
	cfg.SRE.Runways.SizeIntervalSeconds = 1800
	if o := runwayOptions(cfg, "orders"); o.SizeInterval != 30*time.Minute {
		t.Fatalf("size interval %s, want 30m", o.SizeInterval)
	}
}
