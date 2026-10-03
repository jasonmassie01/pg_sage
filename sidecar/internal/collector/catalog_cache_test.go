package collector

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Unit tests for the pure pieces of the bounded catalog reads (perf fix
// phase, measured.md M1/M3/M4/M7). No concurrent-access tests here: these
// helpers are only called from the collector's single cycle goroutine and
// the collector serializes them under catalogMu (covered by the DB tests).

func seq(name string, pct float64) SequenceStats {
	return SequenceStats{SchemaName: "s", SequenceName: name, PctUsed: pct}
}

func TestSelectSequences_FloorTopNAndCap(t *testing.T) {
	var all []SequenceStats
	for i := 0; i < 50; i++ { // under the floor, distinct
		all = append(all, seq(fmt.Sprintf("low_%02d", i), float64(i)/100))
	}
	all = append(all, seq("hot_a", 90), seq("hot_b", 5), seq("hot_c", 1))
	got := selectSequences(all, 1.0, 10, 1000)
	if len(got) != 10 {
		t.Fatalf("kept %d, want the 3 above the floor plus 7 more of the top 10", len(got))
	}
	if got[0].SequenceName != "hot_a" || got[1].SequenceName != "hot_b" ||
		got[2].SequenceName != "hot_c" {
		t.Fatalf("order = %v, want hot_a, hot_b, hot_c first", names(got[:3]))
	}
	for i := 1; i < len(got); i++ {
		if got[i].PctUsed > got[i-1].PctUsed {
			t.Fatalf("not ordered by use at %d: %v", i, names(got))
		}
	}
	if got[3].SequenceName != "low_49" {
		t.Fatalf("first below the floor = %s, want low_49", got[3].SequenceName)
	}
	capped := selectSequences(all, 0, 0, 2) // everything >= 0 qualifies; cap wins
	if len(capped) != 2 || capped[1].SequenceName != "hot_b" {
		t.Fatalf("cap 2 = %v, want hot_a, hot_b", names(capped))
	}
}

func TestSelectSequences_TiesOrderedBySchemaThenName(t *testing.T) {
	in := []SequenceStats{
		{SchemaName: "b", SequenceName: "x", PctUsed: 2},
		{SchemaName: "a", SequenceName: "z", PctUsed: 2},
		{SchemaName: "a", SequenceName: "y", PctUsed: 2},
	}
	got := selectSequences(in, 1, 0, 10)
	want := "a.y a.z b.x"
	if s := qualified(got); s != want {
		t.Fatalf("tie order = %q, want %q", s, want)
	}
}

func TestSelectSequences_EmptyAndNil(t *testing.T) {
	if got := selectSequences(nil, 1, 100, 1000); len(got) != 0 {
		t.Fatalf("nil input = %v", got)
	}
	if got := selectSequences([]SequenceStats{seq("a", 50)}, 1, 100, 0); len(got) != 0 {
		t.Fatalf("maxRows 0 = %v, want nothing", got)
	}
}

func TestSelectSequences_DoesNotMutateInput(t *testing.T) {
	in := []SequenceStats{seq("a", 1), seq("b", 9)}
	selectSequences(in, 0, 10, 10)
	if in[0].SequenceName != "a" || in[1].SequenceName != "b" {
		t.Fatalf("input reordered: %v", names(in))
	}
}

func names(s []SequenceStats) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.SequenceName
	}
	return out
}

func qualified(s []SequenceStats) string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.SchemaName + "." + x.SequenceName
	}
	return strings.Join(out, " ")
}

func used(oid uint32, name string, pct float64) sequenceRow {
	return sequenceRow{oid: oid, readable: true, used: true, stats: seq(name, pct)}
}

func TestSequenceCache_PageReplacesItsOIDRange(t *testing.T) {
	sc := &sequenceCache{}
	sc.apply(0, []sequenceRow{used(10, "a", 1), used(20, "b", 2), used(30, "c", 3)}, false)
	sc.apply(30, []sequenceRow{used(40, "d", 4)}, true)
	if got := len(sc.list()); got != 4 {
		t.Fatalf("after first pass %d cached, want 4", got)
	}
	// Second pass: b stopped being readable, c was dropped, e is new;
	// d (the next page) must survive until its own page is re-read.
	sc.apply(0, []sequenceRow{used(10, "a", 1.5),
		{oid: 20, readable: false, stats: seq("b", 0)}, used(25, "e", 9)}, false)
	got := map[string]float64{}
	for _, s := range sc.list() {
		got[s.SequenceName] = s.PctUsed
	}
	want := map[string]float64{"a": 1.5, "e": 9, "d": 4}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("cache = %v, want %v", got, want)
	}
	// The final page clears everything after its cursor.
	sc.apply(25, nil, true)
	if got := len(sc.list()); got != 2 {
		t.Fatalf("after an empty final page %d cached, want 2 (a, e)", got)
	}
}

func TestSequenceCache_UnusedRowsAreNotCached(t *testing.T) {
	sc := &sequenceCache{}
	sc.apply(0, []sequenceRow{{oid: 5, readable: true, stats: seq("never", 0)}}, true)
	if got := sc.list(); len(got) != 0 {
		t.Fatalf("unused sequence cached: %v", got)
	}
}

func TestIndexDefCache_HitMissAndEviction(t *testing.T) {
	d := newIndexDefCache()
	now := time.Unix(1_000_000, 0)
	if flushed := d.beginPass(7, now, time.Hour); !flushed {
		t.Fatal("first pass must start empty")
	}
	if _, ok := d.lookup(1, "v1"); ok {
		t.Fatal("hit on an empty cache")
	}
	d.store(1, "v1", "CREATE INDEX a")
	d.store(2, "v1", "CREATE INDEX b")
	d.endPass()
	if d.beginPass(7, now.Add(time.Minute), time.Hour) {
		t.Fatal("unchanged watermark within max age must keep the cache")
	}
	if def, ok := d.lookup(1, "v1"); !ok || def != "CREATE INDEX a" {
		t.Fatalf("lookup(1) = %q, %v", def, ok)
	}
	if _, ok := d.lookup(1, "v2"); ok {
		t.Fatal("a changed version must miss")
	}
	d.store(1, "v2", "CREATE INDEX a2")
	d.endPass() // index 2 was not seen this pass: dropped
	if d.size() != 1 {
		t.Fatalf("cache holds %d entries after eviction, want 1", d.size())
	}
}

func TestIndexDefCache_WatermarkAndMaxAgeFlush(t *testing.T) {
	d := newIndexDefCache()
	now := time.Unix(2_000_000, 0)
	d.beginPass(1, now, time.Hour)
	d.store(1, "v", "def")
	d.endPass()
	if !d.beginPass(2, now, time.Hour) {
		t.Fatal("a catalog-update watermark change must flush (column renames)")
	}
	if _, ok := d.lookup(1, "v"); ok {
		t.Fatal("flushed cache still hits")
	}
	d.store(1, "v", "def")
	d.endPass()
	if d.beginPass(2, now.Add(59*time.Minute), time.Hour) {
		t.Fatal("flushed before max age")
	}
	d.endPass()
	if !d.beginPass(2, now.Add(61*time.Minute), time.Hour) {
		t.Fatal("max age must flush even when counters do not move")
	}
}

func TestIndexDefCache_FailedPassKeepsEntries(t *testing.T) {
	d := newIndexDefCache()
	now := time.Unix(3_000_000, 0)
	d.beginPass(1, now, time.Hour)
	d.store(1, "v", "a")
	d.store(2, "v", "b")
	d.endPass()
	d.beginPass(1, now, time.Hour)
	d.lookup(1, "v") // the pass fails before reaching index 2: no endPass
	d.beginPass(1, now, time.Hour)
	if _, ok := d.lookup(2, "v"); !ok {
		t.Fatal("an aborted pass evicted entries it never reached")
	}
}

func TestTopByBytes(t *testing.T) {
	oids := []uint32{5, 1, 9, 3}
	bytes := []int64{100, 900, 100, 0}
	if got := fmt.Sprint(topByBytes(oids, bytes, 2)); got != "[1 5]" {
		t.Fatalf("top 2 = %s, want [1 5] (largest, then lowest oid on ties)", got)
	}
	if got := topByBytes(oids, bytes, 0); len(got) != 0 {
		t.Fatalf("top 0 = %v", got)
	}
	if got := topByBytes(oids, bytes, 10); len(got) != 4 {
		t.Fatalf("top 10 of 4 = %v", got)
	}
	if got := topByBytes(nil, nil, 3); len(got) != 0 {
		t.Fatalf("top of nothing = %v", got)
	}
}

func TestDBSizeCache_Due(t *testing.T) {
	var d dbSizeCache
	now := time.Unix(4_000_000, 0)
	if !d.due(now, 15*time.Minute) {
		t.Fatal("never measured must be due")
	}
	d = dbSizeCache{bytes: 10, at: now}
	if d.due(now.Add(14*time.Minute+59*time.Second), 15*time.Minute) {
		t.Fatal("due before the interval")
	}
	if !d.due(now.Add(15*time.Minute), 15*time.Minute) {
		t.Fatal("not due at the interval boundary")
	}
	if !d.due(now, 0) {
		t.Fatal("a zero interval means every cycle")
	}
}

func TestSystemStatsJSON_UnknownDBSizeIsNull(t *testing.T) {
	j, err := json.Marshal(SystemStats{MaxConnections: 5, CacheHitRatio: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(j), `"db_size_bytes":null`) {
		t.Fatalf("unknown size marshals as %s, want null (averages ignore it)", j)
	}
	j, _ = json.Marshal(SystemStats{DBSizeBytes: 4096})
	if !strings.Contains(string(j), `"db_size_bytes":4096`) {
		t.Fatalf("known size marshals as %s", j)
	}
	var back SystemStats
	if err := json.Unmarshal([]byte(`{"db_size_bytes":null}`), &back); err != nil ||
		back.DBSizeBytes != 0 {
		t.Fatalf("null size reads back as %d (%v)", back.DBSizeBytes, err)
	}
	if err := json.Unmarshal([]byte(`{"db_size_bytes":77}`), &back); err != nil ||
		back.DBSizeBytes != 77 {
		t.Fatalf("size reads back as %d (%v)", back.DBSizeBytes, err)
	}
}

func TestCatalogSQL_NoPerRelationStorageCalls(t *testing.T) {
	for name, sql := range map[string]string{
		"tableStatsSQL": tableStatsSQL, "indexStatsSQL": indexStatsSQL,
		"systemStatsSQL14": systemStatsSQL14, "systemStatsSQL17": systemStatsSQL17,
	} {
		for _, fn := range []string{"pg_total_relation_size(", "pg_table_size(",
			"pg_indexes_size(", "pg_relation_size(", "pg_get_indexdef(",
			"pg_database_size("} {
			if strings.Contains(sql, fn) {
				t.Errorf("%s calls %s on the hot path", name, fn)
			}
		}
	}
	if strings.Contains(tableStatsSQL, "pg_stat_user_tables") {
		t.Error("tableStatsSQL reads the grouped pg_stat_user_tables view per page")
	}
	if !strings.Contains(tableStatsSQL, "c.oid > $1") ||
		!strings.Contains(tableStatsSQL, "ORDER BY c.oid") {
		t.Error("tableStatsSQL is not a pg_class oid keyset")
	}
}

func TestSequencePageSQL_BoundedByLockBudget(t *testing.T) {
	for _, want := range []string{"max_locks_per_transaction", "max_prepared_transactions",
		"LIMIT LEAST($2", "q.seqrelid > $1", "ORDER BY q.seqrelid",
		"has_sequence_privilege"} {
		if !strings.Contains(sequencePageSQL, want) {
			t.Errorf("sequencePageSQL lacks %q", want)
		}
	}
	if strings.Contains(sequencePageSQL, "pg_sequences") {
		t.Error("sequencePageSQL reads pg_sequences (locks every sequence)")
	}
}
