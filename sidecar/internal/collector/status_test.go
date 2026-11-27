package collector

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// collection-08: Status before any cycle is a valid, empty report.
func TestStatusBeforeFirstCycle(t *testing.T) {
	cfg := testConfig()
	cfg.HasWALColumns, cfg.HasPlanTimeColumns = true, false
	c := New(nil, cfg, 170004, noopLog)
	st := c.Status()
	if st.BreakerState != BreakerClosed || st.BreakerReason != "" {
		t.Errorf("state/reason = %q/%q, want closed/\"\"", st.BreakerState, st.BreakerReason)
	}
	if !st.LastSuccess.IsZero() || !st.LastErrorAt.IsZero() || st.LastErrorText != "" {
		t.Errorf("times/error = %v/%v/%q, want zero", st.LastSuccess, st.LastErrorAt,
			st.LastErrorText)
	}
	if st.Unavailable == nil || len(st.Unavailable) != 0 {
		t.Errorf("Unavailable = %#v, want an empty non-nil map", st.Unavailable)
	}
	if st.SkipsTotal == nil || st.SkipsTotal[SkipReasonLoad] != 0 ||
		st.SkipsTotal[SkipReasonLoadQueryError] != 0 {
		t.Errorf("SkipsTotal = %#v, want both reasons at zero", st.SkipsTotal)
	}
	if st.PersistFailures != 0 || st.QueryStoreFailures != 0 || st.ConsecutiveSkips != 0 {
		t.Errorf("counters = %d/%d/%d, want zero", st.PersistFailures,
			st.QueryStoreFailures, st.ConsecutiveSkips)
	}
	if st.LoadCeiling != 0.90 {
		t.Errorf("LoadCeiling = %v, want 0.90 from cpu_ceiling_pct 90", st.LoadCeiling)
	}
	caps := st.Capabilities
	if caps.PGVersionNum != 170004 || !caps.HasWALColumns || caps.HasPlanTimeColumns {
		t.Errorf("capabilities = %+v, want the startup seed (170004, WAL only)", caps)
	}
}

func TestStatusJSONNamesFields(t *testing.T) {
	c := New(nil, testConfig(), 160000, noopLog)
	raw, err := json.Marshal(c.Status())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"breaker_state":"closed"`, `"unavailable":{}`,
		`"persist_failures":0`, `"query_store_failures":0`, `"skips_total":{`,
		`"capabilities":{"pg_version_num":160000`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("status JSON %s lacks %s", raw, key)
		}
	}
}

func TestStatusUnavailableIsACopy(t *testing.T) {
	c := New(nil, testConfig(), 170000, noopLog)
	snap := &Snapshot{CollectedAt: time.Now(), Unavailable: map[string]string{
		"indexes": "canceling statement due to statement timeout"}}
	c.mu.Lock()
	c.latest = snap
	c.mu.Unlock()
	st := c.Status()
	if st.Unavailable["indexes"] == "" || len(st.Unavailable) != 1 {
		t.Fatalf("Unavailable = %v, want the latest snapshot's", st.Unavailable)
	}
	st.Unavailable["tables"] = "mutated"
	delete(st.Unavailable, "indexes")
	if len(snap.Unavailable) != 1 || snap.Unavailable["indexes"] == "" {
		t.Errorf("mutating Status().Unavailable changed the snapshot: %v", snap.Unavailable)
	}
}

func TestStatusCountsPersistAndQueryStoreFailures(t *testing.T) {
	c := New(nil, testConfig(), 170000, noopLog)
	c.notePersistFailure(errors.New("permission denied for table snapshots"))
	c.notePersistFailure(errors.New("permission denied for table snapshots"))
	c.noteQueryStoreFailure(errors.New("relation sage.query_store does not exist"))
	st := c.Status()
	if st.PersistFailures != 2 || st.QueryStoreFailures != 1 {
		t.Errorf("persist/query_store = %d/%d, want 2/1", st.PersistFailures,
			st.QueryStoreFailures)
	}
	if !strings.HasPrefix(st.LastErrorText, "query_store: ") ||
		!strings.Contains(st.LastErrorText, "does not exist") {
		t.Errorf("LastErrorText = %q, want the latest (query_store) error", st.LastErrorText)
	}
	if st.LastErrorAt.IsZero() {
		t.Error("LastErrorAt is zero after failures")
	}
}

func TestStatusNoteErrorNilIsIgnored(t *testing.T) {
	c := New(nil, testConfig(), 170000, noopLog)
	c.noteError("persist", nil)
	if st := c.Status(); !st.LastErrorAt.IsZero() || st.LastErrorText != "" {
		t.Errorf("a nil error was recorded: %+v", st)
	}
}

func TestBreakerStateCodes(t *testing.T) {
	cases := map[BreakerState]int{BreakerClosed: 0, BreakerSkipping: 1,
		BreakerDormant: 2, BreakerState("bogus"): -1, BreakerState(""): -1}
	for state, want := range cases {
		if got := state.Code(); got != want {
			t.Errorf("%q.Code() = %d, want %d", state, got, want)
		}
	}
}
