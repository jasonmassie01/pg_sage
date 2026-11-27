package probes

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// sre-03: the values a claim may quote are the typed row values, each as
// one exact token: never the head line, the row count or the [i] ordinals.
// No concurrency test: GroundingValues reads only its receiver.

func TestResult_GroundingValuesAreTheRowValues(t *testing.T) {
	ts := time.Date(2026, 11, 5, 4, 12, 45, 500, time.FixedZone("x", 3600))
	res := Result{ProbeID: PlanRegressions, Version: "v2", Status: StatusOK,
		Columns: []string{"queryid", "mean_ms", "calls", "since", "relation", "waiting", "gone"},
		Rows: []Row{{"queryid": json.Number("-8123456789012345678"), "mean_ms": 75.3125,
			"calls": int64(1200), "since": ts, "relation": "public.orders_2024",
			"waiting": true, "gone": nil}}}
	got := strings.Join(res.GroundingValues(10), "|")
	want := "-8123456789012345678|75.31|1200|2026-11-05T03:12:45Z|public.orders_2024"
	if got != want {
		t.Fatalf("GroundingValues = %q, want %q", got, want)
	}
}

func TestResult_GroundingValuesBoundRowsLikeText(t *testing.T) {
	res := Result{ProbeID: LongTransactions, Version: "v1", Status: StatusOK,
		Columns: []string{"pid"}}
	for i := 0; i < 5; i++ {
		res.Rows = append(res.Rows, Row{"pid": int64(100 + i)})
	}
	if got := strings.Join(res.GroundingValues(2), ","); got != "100,101" {
		t.Fatalf("GroundingValues(2) = %q, want the 2 rows Text shows", got)
	}
	if got := len(res.GroundingValues(0)); got != 0 {
		t.Fatalf("GroundingValues(0) has %d values, want none", got)
	}
}

func TestResult_GroundingValuesOfAFailureAreItsReason(t *testing.T) {
	res := Result{ProbeID: LockGraph, Version: "v1", Status: StatusError,
		Reason: "canceled after 5000 ms", Error: "SQLSTATE 57014"}
	got := strings.Join(res.GroundingValues(10), "|")
	if got != "canceled after 5000 ms|SQLSTATE 57014" {
		t.Fatalf("GroundingValues = %q", got)
	}
}

func TestResult_GroundingValuesOfNothingIsEmpty(t *testing.T) {
	if got := (Result{}).GroundingValues(10); len(got) != 0 {
		t.Fatalf("zero result values = %q, want none", got)
	}
	empty := Result{ProbeID: LockGraph, Version: "v1", Status: StatusEmpty}
	if got := empty.GroundingValues(10); len(got) != 0 {
		t.Fatalf("empty result values = %q, want none", got)
	}
	nan := Result{Status: StatusOK, Columns: []string{"x"}, Rows: []Row{{"x": math.NaN()}}}
	if got := nan.GroundingValues(10); len(got) != 0 {
		t.Fatalf("NaN values = %q, want none (unknown is not a number)", got)
	}
}
