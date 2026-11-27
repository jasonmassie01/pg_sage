package probes

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// forecast-02: runway_trends returns series in (kind, subject) order
// under a 200-row cap. RunwayTrendsOf reports whether the cap cut the
// read and the last series returned, so the monitor knows which kinds it
// read completely.
//
// No concurrency test: the decoder is a pure function of its Result.

func trendRows(n int, kinds ...string) []Row {
	at := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	var rows []Row
	for i := 0; len(rows) < n; i++ {
		rows = append(rows, Row{"kind": kinds[i%len(kinds)], "subject": fmt.Sprintf(
			"s%03d", i), "samples": int64(12), "first_at": at, "last_at": at.Add(time.Hour),
			"last_value": 1.0, "last_limit": nil, "rate_per_s": 1.0, "r2": 1.0})
	}
	return rows
}

func TestRunwayTrendsOf_TruncatedReportsTheCut(t *testing.T) {
	rows := append(trendRows(1, RunwaySequence), trendRows(199, RunwayWALPosition)...)
	res := Result{ProbeID: RunwayTrendsProbe, Version: "v2", Status: StatusOK, Rows: rows,
		Truncated: true, ObservedAt: time.Now()}
	set, err := RunwayTrendsOf(res)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(set.Trends) != 200 || !set.Truncated || set.LastKind != RunwayWALPosition ||
		set.LastSubject != "s198" {
		t.Fatalf("set = %d trends, truncated %v, last %s/%s", len(set.Trends),
			set.Truncated, set.LastKind, set.LastSubject)
	}
	for kind, want := range map[string]bool{RunwayDatabaseBytes: true, RunwayDiskUsed: true,
		RunwayMXID: true, RunwaySequence: true, RunwayWALPosition: false,
		RunwayWALSlot: false, RunwayXID: false} {
		if got := set.Complete(kind); got != want {
			t.Errorf("Complete(%s) = %v, want %v", kind, got, want)
		}
	}
}

func TestRunwayTrendsOf_UntruncatedIsComplete(t *testing.T) {
	set, err := RunwayTrendsOf(okResult(RunwayTrendsProbe, nil, trendRows(3, RunwayXID)...))
	if err != nil || len(set.Trends) != 3 || set.Truncated || set.LastKind != RunwayXID {
		t.Fatalf("set = %+v (%v)", set, err)
	}
	for _, kind := range []string{RunwayDatabaseBytes, RunwayWALSlot, RunwayXID} {
		if !set.Complete(kind) {
			t.Errorf("an untruncated read is incomplete for %s", kind)
		}
	}
}

// Nil/empty and boundary: an empty read is complete; a read truncated
// before its first row (the byte cap) is complete for nothing.
func TestRunwayTrendsOf_TruncatedEmptyIsCompleteForNothing(t *testing.T) {
	empty, err := RunwayTrendsOf(okResult(RunwayTrendsProbe, nil))
	if err != nil || len(empty.Trends) != 0 || !empty.Complete(RunwayXID) {
		t.Fatalf("empty read = %+v (%v), want complete", empty, err)
	}
	cut := Result{ProbeID: RunwayTrendsProbe, Status: StatusEmpty, Truncated: true}
	set, err := RunwayTrendsOf(cut)
	if err != nil || !set.Truncated || set.LastKind != "" {
		t.Fatalf("cut read = %+v (%v)", set, err)
	}
	for _, kind := range []string{RunwayDatabaseBytes, RunwayMXID, RunwayXID} {
		if set.Complete(kind) {
			t.Errorf("a read cut before its first row is complete for %s", kind)
		}
	}
}

// Error propagation: an unavailable or malformed read is an error, never
// an empty complete set.
func TestRunwayTrendsOf_TruncatedErrorsPropagate(t *testing.T) {
	var ue *UnavailableError
	if _, err := RunwayTrendsOf(Result{ProbeID: RunwayTrendsProbe, Status: StatusError,
		Reason: "statement_timeout", Truncated: true}); !errors.As(err, &ue) {
		t.Fatalf("failed read = %v, want UnavailableError", err)
	}
	if _, err := RunwayTrendsOf(okResult(RunwayTrendsProbe, nil,
		Row{"kind": "xid", "samples": int64(2)})); err == nil {
		t.Fatal("a trend without a subject decoded")
	}
}
