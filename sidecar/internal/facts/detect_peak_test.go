package facts

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// evid-03b: the peak-window suggestion is the hours of the week whose
// execution time is in the week's top decile in at least 3 of the 4 weeks,
// stated in the policy window grammar (UTC). These tests cover the pure
// computation; detect_peak_db_test.go covers the reads.
// No concurrent access tests: suggestPeaks is a pure function.

// peakFrom is a Monday 00:00 UTC, the start of a 28-day window.
var peakFrom = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

// peakLoad is 28 days of base load (100 ms an hour) with peaks: each
// entry is a weekday, an hour and the weeks (0-3) it peaks in.
type peakAt struct {
	day   time.Weekday
	hour  int
	weeks []int
}

func peakLoad(peaks ...peakAt) map[time.Time]float64 {
	exec := map[time.Time]float64{}
	for h := 0; h < 28*24; h++ {
		exec[peakFrom.Add(time.Duration(h)*time.Hour)] = 100
	}
	for _, p := range peaks {
		for _, w := range p.weeks {
			day := (int(p.day) + 6) % 7 // Monday is day 0 of the window
			at := peakFrom.Add(time.Duration(w*168+day*24+p.hour) * time.Hour)
			exec[at] = 10000 + float64(p.hour)
		}
	}
	return exec
}

func TestSuggestPeaksThreeOfFourWeeks(t *testing.T) {
	exec := peakLoad(peakAt{time.Monday, 9, []int{0, 1, 2}},
		peakAt{time.Monday, 10, []int{0, 1, 3}}, peakAt{time.Monday, 11, []int{1, 2, 3}},
		peakAt{time.Tuesday, 14, []int{0, 1}}) // 2 of 4: not suggested
	got := suggestPeaks(peakFrom, exec)
	if got.PeakWindows != "mon 09:00-12:00 UTC" || got.Reason != "" {
		t.Fatalf("suggestion %+v, want mon 09:00-12:00 UTC", got)
	}
	if len(got.Hours) != 9 {
		t.Fatalf("hours it rests on = %d (%v), want 9 (3 slots x 3 weeks)", len(got.Hours),
			got.Hours)
	}
	for _, h := range got.Hours {
		if h.Hour.Weekday() != time.Monday || h.ExecMs < 10000 {
			t.Fatalf("evidence hour %+v is not a Monday peak", h)
		}
	}
	w, err := policy.ParseWindow(got.PeakWindows)
	if err != nil {
		t.Fatalf("the suggestion does not parse: %v", err)
	}
	for at, want := range map[time.Time]bool{
		time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC):   true,
		time.Date(2026, 11, 16, 11, 59, 0, 0, time.UTC): true,
		time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC):  false,
		time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC):  false,
	} {
		if w.Contains(at) != want {
			t.Errorf("Contains(%s) = %v, want %v", at, !want, want)
		}
	}
}

func TestSuggestPeaksTwoOfFourIsNothing(t *testing.T) {
	got := suggestPeaks(peakFrom, peakLoad(peakAt{time.Tuesday, 14, []int{0, 1}},
		peakAt{time.Friday, 3, []int{2, 3}}))
	if got.PeakWindows != "" || got.Reason == "" || len(got.Hours) != 0 {
		t.Fatalf("2 of 4 weeks suggested %+v", got)
	}
}

// Days with the same hours share a window; a range to midnight ends at
// 00:00 and a whole day is the day alone.
func TestSuggestPeaksGroupsDaysAndMidnight(t *testing.T) {
	all := []int{0, 1, 2, 3}
	var peaks []peakAt
	for _, d := range []time.Weekday{time.Monday, time.Tuesday, time.Wednesday} {
		peaks = append(peaks, peakAt{d, 22, all}, peakAt{d, 23, all})
	}
	got := suggestPeaks(peakFrom, peakLoad(peaks...))
	if got.PeakWindows != "mon,tue,wed 22:00-00:00 UTC" {
		t.Fatalf("grouped suggestion %q", got.PeakWindows)
	}
	w, err := policy.ParseWindow(got.PeakWindows)
	if err != nil || !w.Contains(time.Date(2026, 11, 18, 23, 30, 0, 0, time.UTC)) ||
		w.Contains(time.Date(2026, 11, 19, 0, 30, 0, 0, time.UTC)) {
		t.Fatalf("window %q: err %v or wrong instants", got.PeakWindows, err)
	}
	day := map[time.Time]float64{}
	for h := 0; h < 28*24; h++ {
		at := peakFrom.Add(time.Duration(h) * time.Hour)
		day[at] = 0
		if at.Weekday() == time.Sunday && at.Hour() < 17 {
			day[at] = 500 + float64(at.Hour())
		}
	}
	if got := suggestPeaks(peakFrom, day); got.PeakWindows != "sun 00:00-17:00 UTC" {
		t.Fatalf("one day's peak %q", got.PeakWindows)
	}
}

// Ties at the decile boundary are left out (it suggests less, never more),
// and idle hours are never peaks.
func TestSuggestPeaksTiesAndIdleHours(t *testing.T) {
	flat := peakLoad()
	if got := suggestPeaks(peakFrom, flat); got.PeakWindows != "" {
		t.Fatalf("flat load suggested %q", got.PeakWindows)
	}
	idle := map[time.Time]float64{}
	if got := suggestPeaks(peakFrom, idle); got.PeakWindows != "" || got.Reason == "" {
		t.Fatalf("no rollup hours suggested %+v", got)
	}
	if got := suggestPeaks(peakFrom, nil); got.PeakWindows != "" || got.Reason == "" {
		t.Fatalf("nil rollup suggested %+v", got)
	}
	// 17 hours (the decile of 168) tie at the top: all of them are in it.
	top := peakLoad()
	for w := 0; w < 4; w++ {
		for h := 0; h < 17; h++ {
			top[peakFrom.Add(time.Duration(w*168+24+h)*time.Hour)] = 9000
		}
	}
	if got := suggestPeaks(peakFrom, top); got.PeakWindows != "tue 00:00-17:00 UTC" {
		t.Fatalf("17 tied hours: %q", got.PeakWindows)
	}
	// 18 tied hours are more than a decile: none of them is suggested.
	for w := 0; w < 4; w++ {
		top[peakFrom.Add(time.Duration(w*168+24+17)*time.Hour)] = 9000
	}
	if got := suggestPeaks(peakFrom, top); got.PeakWindows != "" {
		t.Fatalf("18 tied hours: %q", got.PeakWindows)
	}
}

// Hours outside the 28-day window are ignored.
func TestSuggestPeaksIgnoresHoursOutsideTheWindow(t *testing.T) {
	exec := peakLoad()
	for w := 0; w < 4; w++ {
		exec[peakFrom.Add(-time.Duration(w*168+1)*time.Hour)] = 1e6 // before the window
	}
	exec[peakFrom.Add(28*24*time.Hour)] = 1e6
	if got := suggestPeaks(peakFrom, exec); got.PeakWindows != "" {
		t.Fatalf("out-of-window hours suggested %q", got.PeakWindows)
	}
}

// A suggestion too scattered for a brief value (300 characters) is
// refused with its reason rather than cut: 17 single hours, each on its
// own day and hour, make 17 windows.
func TestSuggestPeaksTooScatteredForTheBrief(t *testing.T) {
	var peaks []peakAt
	for h := 0; h < 17; h++ {
		peaks = append(peaks, peakAt{time.Weekday(h % 7), h, []int{0, 1, 2, 3}})
	}
	got := suggestPeaks(peakFrom, peakLoad(peaks...))
	if got.PeakWindows != "" || !strings.Contains(got.Reason, "300") ||
		len(got.Hours) != 0 {
		t.Fatalf("scattered suggestion %+v, want none naming the 300-character bound", got)
	}
	// Fewer windows fit, and the suggestion is a valid brief value.
	got = suggestPeaks(peakFrom, peakLoad(peaks[:6]...))
	if got.PeakWindows == "" || len(got.PeakWindows) > maxValueText {
		t.Fatalf("6 windows: %+v", got)
	}
	if _, err := validateBrief(map[string]string{"peak_windows": got.PeakWindows}); err != nil {
		t.Fatalf("suggestion %q is not a valid brief: %v", got.PeakWindows, err)
	}
}
