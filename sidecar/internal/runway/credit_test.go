package runway

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Avoided-incident credit for a WAL bound (LEDGER "Avoided-incident
// credit"): only when the measured fill trend projected the declared disk
// full inside the horizon before the verified action, and the projection
// after it (the bounded WAL plus measured database growth) no longer
// reaches it.

const gb = 1e9

func creditCase() CreditInput {
	disk := probes.RunwayTrend{Kind: probes.RunwayDiskUsed, Subject: probes.SubjectCluster,
		Samples: 20, FirstAt: evalAt.Add(-2 * time.Hour), LastAt: evalAt,
		LastValue: 90 * gb, Limit: 100 * gb, RatePerS: 200e3, R2: 0.9}
	dbs := probes.RunwayTrend{Kind: probes.RunwayDatabaseBytes,
		Subject: probes.SubjectCluster, Samples: 20, FirstAt: evalAt.Add(-2 * time.Hour),
		LastAt: evalAt, LastValue: 80 * gb, RatePerS: 1e3, R2: 0.8}
	return CreditInput{Capacity: 100 * gb, BoundBytes: 10 * gb, Horizon: 72 * time.Hour,
		MinSamples: 10, MinSpan: 30 * time.Minute,
		Before: DiskMeasure{UsedBytes: 90 * gb, RetainedBytes: 9 * gb, Disk: disk,
			Databases: dbs, TrendsOK: true},
		After: DiskMeasure{UsedBytes: 88 * gb, RetainedBytes: 8 * gb, TrendsOK: true}}
}

func TestWALBoundCredit_MeasuredNearMissIsCredited(t *testing.T) {
	ok, why := WALBoundCredit(creditCase())
	if !ok {
		t.Fatalf("credit refused: %s", why)
	}
	if !strings.Contains(why, "s before") {
		t.Fatalf("reason %q must state the projection before the action", why)
	}
}

func TestWALBoundCredit_RefusesWhatWasNotMeasured(t *testing.T) {
	cases := map[string]func(*CreditInput){
		"no declared capacity": func(c *CreditInput) { c.Capacity = 0 },
		"no trend":             func(c *CreditInput) { c.Before.TrendsOK = false },
		"too few samples":      func(c *CreditInput) { c.Before.Disk.Samples = 9 },
		"too short a span": func(c *CreditInput) {
			c.Before.Disk.FirstAt = evalAt.Add(-29 * time.Minute)
		},
		"sawtooth":            func(c *CreditInput) { c.Before.Disk.R2 = 0.49 },
		"not filling":         func(c *CreditInput) { c.Before.Disk.RatePerS = 0 },
		"beyond the horizon":  func(c *CreditInput) { c.Before.Disk.RatePerS = 30e3 },
		"bound not in effect": func(c *CreditInput) { c.BoundBytes = -1 },
		"database growth unmeasured": func(c *CreditInput) {
			c.Before.Databases.Samples = 2
		},
		"still reaches capacity": func(c *CreditInput) { c.BoundBytes = 20 * gb },
		"unmeasured after":       func(c *CreditInput) { c.After.UsedBytes = 0 },
		"managed provider":       func(c *CreditInput) { c.ManagedProvider = true },
	}
	for name, mutate := range cases {
		c := creditCase()
		mutate(&c)
		if ok, why := WALBoundCredit(c); ok || why == "" {
			t.Errorf("%s: credited=%v reason %q", name, ok, why)
		}
	}
}

// Boundaries: a projection exactly at the horizon counts; falling
// database size counts as no growth, not negative growth.
func TestWALBoundCredit_Boundaries(t *testing.T) {
	c := creditCase()
	c.Before.Disk.RatePerS = (100*gb - 90*gb) / (72 * time.Hour).Seconds()
	if ok, why := WALBoundCredit(c); !ok {
		t.Fatalf("exactly at the horizon refused: %s", why)
	}
	c.Before.Disk.RatePerS *= 0.999
	if ok, _ := WALBoundCredit(c); ok {
		t.Fatal("just beyond the horizon credited")
	}
	c = creditCase()
	c.Before.Databases.RatePerS = -5e3
	c.BoundBytes = 19.9 * gb // 88 + 11.9 = 99.9 GB stays under 100 GB
	if ok, why := WALBoundCredit(c); !ok {
		t.Fatalf("shrinking databases: %s", why)
	}
	c.After.UsedBytes = 88.2 * gb
	if ok, _ := WALBoundCredit(c); ok {
		t.Fatal("88.2 + 11.9 GB reaches capacity; shrinking databases must not hide it")
	}
}
