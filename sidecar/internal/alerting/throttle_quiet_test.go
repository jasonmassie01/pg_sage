package alerting

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// at pins the throttle clock to hh:mm UTC on a fixed day.
func at(th *Throttle, hh, mm int) {
	th.now = func() time.Time { return time.Date(2026, 4, 1, hh, mm, 0, 0, time.UTC) }
}

// Replaces TestCoverage_ShouldAlert_QuietHoursBlock, which checked only
// IsQuietHours and asserted nothing about the alert decision (ci-11).
func TestDecide_QuietHoursDefersAllButCritical(t *testing.T) {
	th := NewThrottle(0, "00:00", "23:00", "UTC")
	at(th, 12, 0)
	if got := th.Decide("k", "warning"); got != DecisionDefer {
		t.Errorf("warning at 12:00 in quiet 00:00-23:00: Decide = %v, want DecisionDefer", got)
	}
	if got := th.Decide("k", "info"); got != DecisionDefer {
		t.Errorf("info at 12:00 in quiet 00:00-23:00: Decide = %v, want DecisionDefer", got)
	}
	if th.ShouldAlert("k", "warning") {
		t.Error("ShouldAlert(warning) = true during quiet hours, want false")
	}
	if got := th.Decide("k", "critical"); got != DecisionSend {
		t.Errorf("critical in quiet hours: Decide = %v, want DecisionSend", got)
	}
}

func TestDecide_OutsideQuietHoursSends(t *testing.T) {
	th := NewThrottle(0, "01:00", "02:00", "UTC")
	at(th, 12, 0)
	if got := th.Decide("k", "warning"); got != DecisionSend {
		t.Errorf("warning at 12:00, quiet 01:00-02:00: Decide = %v, want DecisionSend", got)
	}
}

func TestDecide_QuietHoursBoundaries(t *testing.T) {
	th := NewThrottle(0, "01:00", "02:00", "UTC")
	for _, tc := range []struct {
		hh, mm int
		want   Decision
	}{
		{0, 59, DecisionSend},
		{1, 0, DecisionDefer},  // start is inclusive
		{1, 59, DecisionDefer}, // last quiet minute
		{2, 0, DecisionSend},   // end is exclusive
	} {
		at(th, tc.hh, tc.mm)
		if got := th.Decide("k", "warning"); got != tc.want {
			t.Errorf("warning at %02d:%02d, quiet 01:00-02:00: Decide = %v, want %v",
				tc.hh, tc.mm, got, tc.want)
		}
	}
}

func TestDecide_QuietHoursWrapMidnightAndDisabled(t *testing.T) {
	wrap := NewThrottle(0, "22:30", "06:00", "UTC")
	at(wrap, 23, 0)
	if got := wrap.Decide("k", "warning"); got != DecisionDefer {
		t.Errorf("23:00 in quiet 22:30-06:00: Decide = %v, want DecisionDefer", got)
	}
	at(wrap, 12, 0)
	if got := wrap.Decide("k", "warning"); got != DecisionSend {
		t.Errorf("12:00 outside quiet 22:30-06:00: Decide = %v, want DecisionSend", got)
	}
	off := NewThrottle(0, "", "", "UTC")
	at(off, 12, 0)
	if got := off.Decide("k", "warning"); got != DecisionSend {
		t.Errorf("quiet hours unset: Decide = %v, want DecisionSend", got)
	}
}

// Replaces TestCoverage_LogAlert_PoolExecError, which ended in
// "_ = loggedErr" (ci-11). A pool whose server refuses connections makes
// the insert fail without a database; the failure must reach the log.
func TestLogAlert_InsertFailureIsLogged(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	var logged []string
	logFn := func(level, msg string, args ...any) {
		if level == "ERROR" {
			logged = append(logged, msg)
		}
	}
	m := New(pool, ManagerConfig{}, nil, logFn)
	m.logAlert(context.Background(), 99999, "critical", "test", "key:test", "sent", "")
	if len(logged) != 1 || !strings.Contains(logged[0], "insert alert_log") {
		t.Fatalf("ERROR logs = %q, want one \"insert alert_log\" entry", logged)
	}
}
