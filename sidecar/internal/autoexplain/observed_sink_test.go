package autoexplain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

// rca-04: the executed plans auto_explain writes to the server log are
// ingested by one shared sink (the Supabase log sink and the self-hosted
// log watcher). Every record that looks like a plan but is not stored is
// counted under a reason; a store failure is logged with its queryid and
// does not stop later plans. No concurrent-access test: a sink value keeps
// no state between Ingest calls (callers own their counters).

const sinkDB = "app"

func planEntry(queryID int64, app string) logwatch.LogEntry {
	return logwatch.LogEntry{Database: sinkDB, Timestamp: time.Unix(1700000000, 0).UTC(),
		Application: app, Message: fmt.Sprintf("duration: 12.500 ms  plan:\n"+
			`{"Query Text":"SELECT * FROM t WHERE id = 1","Query Identifier":%d,`+
			`"Plan":{"Node Type":"Index Scan","Total Cost":8.3,"Actual Rows":1}}`, queryID)}
}

type fakeStore struct {
	stored []ObservedPlan
	fail   map[int64]error
}

func (f *fakeStore) store(_ context.Context, p ObservedPlan) error {
	if err := f.fail[p.QueryID]; err != nil {
		return err
	}
	f.stored = append(f.stored, p)
	return nil
}

type capturedLog struct{ lines []string }

func (c *capturedLog) logFn(level, format string, args ...any) {
	c.lines = append(c.lines, level+" "+fmt.Sprintf(format, args...))
}

func newSink(st *fakeStore, logs *capturedLog) ObservedPlanSink {
	return ObservedPlanSink{Database: sinkDB, Store: st.store, MaxPerCycle: 100,
		LogFn: logs.logFn}
}

func TestObservedPlanSink_StoresAnExecutedPlan(t *testing.T) {
	st, logs := &fakeStore{}, &capturedLog{}
	res, err := newSink(st, logs).Ingest(context.Background(),
		[]logwatch.LogEntry{planEntry(42, "app")})
	if err != nil || res.Stored != 1 || len(res.Rejected) != 0 {
		t.Fatalf("Ingest = %+v, %v; want one stored, none rejected", res, err)
	}
	p := st.stored[0]
	if p.QueryID != 42 || p.ExecutionMS != 12.5 || p.TotalCost != 8.3 ||
		!p.CapturedAt.Equal(time.Unix(1700000000, 0)) ||
		!strings.Contains(string(p.JSON), `"Actual Rows":1`) {
		t.Fatalf("stored plan = %+v", p)
	}
}

func TestObservedPlanSink_RejectsWithACountedReason(t *testing.T) {
	truncated := planEntry(1, "app")
	truncated.Message = truncated.Message[:len(truncated.Message)-20]
	noID := planEntry(2, "app")
	noID.Message = "duration: 1 ms  plan:\n" +
		`{"Query Text":"SELECT 1","Plan":{"Total Cost":1}}`
	self := planEntry(3, "pg_sage")
	other := planEntry(4, "app")
	other.Database = "other_db"
	noTime := planEntry(5, "app")
	noTime.Timestamp = time.Time{}
	statement := planEntry(6, "app")
	statement.Message = "duration: 3 ms  statement: SELECT 'plan:'"
	st := &fakeStore{}
	res, err := newSink(st, &capturedLog{}).Ingest(context.Background(),
		[]logwatch.LogEntry{truncated, noID, self, other, noTime, statement,
			planEntry(7, "app")})
	if err != nil || res.Stored != 1 || len(st.stored) != 1 || st.stored[0].QueryID != 7 {
		t.Fatalf("Ingest = %+v, %v; want only queryid 7 stored", res, err)
	}
	want := map[string]int{RejectInvalidPlan: 2, RejectMissingQueryID: 1,
		RejectSelf: 1, RejectOtherDatabase: 1}
	if len(res.Rejected) != len(want) {
		t.Fatalf("rejected = %v, want %v (a plain duration line is not a plan)",
			res.Rejected, want)
	}
	for reason, n := range want {
		if res.Rejected[reason] != n {
			t.Fatalf("rejected = %v, want %v", res.Rejected, want)
		}
	}
}

func TestObservedPlanSink_StoreFailureIsLoggedAndLaterPlansStillIngest(t *testing.T) {
	st := &fakeStore{fail: map[int64]error{11: errors.New("connection refused")}}
	logs := &capturedLog{}
	res, err := newSink(st, logs).Ingest(context.Background(),
		[]logwatch.LogEntry{planEntry(11, "app"), planEntry(12, "app")})
	if err != nil {
		t.Fatalf("a store failure must not fail the cycle: %v", err)
	}
	if res.Stored != 1 || len(st.stored) != 1 || st.stored[0].QueryID != 12 ||
		res.Rejected[RejectStoreFailed] != 1 {
		t.Fatalf("Ingest = %+v; want 12 stored and one store failure", res)
	}
	joined := strings.Join(logs.lines, "\n")
	if !strings.Contains(joined, "queryid=11") || !strings.Contains(joined, "connection refused") ||
		!strings.HasPrefix(logs.lines[0], "WARN ") {
		t.Fatalf("store failure log = %q, want a WARN naming queryid 11 and the cause", joined)
	}
}

// The Supabase sink keeps its retry contract: the first store failure
// ends the batch with an error that wraps the cause.
func TestObservedPlanSink_StopOnStoreErrorReturnsTheCause(t *testing.T) {
	cause := errors.New("metadata store down")
	st := &fakeStore{fail: map[int64]error{21: cause}}
	sink := newSink(st, &capturedLog{})
	sink.StopOnStoreError = true
	res, err := sink.Ingest(context.Background(),
		[]logwatch.LogEntry{planEntry(21, "app"), planEntry(22, "app")})
	if !errors.Is(err, cause) || res.Stored != 0 || len(st.stored) != 0 {
		t.Fatalf("Ingest = %+v, %v; want the cause and nothing stored after it", res, err)
	}
}

// Boundary: the per-cycle cap stores exactly MaxPerCycle plans and counts
// the rest; zero means no cap.
func TestObservedPlanSink_PerCycleCap(t *testing.T) {
	entries := make([]logwatch.LogEntry, 0, 101)
	for i := range 101 {
		entries = append(entries, planEntry(int64(1000+i), "app"))
	}
	st := &fakeStore{}
	res, err := newSink(st, &capturedLog{}).Ingest(context.Background(), entries)
	if err != nil || res.Stored != 100 || res.Rejected[RejectOverCycleCap] != 1 {
		t.Fatalf("101 plans with cap 100 = %+v, %v", res, err)
	}
	uncapped := newSink(&fakeStore{}, &capturedLog{})
	uncapped.MaxPerCycle = 0
	if res, err := uncapped.Ingest(context.Background(), entries); err != nil ||
		res.Stored != 101 {
		t.Fatalf("uncapped = %+v, %v; want all 101", res, err)
	}
}

// Nil/empty and invalid configuration.
func TestObservedPlanSink_EmptyInputAndInvalidSink(t *testing.T) {
	res, err := newSink(&fakeStore{}, &capturedLog{}).Ingest(context.Background(), nil)
	if err != nil || res.Stored != 0 || len(res.Rejected) != 0 {
		t.Fatalf("empty batch = %+v, %v", res, err)
	}
	for name, sink := range map[string]ObservedPlanSink{
		"no database": {Store: (&fakeStore{}).store},
		"no store":    {Database: sinkDB},
	} {
		if _, err := sink.Ingest(context.Background(),
			[]logwatch.LogEntry{planEntry(1, "app")}); err == nil {
			t.Fatalf("%s: Ingest accepted an unusable sink", name)
		}
	}
}

// Error propagation: a cancelled context stops the cycle before storing.
func TestObservedPlanSink_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := &fakeStore{}
	_, err := newSink(st, &capturedLog{}).Ingest(ctx, []logwatch.LogEntry{planEntry(1, "app")})
	if !errors.Is(err, context.Canceled) || len(st.stored) != 0 {
		t.Fatalf("cancelled Ingest = %v, stored %d", err, len(st.stored))
	}
}

func TestIsPlanMessage(t *testing.T) {
	for msg, want := range map[string]bool{
		"duration: 12.500 ms  plan:\n{}":            true,
		"duration: 1 ms plan: {}":                   true,
		`duration: 1 ms  plan:\n{"Query Text"`:      true,
		"duration: 3 ms  statement: SELECT 1":       false,
		"duration: 3 ms  statement: SELECT 'plan:'": false,
		"":                            false,
		"LOG:  duration: 1 ms  plan:": false,
	} {
		if got := IsPlanMessage(msg); got != want {
			t.Errorf("IsPlanMessage(%q) = %v, want %v", msg, got, want)
		}
	}
}
