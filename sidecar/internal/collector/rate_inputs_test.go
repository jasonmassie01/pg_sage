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
