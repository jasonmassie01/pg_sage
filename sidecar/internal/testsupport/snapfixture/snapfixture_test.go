package snapfixture_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/testsupport/snapfixture"
)

var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func scenario() snapfixture.Scenario {
	return snapfixture.Scenario{Start: start, Step: time.Minute, Cycles: 12, Tables: 4,
		Indexes: 40, Seed: 7}
}

func generate(t *testing.T, s snapfixture.Scenario) []snapfixture.Cycle {
	t.Helper()
	cycles, err := s.Generate()
	if err != nil {
		t.Fatalf("generate %+v: %v", s, err)
	}
	if len(cycles) != s.Cycles {
		t.Fatalf("cycles = %d, want %d", len(cycles), s.Cycles)
	}
	return cycles
}

// doc returns cycle n's (1-based) document for category.
func doc(t *testing.T, cycles []snapfixture.Cycle, n int, category string) ([]byte, bool) {
	t.Helper()
	for _, d := range cycles[n-1].Docs {
		if d.Category == category {
			return d.Data, true
		}
	}
	return nil, false
}

func decode[T any](t *testing.T, cycles []snapfixture.Cycle, n int, category string) []T {
	t.Helper()
	data, ok := doc(t, cycles, n, category)
	if !ok {
		t.Fatalf("cycle %d has no %s document", n, category)
	}
	var out []T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode cycle %d %s: %v", n, category, err)
	}
	return out
}

func indexesAt(t *testing.T, cycles []snapfixture.Cycle, n int) []collector.IndexStats {
	t.Helper()
	return decode[collector.IndexStats](t, cycles, n, "indexes")
}

func names(ixs []collector.IndexStats) []string {
	out := make([]string, 0, len(ixs))
	for _, ix := range ixs {
		out = append(out, ix.IndexRelName)
	}
	return out
}

func position(ixs []collector.IndexStats, name string) int {
	for k, ix := range ixs {
		if ix.IndexRelName == name {
			return k
		}
	}
	return -1
}

func TestGenerateIsDeterministicForOneSeed(t *testing.T) {
	s := scenario()
	s.Events = snapfixture.Events{CounterReset: 5, DropIndex: 3, QueryChurn: 2}
	first, second := generate(t, s), generate(t, s)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("two runs of one scenario differ")
	}
	s.Seed = 8
	other := generate(t, s)
	a, _ := doc(t, first, 12, "queries")
	b, _ := doc(t, other, 12, "queries")
	if string(a) == string(b) {
		t.Error("a different seed produced the same queries")
	}
}

// Concurrency: Generate builds a fresh state and random source per call, so
// parallel calls agree with a serial one.
func TestGenerateInParallelMatchesSerial(t *testing.T) {
	s := scenario()
	want := generate(t, s)
	results := make([][]snapfixture.Cycle, 8)
	var wg sync.WaitGroup
	for k := range results {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			results[k], _ = s.Generate()
		}(k)
	}
	wg.Wait()
	for k, got := range results {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parallel run %d differs from the serial run", k)
		}
	}
}

func TestGenerateMatchesTheScenarioShape(t *testing.T) {
	s := scenario()
	cycles := generate(t, s)
	wantCats := []string{"config_data", "foreign_keys", "indexes", "locks", "partitions",
		"queries", "sequences", "system", "tables"}
	for k, c := range cycles {
		if want := start.Add(time.Duration(k) * time.Minute); !c.At.Equal(want) {
			t.Errorf("cycle %d at %v, want %v", k+1, c.At, want)
		}
		var cats []string
		for _, d := range c.Docs {
			cats = append(cats, d.Category)
		}
		if !reflect.DeepEqual(cats, wantCats) {
			t.Fatalf("cycle %d categories = %v", k+1, cats)
		}
	}
	tables := decode[collector.TableStats](t, cycles, 1, "tables")
	if len(tables) != 4 || tables[0].RelName != "t000" || tables[3].RelName != "t003" {
		t.Errorf("tables = %d, first %+v", len(tables), tables[0])
	}
	ixs := indexesAt(t, cycles, 1)
	if len(ixs) != s.Tables+s.Indexes {
		t.Fatalf("indexes = %d, want %d", len(ixs), s.Tables+s.Indexes)
	}
	if !ixs[0].IsPrimary || ixs[0].IndexRelName != "t000_pkey" ||
		ixs[s.Tables].IndexRelName != "ix_t000_c00000" || ixs[s.Tables].IsPrimary {
		t.Errorf("index order: %v", names(ixs[:s.Tables+1]))
	}
	if got := len(decode[collector.QueryStats](t, cycles, 1, "queries")); got != 60 {
		t.Errorf("queries = %d, want 60", got)
	}
	if data, _ := doc(t, cycles, 1, "locks"); string(data) != "null" {
		t.Errorf("locks = %s, want null", data)
	}
}

// Invalid input: a scenario without cycles, a step or tables is an error,
// not a panic.
func TestGenerateRejectsAnEmptyScenario(t *testing.T) {
	cases := map[string]func(*snapfixture.Scenario){
		"zero cycles":     func(s *snapfixture.Scenario) { s.Cycles = 0 },
		"negative cycles": func(s *snapfixture.Scenario) { s.Cycles = -1 },
		"zero step":       func(s *snapfixture.Scenario) { s.Step = 0 },
		"negative step":   func(s *snapfixture.Scenario) { s.Step = -time.Second },
		"zero tables":     func(s *snapfixture.Scenario) { s.Tables = 0 },
		"zero value":      func(s *snapfixture.Scenario) { *s = snapfixture.Scenario{} },
	}
	for name, mutate := range cases {
		s := scenario()
		mutate(&s)
		cycles, err := s.Generate()
		if err == nil || cycles != nil || !strings.Contains(err.Error(), "scenario needs") {
			t.Errorf("%s: cycles %d err %v, want the scenario error", name, len(cycles), err)
		}
	}
}

// Boundary and empty: one cycle, one table, no secondary indexes.
func TestGenerateSmallestScenario(t *testing.T) {
	s := snapfixture.Scenario{Start: start, Step: time.Second, Cycles: 1, Tables: 1}
	cycles := generate(t, s)
	if !cycles[0].At.Equal(start) {
		t.Errorf("only cycle at %v, want %v", cycles[0].At, start)
	}
	if got := names(indexesAt(t, cycles, 1)); !reflect.DeepEqual(got, []string{"t000_pkey"}) {
		t.Errorf("indexes = %v, want the primary key only", got)
	}
	if data, _ := doc(t, cycles, 1, "foreign_keys"); string(data) != "null" {
		t.Errorf("one table has no foreign keys: %s", data)
	}
}

// Cold indexes (secondary names ending in 5) are never scanned, so the
// unused-index evidence has subjects; the rest are.
func TestColdIndexesAreNeverScanned(t *testing.T) {
	cycles := generate(t, scenario())
	var warm int64
	for n := 1; n <= len(cycles); n++ {
		for _, ix := range indexesAt(t, cycles, n) {
			cold := strings.HasPrefix(ix.IndexRelName, "ix_") &&
				strings.HasSuffix(ix.IndexRelName, "5")
			if cold && ix.IdxScan != 0 {
				t.Errorf("cycle %d: cold %s scanned %d times", n, ix.IndexRelName, ix.IdxScan)
			}
			if !cold {
				warm += ix.IdxScan
			}
		}
	}
	if warm == 0 {
		t.Error("no warm index was ever scanned")
	}
}
