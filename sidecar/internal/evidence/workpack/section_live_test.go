package workpack

import (
	"encoding/json"
	"strings"
	"testing"
)

// review-02b: the autovacuum section states each table's live rows from
// the window's last tables snapshot, so a generator can size a per-table
// scale factor; a snapshot without n_live_tup is unknown, never zero.
func TestBuildAutovacuum_LiveTuples(t *testing.T) {
	live := int64(2_000_000)
	zero := int64(0)
	win := relWindow{status: relStatusOK, tabFirst: map[string]tableCounters{},
		tabLast: map[string]tableCounters{
			"public.known": {live: &live},
			"public.empty": {live: &zero},
			"public.blind": {},
		}}
	got := buildAutovacuum(win, []string{"2026-10-30"}, nil)
	by := map[string]AutovacuumFact{}
	for _, f := range got.Tables {
		by[f.Table] = f
	}
	if by["public.known"].LiveTuples != live {
		t.Fatalf("known: live %v, want %d", by["public.known"].LiveTuples, live)
	}
	if by["public.empty"].LiveTuples != zero {
		t.Fatalf("an empty table: live %v, want 0 (measured as nothing)",
			by["public.empty"].LiveTuples)
	}
	if by["public.blind"].LiveTuples != valueUnknown {
		t.Fatalf("no n_live_tup: live %v, want unknown", by["public.blind"].LiveTuples)
	}
	raw, err := json.Marshal(by["public.blind"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"live_tuples":"unknown"`) {
		t.Fatalf("encoded fact %s lacks live_tuples unknown", raw)
	}
}

// The tables read selects n_live_tup last, after the reloptions it scans.
func TestTableCountersSQL_ReadsLiveTuples(t *testing.T) {
	i := strings.Index(tableCountersSQL, "n_live_tup")
	if i < 0 || i < strings.Index(tableCountersSQL, "reloptions") {
		t.Fatalf("tableCountersSQL does not read n_live_tup after reloptions:\n%s",
			tableCountersSQL)
	}
}
