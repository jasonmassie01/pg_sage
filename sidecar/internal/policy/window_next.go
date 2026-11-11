package policy

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// windowScanMinutes bounds every window scan: 8 days of minutes (11,520
// Contains calls), longer than any weekly window's gap.
const windowScanMinutes = 8 * 24 * 60

// ErrNoWindowWithinScan reports a window that does not open (or never
// leaves room for the start margin) within the 8-day scan.
var ErrNoWindowWithinScan = errors.New("no maintenance window opens within 8 days")

// EffectiveWindow is when a moderate or high operator-approved action may
// start: inside trust.maintenance_window when one is configured, and
// inside any standing-policy window. It is the predicate the operator gate
// and MaintenanceWindowOpen evaluate. The zero value never opens.
type EffectiveWindow struct {
	// trust is trust.maintenance_window; nil when it does not bind.
	trust *Window
	// trustNever is a configured trust window that never opens (empty,
	// or a disabled alias).
	trustNever bool
	policy     []Window
}

// NewEffectiveWindow builds the intersection of trust.maintenance_window
// (when trustConfigured) and the union of the policy windows. A configured
// but empty trust expression never opens. Invalid expressions are errors.
func NewEffectiveWindow(trustConfigured bool, trustExpression string,
	policyWindows []string) (EffectiveWindow, error) {
	if len(policyWindows) == 0 {
		return EffectiveWindow{}, errors.New("maintenance_windows cannot be empty")
	}
	effective := EffectiveWindow{}
	for _, expression := range policyWindows {
		window, err := ParseWindow(expression)
		if err != nil {
			return EffectiveWindow{}, fmt.Errorf("policy window: %w", err)
		}
		effective.policy = append(effective.policy, window)
	}
	if !trustConfigured {
		return effective, nil
	}
	if strings.TrimSpace(trustExpression) == "" {
		effective.trustNever = true
		return effective, nil
	}
	trust, err := ParseWindow(trustExpression)
	if err != nil {
		return EffectiveWindow{}, fmt.Errorf("trust.maintenance_window: %w", err)
	}
	effective.trust = &trust
	return effective, nil
}

// Contains reports whether at is inside the effective window.
func (w EffectiveWindow) Contains(at time.Time) bool {
	if w.trustNever || (w.trust != nil && !w.trust.Contains(at)) {
		return false
	}
	for _, window := range w.policy {
		if window.Contains(at) {
			return true
		}
	}
	return false
}

// Location is the zone the window is written in (trust first, then the
// first zoned policy window), or nil when no window names one.
func (w EffectiveWindow) Location() *time.Location {
	if w.trust != nil && w.trust.location != nil {
		return w.trust.location
	}
	for _, window := range w.policy {
		if window.location != nil {
			return window.location
		}
	}
	return nil
}

// minuteAt is the k-th instant of a scan from t: t itself, then the
// following minute boundaries.
func minuteAt(t time.Time, k int) time.Time {
	if k == 0 {
		return t
	}
	return t.Truncate(time.Minute).Add(time.Duration(k) * time.Minute)
}

// NextOpen returns t when the window is open at t, else the first minute
// after t at which it opens; false when it does not open within 8 days.
func (w EffectiveWindow) NextOpen(t time.Time) (time.Time, bool) {
	for k := 0; k <= windowScanMinutes; k++ {
		if at := minuteAt(t, k); w.Contains(at) {
			return at, true
		}
	}
	return time.Time{}, false
}

// ClosesAt returns the first minute boundary after t at which the window
// is closed. It is false when the window is closed at t or stays open for
// the whole 8-day scan.
func (w EffectiveWindow) ClosesAt(t time.Time) (time.Time, bool) {
	if !w.Contains(t) {
		return time.Time{}, false
	}
	return w.closeAfter(t, windowScanMinutes)
}

func (w EffectiveWindow) closeAfter(t time.Time, limit int) (time.Time, bool) {
	for k := 1; k <= limit; k++ {
		if at := minuteAt(t, k); !w.Contains(at) {
			return at, true
		}
	}
	return time.Time{}, false
}

// NextStart is the first instant at or after t at which an action may
// start with at least margin left before the window closes, and that
// close (zero when the window stays open past the scan). It is false when
// no such start exists within 8 days.
func (w EffectiveWindow) NextStart(t time.Time, margin time.Duration) (
	time.Time, time.Time, bool) {
	marginMinutes := int(margin / time.Minute)
	runStart, inRun := time.Time{}, false
	for k := 0; k <= windowScanMinutes+marginMinutes+1; k++ {
		at := minuteAt(t, k)
		open := w.Contains(at)
		switch {
		case open && !inRun && k <= windowScanMinutes:
			runStart, inRun = at, true
		case !open && inRun:
			if at.Sub(runStart) >= margin {
				return runStart, at, true
			}
			inRun = false
		}
	}
	if inRun {
		// Open for longer than the scan past its start: no close to meet.
		return runStart, time.Time{}, true
	}
	return time.Time{}, time.Time{}, false
}

// FormatWindowStart renders a window opening for a person: in the
// window's zone when it names one, else in the process clock the gate
// evaluates an unzoned window in.
func FormatWindowStart(at time.Time, location *time.Location) string {
	if location == nil {
		return at.In(time.Local).Format("Mon 2006-01-02 15:04 MST")
	}
	return at.In(location).Format("Mon 2006-01-02 15:04 ") + location.String()
}
