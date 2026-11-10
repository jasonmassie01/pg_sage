package collector

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// collection-06: the system counters are lifetime totals; the interval
// between two snapshots is what an operator reads as "now".

var (
	ivDBEpoch   = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	ivCkptEpoch = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	ivStart     = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
)

func ivSnap(at time.Time, s SystemStats) *Snapshot {
	if s.RelationStatsEpoch.IsZero() {
		s.RelationStatsEpoch = ivDBEpoch
	}
	if s.CheckpointStatsEpoch.IsZero() {
		s.CheckpointStatsEpoch = ivCkptEpoch
	}
	return &Snapshot{CollectedAt: at, System: s}
}

func ivBase() SystemStats {
	return SystemStats{
		BlksHit: 1_000_000, BlksRead: 5_000, Deadlocks: 10,
		XactCommit: 50_000, XactRollback: 100, TempBytes: 1 << 20,
		BlkReadTime: 1_000, BlkWriteTime: 200, TotalCheckpoints: 40,
		CacheHitRatio: 0.995,
	}
}

func ivNext() SystemStats {
	s := ivBase()
	s.BlksHit += 8_000
	s.BlksRead += 2_000
	s.Deadlocks += 3
	s.XactCommit += 900
	s.XactRollback += 10
	s.TempBytes += 4096
	s.BlkReadTime += 250.5
	s.BlkWriteTime += 49.5
	s.TotalCheckpoints += 2
	return s
}

func i64(t *testing.T, name string, p *int64, want int64) {
	t.Helper()
	if p == nil {
		t.Fatalf("%s = nil, want %d", name, want)
	}
	if *p != want {
		t.Errorf("%s = %d, want %d", name, *p, want)
	}
}

func f64(t *testing.T, name string, p *float64, want float64) {
	t.Helper()
	if p == nil {
		t.Fatalf("%s = nil, want %v", name, want)
	}
	if *p != want {
		t.Errorf("%s = %v, want %v", name, *p, want)
	}
}

// assertDBFieldsNil checks every per-database interval field is unknown.
func assertDBFieldsNil(t *testing.T, iv *SystemInterval) {
	t.Helper()
	if iv.CacheHitRatio != nil || iv.BlksAccessed != nil || iv.Deadlocks != nil ||
		iv.Xacts != nil || iv.TempBytes != nil || iv.BlkReadTimeMs != nil ||
		iv.BlkWriteTimeMs != nil {
		t.Fatalf("database interval fields not nil: %+v", *iv)
	}
}

// CHECK-28: +8,000 hits and +2,000 reads over 60 s is exactly 0.8.
func TestSystemInterval_ExactRatioAndDeltas(t *testing.T) {
	prev := ivSnap(ivStart, ivBase())
	cur := ivSnap(ivStart.Add(60*time.Second), ivNext())
	iv := computeSystemInterval(prev, cur)
	if iv == nil {
		t.Fatal("interval = nil, want measured")
	}
	if iv.Seconds != 60 {
		t.Errorf("Seconds = %v, want 60", iv.Seconds)
	}
	f64(t, "CacheHitRatio", iv.CacheHitRatio, 0.8)
	i64(t, "BlksAccessed", iv.BlksAccessed, 10_000)
	i64(t, "Deadlocks", iv.Deadlocks, 3)
	i64(t, "Xacts", iv.Xacts, 910)
	i64(t, "TempBytes", iv.TempBytes, 4096)
	f64(t, "BlkReadTimeMs", iv.BlkReadTimeMs, 250.5)
	f64(t, "BlkWriteTimeMs", iv.BlkWriteTimeMs, 49.5)
	i64(t, "Checkpoints", iv.Checkpoints, 2)
}

// Nil/zero: no previous snapshot, nothing to difference against.
func TestSystemInterval_NoPrevious(t *testing.T) {
	if iv := computeSystemInterval(nil, ivSnap(ivStart, ivNext())); iv != nil {
		t.Fatalf("interval = %+v, want nil without a previous snapshot", *iv)
	}
	if iv := computeSystemInterval(ivSnap(ivStart, ivBase()), nil); iv != nil {
		t.Fatalf("interval = %+v, want nil without a current snapshot", *iv)
	}
}

// Nil/zero: no block accesses in the interval is an unknown ratio, not 0.
func TestSystemInterval_ZeroBlocksNilRatio(t *testing.T) {
	prev := ivSnap(ivStart, ivBase())
	cur := ivSnap(ivStart.Add(30*time.Second), ivBase())
	iv := computeSystemInterval(prev, cur)
	if iv == nil {
		t.Fatal("interval = nil, want measured with zero deltas")
	}
	if iv.CacheHitRatio != nil {
		t.Errorf("CacheHitRatio = %v, want nil with no block accesses", *iv.CacheHitRatio)
	}
	i64(t, "BlksAccessed", iv.BlksAccessed, 0)
	i64(t, "Deadlocks", iv.Deadlocks, 0)
	i64(t, "Checkpoints", iv.Checkpoints, 0)
}

// Boundary: a moved relation stats epoch (pg_stat_reset) nils every
// database field, never a negative delta; checkpoints are cluster-wide
// and survive it.
func TestSystemInterval_DatabaseEpochMoved(t *testing.T) {
	prev := ivSnap(ivStart, ivBase())
	reset := SystemStats{BlksHit: 10, BlksRead: 1, TotalCheckpoints: 42,
		RelationStatsEpoch: ivDBEpoch.Add(time.Hour)}
	cur := ivSnap(ivStart.Add(60*time.Second), reset)
	iv := computeSystemInterval(prev, cur)
	if iv == nil {
		t.Fatal("interval = nil, want one with nil database fields")
	}
	assertDBFieldsNil(t, iv)
	i64(t, "Checkpoints", iv.Checkpoints, 2)
}

// Boundary: only the checkpoint epoch moving nils just Checkpoints.
func TestSystemInterval_CheckpointEpochMoved(t *testing.T) {
	prev := ivSnap(ivStart, ivBase())
	next := ivNext()
	next.TotalCheckpoints = 1
	next.CheckpointStatsEpoch = ivCkptEpoch.Add(time.Hour)
	iv := computeSystemInterval(prev, ivSnap(ivStart.Add(time.Minute), next))
	if iv == nil {
		t.Fatal("interval = nil")
	}
	if iv.Checkpoints != nil {
		t.Errorf("Checkpoints = %d, want nil across a checkpoint reset", *iv.Checkpoints)
	}
	f64(t, "CacheHitRatio", iv.CacheHitRatio, 0.8)
	i64(t, "Deadlocks", iv.Deadlocks, 3)
}

// Boundary: an unknown (zero) epoch on either side is not a match.
func TestSystemInterval_UnknownEpochIsNotAMatch(t *testing.T) {
	prev := ivSnap(ivStart, ivBase())
	prev.System.RelationStatsEpoch = time.Time{}
	prev.System.CheckpointStatsEpoch = time.Time{}
	iv := computeSystemInterval(prev, ivSnap(ivStart.Add(time.Minute), ivNext()))
	if iv == nil {
		t.Fatal("interval = nil")
	}
	assertDBFieldsNil(t, iv)
	if iv.Checkpoints != nil {
		t.Errorf("Checkpoints = %d, want nil with an unknown epoch", *iv.Checkpoints)
	}
}

// Invalid input: a counter decrease under the same epoch (a reset the
// epoch did not record) gives nil, never a negative value.
func TestSystemInterval_CounterDecreaseSameEpoch(t *testing.T) {
	decreases := map[string]func(*SystemStats){
		"blks_hit":       func(s *SystemStats) { s.BlksHit = 1 },
		"blks_read":      func(s *SystemStats) { s.BlksRead = 1 },
		"deadlocks":      func(s *SystemStats) { s.Deadlocks = 0 },
		"xact_commit":    func(s *SystemStats) { s.XactCommit = 1 },
		"xact_rollback":  func(s *SystemStats) { s.XactRollback = 0 },
		"temp_bytes":     func(s *SystemStats) { s.TempBytes = 0 },
		"blk_read_time":  func(s *SystemStats) { s.BlkReadTime = 1 },
		"blk_write_time": func(s *SystemStats) { s.BlkWriteTime = 1 },
	}
	for name, mutate := range decreases {
		t.Run(name, func(t *testing.T) {
			next := ivNext()
			mutate(&next)
			iv := computeSystemInterval(ivSnap(ivStart, ivBase()),
				ivSnap(ivStart.Add(time.Minute), next))
			if iv == nil {
				t.Fatal("interval = nil, want one with nil database fields")
			}
			assertDBFieldsNil(t, iv)
			i64(t, "Checkpoints", iv.Checkpoints, 2)
		})
	}
	next := ivNext()
	next.TotalCheckpoints = 3
	iv := computeSystemInterval(ivSnap(ivStart, ivBase()), ivSnap(ivStart.Add(time.Minute), next))
	if iv == nil || iv.Checkpoints != nil {
		t.Fatalf("checkpoint decrease: interval %+v, want nil Checkpoints", iv)
	}
}

// Invalid input: a current snapshot not after the previous has no interval.
func TestSystemInterval_NonPositiveElapsed(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		iv := computeSystemInterval(ivSnap(ivStart, ivBase()), ivSnap(ivStart.Add(d), ivNext()))
		if iv != nil {
			t.Errorf("elapsed %s: interval %+v, want nil", d, *iv)
		}
	}
}

// State transition: reset -> nil database fields -> measured again.
func TestSystemInterval_ResetThenMeasured(t *testing.T) {
	c := New(nil, testConfig(), 170000, noopLog)
	first := ivSnap(ivStart, ivBase())
	c.markSystemInterval(first)
	if first.System.Interval != nil {
		t.Fatalf("first cycle interval = %+v, want nil", *first.System.Interval)
	}
	c.latest = first
	reset := SystemStats{BlksHit: 100, BlksRead: 10, TotalCheckpoints: 40,
		RelationStatsEpoch: ivDBEpoch.Add(time.Hour)}
	second := ivSnap(ivStart.Add(time.Minute), reset)
	c.markSystemInterval(second)
	if second.System.Interval == nil {
		t.Fatal("second interval = nil")
	}
	assertDBFieldsNil(t, second.System.Interval)
	c.latest = second
	after := reset
	after.BlksHit, after.BlksRead = 900, 210
	third := ivSnap(ivStart.Add(2*time.Minute), after)
	c.markSystemInterval(third)
	if third.System.Interval == nil {
		t.Fatal("third interval = nil")
	}
	f64(t, "CacheHitRatio", third.System.Interval.CacheHitRatio, 0.8)
	i64(t, "BlksAccessed", third.System.Interval.BlksAccessed, 1000)
}

// CHECK-30: a system row stored before the upgrade still decodes, with
// no interval and zero counters.
func TestSystemInterval_LegacyRowDecodes(t *testing.T) {
	legacy := `{"active_backends":3,"idle_in_transaction":0,"total_backends":9,
		"max_connections":100,"cache_hit_ratio":0.991,"deadlocks":4,
		"blk_read_time":12.5,"blk_write_time":0,"total_checkpoints":77,
		"is_replica":false,"db_size_bytes":12345,"stat_statements_max":5000}`
	var s SystemStats
	if err := json.Unmarshal([]byte(legacy), &s); err != nil {
		t.Fatalf("decode legacy row: %v", err)
	}
	if s.Interval != nil {
		t.Fatalf("legacy Interval = %+v, want nil", *s.Interval)
	}
	if s.CacheHitRatio != 0.991 || s.Deadlocks != 4 || s.TotalCheckpoints != 77 {
		t.Fatalf("legacy fields lost: %+v", s)
	}
	if s.BlksHit != 0 || s.BlksRead != 0 || !s.CheckpointStatsEpoch.IsZero() {
		t.Fatalf("legacy counters = %d/%d epoch %s, want zero", s.BlksHit, s.BlksRead,
			s.CheckpointStatsEpoch)
	}
}

// The interval round-trips through the snapshot JSON, unknowns as null,
// and the top-level cache_hit_ratio keeps its lifetime meaning.
func TestSystemInterval_JSONRoundTrip(t *testing.T) {
	cur := ivSnap(ivStart.Add(time.Minute), ivNext())
	cur.System.Interval = computeSystemInterval(ivSnap(ivStart, ivBase()), cur)
	raw, err := json.Marshal(cur.System)
	if err != nil {
		t.Fatal(err)
	}
	var back SystemStats
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if back.CacheHitRatio != 0.995 {
		t.Errorf("lifetime cache_hit_ratio = %v, want 0.995", back.CacheHitRatio)
	}
	if back.Interval == nil {
		t.Fatalf("interval lost in %s", raw)
	}
	f64(t, "CacheHitRatio", back.Interval.CacheHitRatio, 0.8)
	i64(t, "Deadlocks", back.Interval.Deadlocks, 3)
	if back.BlksHit != cur.System.BlksHit || !back.CheckpointStatsEpoch.Equal(ivCkptEpoch) {
		t.Errorf("counters lost: %d epoch %s", back.BlksHit, back.CheckpointStatsEpoch)
	}
	unknown, _ := json.Marshal(SystemInterval{Seconds: 5})
	want := `{"seconds":5,"cache_hit_ratio":null,"blks_accessed":null,"deadlocks":null,` +
		`"xacts":null,"temp_bytes":null,"blk_read_time_ms":null,"blk_write_time_ms":null,` +
		`"checkpoints":null}`
	if string(unknown) != want {
		t.Errorf("unknown interval JSON = %s, want %s", unknown, want)
	}
}

// Concurrent access: markSystemInterval reads latest under c.mu while a
// reader (the API) takes LatestSnapshot and a writer swaps it, as cycle
// does. Meaningful under -race (unavailable on this machine without cgo);
// without it the test still checks no interval is ever negative.
func TestSystemInterval_ConcurrentLatest(t *testing.T) {
	c := New(nil, testConfig(), 170000, noopLog)
	c.latest = ivSnap(ivStart, ivBase())
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				snap := ivSnap(ivStart.Add(time.Duration(i*1000+j+1)*time.Second), ivNext())
				c.markSystemInterval(snap)
				if iv := snap.System.Interval; iv != nil && iv.BlksAccessed != nil &&
					*iv.BlksAccessed < 0 {
					t.Errorf("negative blocks %d", *iv.BlksAccessed)
				}
				c.mu.Lock()
				c.latest = snap
				c.mu.Unlock()
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if s := c.LatestSnapshot(); s == nil {
					t.Error("latest snapshot nil")
				}
			}
		}()
	}
	wg.Wait()
}
