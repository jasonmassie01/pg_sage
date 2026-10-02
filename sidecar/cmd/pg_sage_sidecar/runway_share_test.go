package main

import (
	"testing"

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
