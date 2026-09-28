package probes

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// Probe results are rendered as bounded, deterministic text for the
// narration catalog. Numbers are formatted by one function so a claim
// that quotes a value matches the evidence exactly.

func TestFormatValue(t *testing.T) {
	ts := time.Date(2026, 9, 27, 10, 11, 12, 999, time.FixedZone("x", 3600))
	cases := []struct {
		in   any
		want string
	}{
		{nil, "null"},
		{int64(42), "42"},
		{int32(-3), "-3"},
		{75.0, "75"},
		{75.3125, "75.31"},
		{0.004, "0"},
		{math.NaN(), "unknown"},
		{true, "true"},
		{"idle in transaction", "idle in transaction"},
		{strings.Repeat("a", 100), strings.Repeat("a", maxTextRunes) + "..."},
		{ts, "2026-09-27T09:11:12Z"},
		{json.Number("12"), "12"},
	}
	for _, c := range cases {
		if got := FormatValue(c.in); got != c.want {
			t.Errorf("FormatValue(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResult_TextListsRowsInColumnOrder(t *testing.T) {
	res := Result{ProbeID: LockGraph, Version: "v1", Status: StatusOK,
		Columns: []string{"waiter_pid", "blocker_pid", "relation"},
		Rows: []Row{
			{"waiter_pid": int64(20), "blocker_pid": int64(10), "relation": "public.t"},
			{"waiter_pid": int64(30), "blocker_pid": int64(20), "relation": nil},
		}}
	got := res.Text(10)
	want := "lock_graph v1 ok, 2 rows\n" +
		"[1] waiter_pid=20 blocker_pid=10 relation=public.t\n" +
		"[2] waiter_pid=30 blocker_pid=20 relation=null"
	if got != want {
		t.Fatalf("Text =\n%s\nwant\n%s", got, want)
	}
}

func TestResult_TextBoundsRowsAndFlagsTruncation(t *testing.T) {
	res := Result{ProbeID: LongTransactions, Version: "v1", Status: StatusOK,
		Columns: []string{"pid"}, Truncated: true}
	for i := 0; i < 5; i++ {
		res.Rows = append(res.Rows, Row{"pid": int64(i + 1)})
	}
	got := res.Text(2)
	if !strings.Contains(got, "[2] pid=2") || strings.Contains(got, "[3]") {
		t.Fatalf("Text(2) did not stop at 2 rows:\n%s", got)
	}
	if !strings.Contains(got, "3 more rows not shown") ||
		!strings.Contains(got, "truncated at the probe cap") {
		t.Fatalf("Text(2) hides omitted rows or truncation:\n%s", got)
	}
}

func TestResult_TextNamesUnavailableStatus(t *testing.T) {
	for _, st := range []Status{StatusNoPrivilege, StatusUnsupported, StatusError} {
		res := Result{ProbeID: PlanRegressions, Version: "v1", Status: st,
			Reason: "some_reason"}
		got := res.Text(5)
		if got != "plan_regressions v1 "+string(st)+": some_reason" {
			t.Errorf("Text = %q", got)
		}
	}
	empty := Result{ProbeID: ReplicationLag, Version: "v1", Status: StatusEmpty}
	if got := empty.Text(5); got != "replication_lag v1 empty, 0 rows" {
		t.Errorf("empty Text = %q", got)
	}
}

func TestResult_PayloadIsBoundedJSON(t *testing.T) {
	res := Result{ProbeID: LockGraph, Version: "v1", Status: StatusOK,
		Columns: []string{"a"}, Rows: []Row{{"a": int64(1)}},
		ObservedAt: time.Now()}
	raw, err := res.Payload()
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if back["probe_id"] != "lock_graph" || back["status"] != "ok" {
		t.Fatalf("payload = %s", raw)
	}
}
