package runway

import (
	"math"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Samples are built from typed probe results only: a known number is a
// sample, an unknown one is not (never zero), and each series carries
// its counter (when it has one) and its limit.

func fullSnapshot() Snapshot {
	return Snapshot{
		XID: &probes.XIDRunway{NextXID: 5e6, MXIDCounter: 40, ClusterXIDAge: 3e5,
			ClusterMXIDAge: 20},
		WAL: &probes.WALRunway{PositionBytes: 9e9, MaxSlotWALKeepSize: -1,
			DatabaseBytes: 2e9, UnreadableDatabases: 0},
		SizeFresh: true,
		Dir:       &probes.WALDirectory{Bytes: 64 << 20},
		Slots: []probes.Slot{{Name: "cdc", RetainedBytes: 1 << 30},
			{Name: "lost", RetainedBytes: math.NaN()}},
		SlotsOK: true,
		Sequences: []probes.SequenceRunway{
			{Sequence: "public.a_seq", LastValue: 900, Limit: 1000},
			{Sequence: "public.cyc_seq", LastValue: 900, Limit: 1000, Cycle: true},
			{Sequence: "public.new_seq", LastValue: math.NaN(), Limit: 1000}},
		SequencesOK: true, SequencesFresh: true,
	}
}

func byKey(ss []Sample) map[string]Sample {
	out := map[string]Sample{}
	for _, s := range ss {
		out[s.Kind+"/"+s.Subject] = s
	}
	return out
}

func TestBuildSamples_EverySeries(t *testing.T) {
	opts := Options{DiskCapacityBytes: 1e11, WALRetainedLimitBytes: 10 << 30}
	got := byKey(BuildSamples(fullSnapshot(), opts))
	check := func(key string, value, counter, limit float64) {
		t.Helper()
		s, ok := got[key]
		if !ok {
			t.Fatalf("no %s sample in %v", key, got)
		}
		same := func(a, b float64) bool { return a == b || (math.IsNaN(a) && math.IsNaN(b)) }
		if !same(s.Value, value) || !same(s.Counter, counter) || !same(s.Limit, limit) {
			t.Fatalf("%s = %+v, want value %v counter %v limit %v", key, s, value,
				counter, limit)
		}
	}
	nan := math.NaN()
	check("xid/cluster", 3e5, 5e6, WraparoundWarnAge)
	check("mxid/cluster", 20, 40, WraparoundWarnAge)
	check("wal_position/cluster", 9e9, 9e9, nan)
	check("database_bytes/cluster", 2e9, nan, nan)
	check("disk_used/cluster", 2e9+64<<20, nan, 1e11)
	check("wal_slot/cdc", 1<<30, nan, 10<<30)
	check("sequence/public.a_seq", 900, 900, 1000)
	if len(got) != 7 {
		t.Fatalf("samples = %v, want 7 (unknown slot, cycling and never-called "+
			"sequences skipped)", got)
	}
}

// A bounded max_slot_wal_keep_size is a slot's limit; an undeclared disk
// capacity leaves disk usage without one.
func TestBuildSamples_LimitsFollowTheSettings(t *testing.T) {
	s := fullSnapshot()
	s.WAL.MaxSlotWALKeepSize = 4 << 30
	got := byKey(BuildSamples(s, Options{WALRetainedLimitBytes: 10 << 30}))
	if got["wal_slot/cdc"].Limit != 4<<30 {
		t.Fatalf("slot limit = %v, want the bounded setting", got["wal_slot/cdc"].Limit)
	}
	if !math.IsNaN(got["disk_used/cluster"].Limit) {
		t.Fatalf("disk limit = %v, want unknown without a declared capacity",
			got["disk_used/cluster"].Limit)
	}
}

// Unknown or unreadable inputs produce no sample, never a zero.
func TestBuildSamples_UnknownInputsAreSkipped(t *testing.T) {
	s := fullSnapshot()
	s.WAL.UnreadableDatabases = 1
	s.Dir = nil
	s.XID.NextXID = math.NaN()
	got := byKey(BuildSamples(s, Options{}))
	for _, key := range []string{"database_bytes/cluster", "disk_used/cluster",
		"xid/cluster"} {
		if _, ok := got[key]; ok {
			t.Errorf("%s sampled from unknown input", key)
		}
	}
	if _, ok := got["mxid/cluster"]; !ok {
		t.Error("an unknown XID counter dropped the multixact series too")
	}
	if n := len(BuildSamples(Snapshot{}, Options{})); n != 0 {
		t.Fatalf("empty snapshot built %d samples", n)
	}
}

// Epochs: a counter that falls (a reset, a restarted sequence) starts a
// new epoch; otherwise the series keeps its epoch.
func TestAssignEpochs(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	last := map[seriesKey]lastPoint{
		{"sequence", "public.a_seq"}:  {epoch: "e-old", counter: 1000},
		{"xid", "cluster"}:            {epoch: "x-1", counter: 10},
		{"database_bytes", "cluster"}: {epoch: "d-1", counter: math.NaN()},
	}
	ss := []Sample{
		{Kind: "sequence", Subject: "public.a_seq", Value: 5, Counter: 5},
		{Kind: "xid", Subject: "cluster", Value: 1, Counter: 12},
		{Kind: "database_bytes", Subject: "cluster", Value: 1, Counter: math.NaN()},
		{Kind: "wal_slot", Subject: "new", Value: 1, Counter: math.NaN()},
	}
	out := assignEpochs(ss, last, now)
	want := []string{epochAt(now), "x-1", "d-1", epochAt(now)}
	for i, s := range out {
		if s.Epoch != want[i] {
			t.Errorf("%s/%s epoch = %q, want %q", s.Kind, s.Subject, s.Epoch, want[i])
		}
	}
	if last[seriesKey{"sequence", "public.a_seq"}].epoch != epochAt(now) ||
		last[seriesKey{"xid", "cluster"}].counter != 12 {
		t.Fatalf("last points not advanced: %+v", last)
	}
}
