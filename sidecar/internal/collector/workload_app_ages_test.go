package collector

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// review-02e: the application age read is bounded, tagged, scoped to this
// database's client sessions and leaves pg_sage's own out.
func TestWorkloadAppAgesSQL_IsBoundedAndExcludesSelf(t *testing.T) {
	for _, want := range []string{sageTag, notSelf, "'client backend'", "current_database()",
		"backend_start", "/ 60", "ORDER BY 3 DESC", "LIMIT 501"} {
		if !strings.Contains(WorkloadAppAgesSQL, want) {
			t.Errorf("app ages SQL lacks %q", want)
		}
	}
}

func appAgeRows(n int) [][]any {
	rows := make([][]any, n)
	for i := range rows {
		rows[i] = []any{"api", int64(i), 1}
	}
	return rows
}

// Boundary: WorkloadAppAgeCap rows are kept whole; one more marks the read
// truncated and is dropped.
func TestScanWorkloadAppAges_CapBoundary(t *testing.T) {
	for _, tc := range []struct {
		n, keep   int
		truncated bool
	}{{0, 0, false}, {WorkloadAppAgeCap, WorkloadAppAgeCap, false},
		{WorkloadAppAgeCap + 1, WorkloadAppAgeCap, true}} {
		ages, truncated, err := ScanWorkloadAppAges(&fakeRows{rows: appAgeRows(tc.n)})
		if err != nil || ages == nil || len(ages) != tc.keep || truncated != tc.truncated {
			t.Errorf("%d rows: %d kept (nil %v), truncated %v, err %v; want %d, %v", tc.n,
				len(ages), ages == nil, truncated, err, tc.keep, tc.truncated)
		}
	}
}

// A NULL minute (backend_start not visible) stays nil, never minute 0; a
// negative minute is kept as read for the generator to count.
func TestScanWorkloadAppAges_NullAndNegativeMinutes(t *testing.T) {
	ages, _, err := ScanWorkloadAppAges(&fakeRows{rows: [][]any{
		{"api", nil, 4}, {"api", int64(-1), 2}, {"web", int64(7), 3}}})
	if err != nil || len(ages) != 3 {
		t.Fatalf("ages %+v, err %v", ages, err)
	}
	if ages[0].Minute != nil || ages[0].Sessions != 4 {
		t.Errorf("NULL minute read as %+v", ages[0])
	}
	if ages[1].Minute == nil || *ages[1].Minute != -1 {
		t.Errorf("negative minute read as %+v", ages[1])
	}
	if ages[2].Application != "web" || *ages[2].Minute != 7 || ages[2].Sessions != 3 {
		t.Errorf("third row %+v", ages[2])
	}
}

// A scan or iteration error is the read's error, with no partial list.
func TestScanWorkloadAppAges_ErrorsPropagate(t *testing.T) {
	boom := errors.New("connection reset")
	ages, _, err := ScanWorkloadAppAges(&fakeRows{rows: appAgeRows(2), err: boom})
	if !errors.Is(err, boom) || ages != nil {
		t.Fatalf("iteration error: ages %v err %v", ages, err)
	}
	ages, _, err = ScanWorkloadAppAges(&fakeRows{rows: [][]any{{"api", "x", 1}}})
	if err == nil || ages != nil {
		t.Fatalf("bad minute type: ages %v err %v, want a scan error", ages, err)
	}
}

// A snapshot written before review-02e decodes with AppAges nil (unknown).
func TestWorkloadSessions_LegacyJSONHasNoAppAges(t *testing.T) {
	var ws WorkloadSessions
	if err := json.Unmarshal([]byte(`{"groups":[],"long":[],"roles":[]}`), &ws); err != nil {
		t.Fatal(err)
	}
	if ws.AppAges != nil || ws.AppAgesTruncated {
		t.Fatalf("legacy app ages %v truncated %v, want nil and false", ws.AppAges,
			ws.AppAgesTruncated)
	}
}

// No concurrent access tests: ScanWorkloadAppAges is stateless and reads
// only the rows it is given.
