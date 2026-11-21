package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
)

// ux-07a: the ledger asks after trust.ask_after_approvals person
// approvals; without a loaded config it keeps the default, and 0 (off)
// reaches the ledger as 0.

func TestAutonomyServiceConfigCarriesAskAfterApprovals(t *testing.T) {
	saved := cfg
	t.Cleanup(func() { cfg = saved })
	cfg = nil
	s := config.DefaultConfig().SRE.Autonomy
	if got := autonomyServiceConfig(s).AskAfterApprovals; got !=
		earned.DefaultAskAfterApprovals {
		t.Fatalf("no config: %d, want %d", got, earned.DefaultAskAfterApprovals)
	}
	for _, n := range []int{0, 1, 25} {
		c := config.DefaultConfig()
		c.Trust.AskAfterApprovals = n
		cfg = c
		if got := autonomyServiceConfig(s).AskAfterApprovals; got != n {
			t.Fatalf("ask_after_approvals %d reached the ledger as %d", n, got)
		}
	}
}
