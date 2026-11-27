package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
)

// A failed host-usage write is logged with the database and resource,
// once per change and at most hourly while it persists (the poller runs
// every minute and must not flood the log).
func TestUsageFailureLog_NamesTheResourceAndDeduplicates(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	var lines []string
	l := &usageFailureLog{now: func() time.Time { return now }, scope: "orders",
		logWarn: func(component, msg string, args ...any) {
			lines = append(lines, component+" "+fmt.Sprintf(msg, args...))
		}}
	u := cloudtel.HourlyUsage{Resource: "rds:us-east-1/orders", Hour: now.Truncate(time.Hour)}
	missing := errors.New(`relation "sage.host_usage_hourly" does not exist`)
	l.report("orders", u, missing)
	now = now.Add(time.Minute)
	l.report("orders", u, missing) // same failure a minute later: quiet
	if len(lines) != 1 {
		t.Fatalf("lines = %q, want one", lines)
	}
	if !strings.Contains(lines[0], `db "orders"`) ||
		!strings.Contains(lines[0], "rds:us-east-1/orders") ||
		!strings.Contains(lines[0], "does not exist") ||
		!strings.Contains(lines[0], "the poll continues") {
		t.Fatalf("line %q must name the database, resource and error", lines[0])
	}
	l.report("orders", u, errors.New("permission denied for table host_usage_hourly"))
	if len(lines) != 2 {
		t.Fatalf("a new error must be logged at once: %q", lines)
	}
	now = now.Add(usageLogEvery)
	l.report("orders", u, errors.New("permission denied for table host_usage_hourly"))
	if len(lines) != 3 {
		t.Fatalf("a persisting error must be repeated hourly: %q", lines)
	}
}

// No concurrent-access test here beyond the mutex: report is called from
// the one poller goroutine of the shared runtime.
