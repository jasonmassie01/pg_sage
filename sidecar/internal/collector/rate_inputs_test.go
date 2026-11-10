package collector

import (
	"encoding/json"
	"testing"
	"time"
)

// rca-03: the system interval carries the sessions opened in it
// (pg_stat_database.sessions), under the database epoch like the other
// pg_stat_database counters.

// No concurrent access test here: computeSystemInterval is pure; the
// published-snapshot race is TestSystemInterval_ConcurrentLatest's.

func TestSystemInterval_SessionsDelta(t *testing.T) {
	prev, next := ivBase(), ivNext()
	prev.Sessions, next.Sessions = 1_000, 1_450
	iv := computeSystemInterval(ivSnap(ivStart, prev), ivSnap(ivStart.Add(time.Minute), next))
	if iv == nil {
		t.Fatal("interval = nil")
	}
	i64(t, "Sessions", iv.Sessions, 450)
}

// Zero: no session opened is a measured zero, not unknown.
func TestSystemInterval_SessionsZero(t *testing.T) {
	prev, next := ivBase(), ivNext()
	prev.Sessions, next.Sessions = 1_000, 1_000
	iv := computeSystemInterval(ivSnap(ivStart, prev), ivSnap(ivStart.Add(time.Minute), next))
	if iv == nil {
		t.Fatal("interval = nil")
	}
	i64(t, "Sessions", iv.Sessions, 0)
}

// State transition: a moved database epoch (pg_stat_reset) or a sessions
// counter that fell under the same epoch leaves the sessions unknown.
func TestSystemInterval_SessionsResetUnknown(t *testing.T) {
	prev, next := ivBase(), ivNext()
	prev.Sessions, next.Sessions = 1_000, 1_450
	moved := ivSnap(ivStart.Add(time.Minute), next)
	moved.System.RelationStatsEpoch = ivDBEpoch.Add(time.Hour)
	if iv := computeSystemInterval(ivSnap(ivStart, prev), moved); iv == nil ||
		iv.Sessions != nil {
		t.Errorf("moved epoch: interval %+v, want nil Sessions", iv)
	}
	next.Sessions = 10
	iv := computeSystemInterval(ivSnap(ivStart, prev), ivSnap(ivStart.Add(time.Minute), next))
	if iv == nil || iv.Sessions != nil || iv.CacheHitRatio != nil {
		t.Errorf("sessions fell: interval %+v, want the database fields nil", iv)
	}
}

// A previous snapshot from before sessions was read (0: the collector's
// own connection is always a session) gives unknown sessions, not the
// database's lifetime count as one interval's.
func TestSystemInterval_SessionsLegacyPreviousUnknown(t *testing.T) {
	prev, next := ivBase(), ivNext()
	next.Sessions = 52_000
	iv := computeSystemInterval(ivSnap(ivStart, prev), ivSnap(ivStart.Add(time.Minute), next))
	if iv == nil || iv.Sessions != nil {
		t.Fatalf("legacy previous: interval %+v, want nil Sessions", iv)
	}
	if iv.CacheHitRatio == nil {
		t.Error("legacy previous dropped the other database fields")
	}
}

// The new system fields survive a JSON round trip (snapshots are stored
// and reloaded), and a legacy row without them decodes to zero values.
func TestSystemStats_RateInputsJSON(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	in := SystemStats{IdleInTxMaxSeconds: 3600.5, Sessions: 77, WALLSN: 0x16B3748,
		WALReadAt: at}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out SystemStats
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.IdleInTxMaxSeconds != 3600.5 || out.Sessions != 77 || out.WALLSN != 0x16B3748 ||
		!out.WALReadAt.Equal(at) {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
	var legacy SystemStats
	if err := json.Unmarshal([]byte(`{"active_backends":3}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.WALLSN != 0 || !legacy.WALReadAt.IsZero() || legacy.Sessions != 0 {
		t.Errorf("legacy row = %+v, want zero rate inputs", legacy)
	}
}

// walPair is two primary snapshots a minute apart whose WAL positions were
// read 59.5 s apart, prev at 0x1000000 and next n bytes later.
func walPair(n int64) (prev, next *Snapshot) {
	p, c := ivBase(), ivNext()
	p.WALLSN, p.WALReadAt = 0x1000000, ivStart.Add(250*time.Millisecond)
	c.WALLSN, c.WALReadAt = 0x1000000+n, ivStart.Add(59750*time.Millisecond)
	return ivSnap(ivStart, p), ivSnap(ivStart.Add(time.Minute), c)
}

// rca-03: the interval carries the WAL insert position's advance and the
// time between the two reads of it, which the WAL rate divides by.
func TestSystemInterval_WALAdvance(t *testing.T) {
	prev, next := walPair(5 << 20)
	iv := computeSystemInterval(prev, next)
	if iv == nil {
		t.Fatal("interval = nil")
	}
	i64(t, "WALBytes", iv.WALBytes, 5<<20)
	if iv.WALSeconds != 59.5 {
		t.Errorf("WALSeconds = %v, want 59.5 (between the reads, not the snapshots)",
			iv.WALSeconds)
	}
}

// Zero: an idle minute is a measured zero, not unknown.
func TestSystemInterval_WALZeroAdvance(t *testing.T) {
	iv := computeSystemInterval(walPair(0))
	if iv == nil {
		t.Fatal("interval = nil")
	}
	i64(t, "WALBytes", iv.WALBytes, 0)
}

// State transition: pg_stat_reset() moves the database epoch but not the
// WAL position, so the WAL advance is still measured across it.
func TestSystemInterval_WALAcrossStatsReset(t *testing.T) {
	prev, next := walPair(3 << 20)
	next.System.RelationStatsEpoch = ivDBEpoch.Add(time.Hour)
	next.System.BlksHit = 10
	iv := computeSystemInterval(prev, next)
	if iv == nil || iv.CacheHitRatio != nil {
		t.Fatalf("interval %+v, want the database fields unknown", iv)
	}
	i64(t, "WALBytes", iv.WALBytes, 3<<20)
}

// The WAL advance is unknown on a standby (either side), without a read
// position on either side, after the position went back (a failover to a
// server that was behind) and when the reads are not in time order.
func TestSystemInterval_WALUnknown(t *testing.T) {
	for name, mutate := range map[string]func(p, c *SystemStats){
		"prev standby":   func(p, _ *SystemStats) { p.IsReplica = true },
		"next standby":   func(_, c *SystemStats) { c.IsReplica = true },
		"prev unread":    func(p, _ *SystemStats) { p.WALReadAt = time.Time{} },
		"next unread":    func(_, c *SystemStats) { c.WALReadAt = time.Time{} },
		"regression":     func(p, c *SystemStats) { c.WALLSN = p.WALLSN - 1 },
		"same read time": func(p, c *SystemStats) { c.WALReadAt = p.WALReadAt },
		"reads reversed": func(p, c *SystemStats) {
			c.WALReadAt = p.WALReadAt.Add(-time.Second)
		},
	} {
		prev, next := walPair(5 << 20)
		mutate(&prev.System, &next.System)
		iv := computeSystemInterval(prev, next)
		if iv == nil {
			t.Fatalf("%s: interval = nil, want the other fields measured", name)
		}
		if iv.WALBytes != nil || iv.WALSeconds != 0 {
			t.Errorf("%s: WAL %v over %v s, want unknown", name, iv.WALBytes, iv.WALSeconds)
		}
		if iv.CacheHitRatio == nil {
			t.Errorf("%s: an unknown WAL advance dropped the database fields", name)
		}
	}
}
