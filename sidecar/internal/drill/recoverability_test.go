package drill

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// drill-01d unit tests: how one database's drill history becomes its
// recoverability, and how the fleet's databases add up to the spec's
// number. No concurrent access tests: Classify and Summarize are pure
// functions over values; the API test runs concurrent requests.

var recNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func passedAt(finished time.Time, rto float64) *Result {
	return &Result{Database: "orders", Provider: "neon_branch", Kind: clone.KindBranch,
		RequestedAt: finished.Add(-time.Duration(rto) * time.Second),
		FinishedAt:  finished, RTOSeconds: &rto, Passed: true}
}

func failedAt(finished time.Time, why string) *Result {
	return &Result{Database: "orders", Provider: "neon_branch", Kind: clone.KindBranch,
		RequestedAt: finished.Add(-time.Minute), FinishedAt: finished, Failure: why}
}

func TestClassify_PassExactly35DaysOldCounts(t *testing.T) {
	pass := passedAt(recNow.Add(-RecoverabilityWindow), 660)
	got := Classify("orders", pass, pass, "", recNow)
	if got.Status != RecoverPassed || got.Database != "orders" || got.LastPass != pass ||
		got.Latest != pass {
		t.Fatalf("a pass exactly 35 days old = %+v, want passed with both drills", got)
	}
}

func TestClassify_Pass35DaysAndOneSecondOldIsStale(t *testing.T) {
	pass := passedAt(recNow.Add(-RecoverabilityWindow-time.Second), 660)
	got := Classify("orders", pass, pass, "", recNow)
	if got.Status != RecoverStale {
		t.Fatalf("a pass 35 days and 1 s old = %q, want %q", got.Status, RecoverStale)
	}
	if !strings.Contains(got.Reason, "35 days") {
		t.Fatalf("stale reason %q does not name the window", got.Reason)
	}
}

func TestClassify_FailedLatestWithoutARecentPassIsFailed(t *testing.T) {
	fail := failedAt(recNow.Add(-time.Hour), "row_count app.orders: 90 rows, source 100")
	old := passedAt(recNow.Add(-40*24*time.Hour), 600)
	got := Classify("orders", fail, old, "", recNow)
	if got.Status != RecoverFailed || got.Reason != fail.Failure {
		t.Fatalf("failed latest = %+v, want failed with the drill's failure", got)
	}
	if got := Classify("orders", fail, nil, "", recNow); got.Status != RecoverFailed {
		t.Fatalf("failed with no pass ever = %q, want failed", got.Status)
	}
}

func TestClassify_FailedAfterARecentPassStillCountsThePass(t *testing.T) {
	// D7 reads the same rule: a failed drill after a pass does not reset it.
	pass := passedAt(recNow.Add(-10*24*time.Hour), 600)
	fail := failedAt(recNow.Add(-time.Hour), "marker: no committed marker")
	got := Classify("orders", fail, pass, "", recNow)
	if got.Status != RecoverPassed || got.Latest != fail || got.LastPass != pass {
		t.Fatalf("fail after a recent pass = %+v, want passed, latest the failure", got)
	}
}

func TestClassify_TransitionFromFailedToPassed(t *testing.T) {
	fail := failedAt(recNow.Add(-2*time.Hour), "readable app.t: permission denied")
	if got := Classify("orders", fail, nil, "", recNow); got.Status != RecoverFailed {
		t.Fatalf("before = %q, want failed", got.Status)
	}
	pass := passedAt(recNow.Add(-time.Hour), 600)
	got := Classify("orders", pass, pass, "", recNow)
	if got.Status != RecoverPassed || got.Reason != "" {
		t.Fatalf("after the next pass = %+v, want passed with no reason", got)
	}
}

func TestClassify_NoDrillsIsNeverDrilledOrUnknown(t *testing.T) {
	got := Classify("orders", nil, nil, "", recNow)
	if got.Status != RecoverNeverDrilled || got.Reason == "" || got.Advice != "" {
		t.Fatalf("no drills, provider set = %+v, want never_drilled with a reason", got)
	}
	got = Classify("orders", nil, nil, "clone.provider is none", recNow)
	if got.Status != RecoverUnknown || got.Reason != "clone.provider is none" {
		t.Fatalf("no drills, no provider = %+v, want unknown with the reason", got)
	}
	if !strings.Contains(got.Advice, "clone.provider") || got.Advice != UnknownAdvice {
		t.Fatalf("unknown advice = %q, want what to configure and grant", got.Advice)
	}
}

func TestClassify_DrillRowsWinOverAnUnknownReason(t *testing.T) {
	pass := passedAt(recNow.Add(-time.Hour), 600)
	got := Classify("orders", pass, pass, "clone.provider is none", recNow)
	if got.Status != RecoverPassed || got.Advice != "" {
		t.Fatalf("a recent pass with the provider since removed = %+v, want passed", got)
	}
}

func TestClassify_ZeroNowNeverCountsAPass(t *testing.T) {
	// A zero clock fails closed: nothing is inside a window that ends at year 1.
	pass := passedAt(recNow, 600)
	if got := Classify("orders", pass, pass, "", time.Time{}); got.Status == RecoverPassed {
		t.Fatalf("zero now = %q, want not passed", got.Status)
	}
}

func classified(status string, rto *float64) Recoverability {
	r := Recoverability{Database: "db", Status: status}
	if status == RecoverPassed {
		r.LastPass = &Result{Passed: true, RTOSeconds: rto}
	}
	return r
}

func rto(v float64) *float64 { return &v }

func TestSummarize_SevenOfNine(t *testing.T) {
	list := []Recoverability{}
	for _, v := range []float64{300, 660, 900, 120, 660, 1200, 480} {
		list = append(list, classified(RecoverPassed, rto(v)))
	}
	list = append(list, classified(RecoverFailed, nil), classified(RecoverUnknown, nil))
	got := Summarize(list)
	if got.Total != 9 || got.Passed != 7 || got.Failed != 1 || got.Unknown != 1 ||
		got.Stale != 0 || got.NeverDrilled != 0 || got.WindowDays != 35 {
		t.Fatalf("summary = %+v, want 7 of 9 passed, 1 failed, 1 unknown", got)
	}
	if got.MedianRTOSeconds == nil || *got.MedianRTOSeconds != 660 {
		t.Fatalf("median RTO = %v, want 660", got.MedianRTOSeconds)
	}
	if len(got.Databases) != 9 {
		t.Fatalf("databases = %d, want all 9 listed", len(got.Databases))
	}
}

func TestSummarize_MedianOfAnEvenCountSkipsUnknownRTO(t *testing.T) {
	list := []Recoverability{classified(RecoverPassed, rto(600)),
		classified(RecoverPassed, rto(900)), classified(RecoverPassed, nil),
		classified(RecoverStale, nil), classified(RecoverNeverDrilled, nil)}
	got := Summarize(list)
	if got.MedianRTOSeconds == nil || *got.MedianRTOSeconds != 750 {
		t.Fatalf("median of 600 and 900 = %v, want 750", got.MedianRTOSeconds)
	}
	if got.Stale != 1 || got.NeverDrilled != 1 || got.Passed != 3 || got.Total != 5 {
		t.Fatalf("summary = %+v", got)
	}
}

func TestSummarize_EmptyFleetHasNoMedian(t *testing.T) {
	got := Summarize(nil)
	if got.Total != 0 || got.MedianRTOSeconds != nil || got.WindowDays != 35 ||
		got.Databases == nil {
		t.Fatalf("empty = %+v, want zero counts, no median, an empty list", got)
	}
	raw, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(raw), `"median_rto_seconds":null`) ||
		!strings.Contains(string(raw), `"databases":[]`) {
		t.Fatalf("empty JSON = %s %v, want a null median and an empty list", raw, err)
	}
}

func TestSummarize_AnUnrecognisedStatusCountsAsUnknown(t *testing.T) {
	got := Summarize([]Recoverability{{Database: "db", Status: "bogus"}})
	if got.Unknown != 1 || got.Total != 1 || got.Passed != 0 {
		t.Fatalf("bogus status = %+v, want it counted unknown, never passed", got)
	}
}

func TestResultJSON_OmitsAZeroRecoveryPoint(t *testing.T) {
	raw, err := json.Marshal(Result{Database: "orders", FinishedAt: recNow})
	if err != nil || strings.Contains(string(raw), "recovery_point") {
		t.Fatalf("JSON = %s %v, want no recovery_point when it is unknown", raw, err)
	}
}

func TestValidHistoryLimit(t *testing.T) {
	for _, c := range []struct {
		n  int
		ok bool
	}{{0, false}, {1, true}, {20, true}, {MaxHistoryLimit, true},
		{MaxHistoryLimit + 1, false}, {-1, false}} {
		if err := ValidHistoryLimit(c.n); (err == nil) != c.ok {
			t.Errorf("ValidHistoryLimit(%d) = %v, want ok=%v", c.n, err, c.ok)
		}
	}
}
