package main

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/migration/rehearsal"
)

// market-03 review: Lakebase deletes a rehearsal branch ttl_minutes after
// its create starts, so a rehearsal (bounded from before that create) must
// end by then, or the server drops the branch mid-rehearsal and a healthy
// migration reads as a failed one.
func TestRehearsalMaxDurationEndsBeforeTheBranchExpires(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.CloneProviderConfig
		want time.Duration
	}{
		{"dle keeps the default bound", config.CloneProviderConfig{Provider: "dle",
			Lakebase: config.CloneLakebaseConfig{TTLMinutes: 5}},
			rehearsal.DefaultMaxDuration},
		{"lakebase ttl 1 minute", lakebaseTTL(1), time.Minute},
		{"lakebase ttl 59 minutes", lakebaseTTL(59), 59 * time.Minute},
		{"lakebase ttl equal to the bound", lakebaseTTL(60), rehearsal.DefaultMaxDuration},
		{"lakebase ttl above the bound", lakebaseTTL(1440), rehearsal.DefaultMaxDuration},
	}
	for _, tc := range cases {
		if got := rehearsalMaxDuration(tc.cfg); got != tc.want {
			t.Errorf("%s: rehearsal bound = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// A ttl that validation would refuse never yields a zero or negative bound
// (zero means "default" to the orchestrator, which would outlive the branch).
func TestRehearsalMaxDurationNeverZeroForLakebase(t *testing.T) {
	for _, ttl := range []int{0, -5} {
		if got := rehearsalMaxDuration(lakebaseTTL(ttl)); got != time.Minute {
			t.Errorf("ttl %d: rehearsal bound = %s, want the 1m floor", ttl, got)
		}
	}
}

func lakebaseTTL(minutes int) config.CloneProviderConfig {
	c := lakebaseCloneCfg()
	c.Lakebase.TTLMinutes = minutes
	return c
}
