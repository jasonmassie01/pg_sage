package policy

import (
	"sync"
	"testing"
	"time"
)

// Wednesday 2026-11-11 12:00 UTC is 06:00 in Chicago (CST, UTC-6).
var previewFrom = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func mustPreview(t *testing.T, expression string, from time.Time) WindowPreview {
	t.Helper()
	window, err := ParseWindow(expression)
	if err != nil {
		t.Fatalf("ParseWindow(%q): %v", expression, err)
	}
	return window.Preview(from, PreviewHorizon)
}

func wantInstant(t *testing.T, name string, got time.Time, want string) {
	t.Helper()
	if want == "" {
		if !got.IsZero() {
			t.Errorf("%s = %s, want none", name, got.Format(time.RFC3339))
		}
		return
	}
	if got.Format(time.RFC3339) != want {
		t.Errorf("%s = %s, want %s", name, got.Format(time.RFC3339), want)
	}
}

func TestWindowPreview_WeeknightsInZone(t *testing.T) {
	preview := mustPreview(t, "weeknights America/Chicago", previewFrom)
	if preview.OpenNow {
		t.Fatal("weeknights is open at 06:00 Chicago; 22:00-06:00 ends at 06:00")
	}
	wantInstant(t, "ClosesAt", preview.ClosesAt, "")
	wantInstant(t, "NextOpen", preview.NextOpen, "2026-11-11T22:00:00-06:00")
	wantInstant(t, "NextClose", preview.NextClose, "2026-11-12T06:00:00-06:00")
}

// Boundary: a range that ends before it starts wraps midnight and belongs
// to the day it starts on.
func TestWindowPreview_WrapsMidnightOnItsStartDay(t *testing.T) {
	preview := mustPreview(t, "23:00-01:00", previewFrom)
	if preview.OpenNow {
		t.Fatal("23:00-01:00 is open at 12:00")
	}
	wantInstant(t, "NextOpen", preview.NextOpen, "2026-11-11T23:00:00Z")
	wantInstant(t, "NextClose", preview.NextClose, "2026-11-12T01:00:00Z")
}

// State: inside a window the preview says when it closes and when the
// following window opens.
func TestWindowPreview_OpenNowReportsCloseAndNextOpening(t *testing.T) {
	from := time.Date(2026, 11, 11, 23, 30, 0, 0, time.UTC)
	preview := mustPreview(t, "23:00-01:00", from)
	if !preview.OpenNow {
		t.Fatal("23:00-01:00 is closed at 23:30")
	}
	wantInstant(t, "ClosesAt", preview.ClosesAt, "2026-11-12T01:00:00Z")
	wantInstant(t, "NextOpen", preview.NextOpen, "2026-11-12T23:00:00Z")
	wantInstant(t, "NextClose", preview.NextClose, "2026-11-13T01:00:00Z")
}

// Boundary: a window that opens on the instant is open now, and one that
// closes on the instant is not.
func TestWindowPreview_EdgesOfTheRange(t *testing.T) {
	opening := time.Date(2026, 11, 11, 23, 0, 0, 0, time.UTC)
	if preview := mustPreview(t, "23:00-01:00", opening); !preview.OpenNow {
		t.Error("23:00-01:00 is closed at exactly 23:00")
	}
	closing := time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC)
	preview := mustPreview(t, "23:00-01:00", closing)
	if preview.OpenNow {
		t.Error("23:00-01:00 is open at exactly 01:00")
	}
	wantInstant(t, "NextOpen", preview.NextOpen, "2026-11-12T23:00:00Z")
}

// A seconds-offset instant still reports minute-aligned transitions.
func TestWindowPreview_SubMinuteInstant(t *testing.T) {
	from := time.Date(2026, 11, 11, 22, 59, 30, 0, time.UTC)
	preview := mustPreview(t, "23:00-01:00", from)
	wantInstant(t, "NextOpen", preview.NextOpen, "2026-11-11T23:00:00Z")
}

func TestWindowPreview_AlwaysNeverCloses(t *testing.T) {
	preview := mustPreview(t, "always", previewFrom)
	if !preview.OpenNow {
		t.Fatal("always is closed")
	}
	wantInstant(t, "ClosesAt", preview.ClosesAt, "")
	wantInstant(t, "NextOpen", preview.NextOpen, "")
}

func TestWindowPreview_CronWidth(t *testing.T) {
	preview := mustPreview(t, "0 2 * * 0 @2h", previewFrom)
	// Sunday 2026-11-15 02:00 UTC, two hours wide.
	wantInstant(t, "NextOpen", preview.NextOpen, "2026-11-15T02:00:00Z")
	wantInstant(t, "NextClose", preview.NextClose, "2026-11-15T04:00:00Z")
}

// Boundary: a window that first opens beyond the horizon reports none.
func TestWindowPreview_NoOpeningInsideHorizon(t *testing.T) {
	preview := mustPreview(t, "0 2 29 2 *", previewFrom)
	if preview.OpenNow {
		t.Fatal("a February 29 window is open in November")
	}
	wantInstant(t, "NextOpen", preview.NextOpen, "")
	wantInstant(t, "NextClose", preview.NextClose, "")
}

// Boundary: an opening exactly at the horizon is inside it; one minute
// later is not.
func TestWindowPreview_HorizonEdge(t *testing.T) {
	window, err := ParseWindow("23:00-01:00")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 11, 11, 13, 0, 0, 0, time.UTC)
	inside := window.Preview(from, 10*time.Hour)
	wantInstant(t, "NextOpen at horizon", inside.NextOpen, "2026-11-11T23:00:00Z")
	outside := window.Preview(from, 10*time.Hour-time.Minute)
	wantInstant(t, "NextOpen past horizon", outside.NextOpen, "")
}

// Nil/zero: the zero Window is never open and the preview says so.
func TestWindowPreview_ZeroWindow(t *testing.T) {
	preview := Window{}.Preview(previewFrom, PreviewHorizon)
	if preview.OpenNow || !preview.NextOpen.IsZero() || !preview.ClosesAt.IsZero() {
		t.Fatalf("zero window preview = %+v, want closed with no opening", preview)
	}
	if got := (Window{}).Preview(previewFrom, 0); got.OpenNow || !got.NextOpen.IsZero() {
		t.Fatalf("zero horizon preview = %+v, want empty", got)
	}
}

// Without a zone the instant's own location is used.
func TestWindowPreview_InstantLocation(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	preview := mustPreview(t, "weeknights", previewFrom.In(chicago))
	wantInstant(t, "NextOpen", preview.NextOpen, "2026-11-11T22:00:00-06:00")
}

// The worst cron case (a 24 h width that never matches) stays bounded.
func TestWindowPreview_WideCronIsBounded(t *testing.T) {
	started := time.Now()
	preview := mustPreview(t, "0 0 29 2 * @24h", previewFrom)
	if !preview.NextOpen.IsZero() {
		t.Fatalf("NextOpen = %s, want none", preview.NextOpen)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("preview took %s, want well under 5s", elapsed)
	}
}

// Concurrent: a parsed Window is read-only and shared by callers.
func TestWindowPreview_ConcurrentCallers(t *testing.T) {
	window, err := ParseWindow("weeknights America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := window.Preview(previewFrom, PreviewHorizon)
			if got.NextOpen.Format(time.RFC3339) != "2026-11-11T22:00:00-06:00" {
				errs <- got.NextOpen.Format(time.RFC3339)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for got := range errs {
		t.Errorf("concurrent NextOpen = %s", got)
	}
}
