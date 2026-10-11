package agentposture

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

const gb = int64(1) << 30

var t0 = time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)

func obs(at time.Time, schema string, bytes, deletes int64) Observation {
	return Observation{At: at, Values: map[string]ObservedValue{
		schema: {Bytes: bytes, Deletes: deletes, Tables: []string{schema + ".checkpoints"}}}}
}

func TestObservationStore_NilSafe(t *testing.T) {
	var s *ObservationStore
	if _, ok := s.Previous("AP-12"); ok {
		t.Fatal("nil store has no observation")
	}
	s.Record("AP-12", obs(t0, "lg", 1, 0)) // must not panic
}

func TestObservationStore_RecordReturnsCopies(t *testing.T) {
	s := NewObservationStore()
	if _, ok := s.Previous("AP-12"); ok {
		t.Fatal("empty store has an observation")
	}
	o := obs(t0, "lg", 5*gb, 2)
	s.Record("AP-12", o)
	o.Values["lg"] = ObservedValue{Bytes: 1}
	got, ok := s.Previous("AP-12")
	if !ok || got.Values["lg"].Bytes != 5*gb || !got.At.Equal(t0) {
		t.Fatalf("Previous = %+v, %v; the store must keep its own copy", got, ok)
	}
	got.Values["lg"] = ObservedValue{}
	again, _ := s.Previous("AP-12")
	if again.Values["lg"].Bytes != 5*gb {
		t.Fatal("Previous must return a copy")
	}
	if _, ok := s.Previous("AP-13"); ok {
		t.Fatal("observations are per detector")
	}
}

func TestObservationStore_ConcurrentUse(t *testing.T) {
	s := NewObservationStore()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("AP-%02d", i%4)
			s.Record(id, obs(t0.Add(time.Duration(i)*time.Minute), "lg", int64(i), 0))
			_, _ = s.Previous(id)
		}(i)
	}
	wg.Wait()
	for i := 0; i < 4; i++ {
		if _, ok := s.Previous(fmt.Sprintf("AP-%02d", i)); !ok {
			t.Fatalf("AP-%02d lost its observation", i)
		}
	}
}

func TestMemoryGrowth_FirstObservationOnlyRecords(t *testing.T) {
	fs, record := memoryGrowth(Observation{}, false, obs(t0, "lg", 50*gb, 0), 5)
	if len(fs) != 0 || !record {
		t.Fatalf("first observation: findings %+v, record %v; want none and record", fs, record)
	}
}

func TestMemoryGrowth_Boundaries(t *testing.T) {
	cases := []struct {
		name    string
		elapsed time.Duration
		grow    int64
		deletes int64
		find    bool
		record  bool
	}{
		{"6 GB in a day, no deletes", 24 * time.Hour, 6 * gb, 0, true, true},
		{"exactly 5 GB a day is not more than 5", 24 * time.Hour, 5 * gb, 0, false, true},
		{"just over 5 GB a day", 24 * time.Hour, 5*gb + gb/100, 0, true, true},
		{"6 GB in a day with deletes", 24 * time.Hour, 6 * gb, 3, false, true},
		{"3 GB in 12 h is 6 GB a day", 12 * time.Hour, 3 * gb, 0, true, true},
		{"shrinking", 24 * time.Hour, -2 * gb, 0, false, true},
		{"too soon keeps the baseline", 30 * time.Minute, 6 * gb, 0, false, false},
		{"one hour is enough", time.Hour, gb, 0, true, true},
	}
	for _, c := range cases {
		prev := obs(t0, "lg", 10*gb, 7)
		cur := obs(t0.Add(c.elapsed), "lg", 10*gb+c.grow, 7+c.deletes)
		fs, record := memoryGrowth(prev, true, cur, 5)
		if record != c.record {
			t.Errorf("%s: record %v, want %v", c.name, record, c.record)
		}
		f := findObject(fs, "lg")
		if (f != nil) != c.find {
			t.Errorf("%s: finding %+v, want %v", c.name, fs, c.find)
			continue
		}
		if f != nil && (f.Severity != Info || f.ObjectType != "schema" || f.FixScript == "" ||
			len(f.Evidence) != 2 || f.Caveat == "") {
			t.Errorf("%s: finding %+v", c.name, *f)
		}
	}
}

// A statistics reset (deletes going down) or a schema seen only once is
// not judged; a clock that went backwards re-baselines.
func TestMemoryGrowth_UnjudgedCases(t *testing.T) {
	prev := obs(t0, "lg", 10*gb, 7)
	fs, record := memoryGrowth(prev, true, obs(t0.Add(24*time.Hour), "lg", 30*gb, 0), 5)
	if len(fs) != 0 || !record {
		t.Fatalf("stats reset: %+v %v", fs, record)
	}
	fs, record = memoryGrowth(prev, true, obs(t0.Add(24*time.Hour), "other", 30*gb, 0), 5)
	if len(fs) != 0 || !record {
		t.Fatalf("new schema: %+v %v", fs, record)
	}
	fs, record = memoryGrowth(prev, true, obs(t0.Add(-time.Hour), "lg", 30*gb, 7), 5)
	if len(fs) != 0 || !record {
		t.Fatalf("clock went backwards: %+v %v", fs, record)
	}
}
