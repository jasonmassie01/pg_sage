package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type fakeWindowWarnings struct {
	warnings []string
	err      error
}

func (f fakeWindowWarnings) MaintenanceWindowOverrideWarnings(
	context.Context,
) ([]string, error) {
	return f.warnings, f.err
}

type capturedLog struct{ lines []string }

func (c *capturedLog) log(component, msg string, args ...any) {
	c.lines = append(c.lines, component+": "+fmt.Sprintf(msg, args...))
}

// D2: an invalid trust.maintenance_window override stored before
// validation existed is reported at startup, once per row.
func TestWarnInvalidStoredWindowsLogsEachOverride(t *testing.T) {
	var out capturedLog
	warnInvalidStoredWindows(context.Background(), fakeWindowWarnings{
		warnings: []string{`global trust.maintenance_window="weeknigths" invalid`,
			`database 7 trust.maintenance_window="0 2 * *" invalid`},
	}, out.log)

	if len(out.lines) != 2 || !strings.Contains(out.lines[0], "weeknigths") ||
		!strings.HasPrefix(out.lines[1], "config: ") {
		t.Fatalf("log lines = %q, want one config warning per override", out.lines)
	}
}

func TestWarnInvalidStoredWindowsReportsReadFailure(t *testing.T) {
	var out capturedLog
	warnInvalidStoredWindows(context.Background(),
		fakeWindowWarnings{err: errors.New("connection refused")}, out.log)

	if len(out.lines) != 1 || !strings.Contains(out.lines[0], "connection refused") {
		t.Fatalf("log lines = %q, want the read failure", out.lines)
	}
}

func TestWarnInvalidStoredWindowsSilentWhenValid(t *testing.T) {
	var out capturedLog
	warnInvalidStoredWindows(context.Background(), fakeWindowWarnings{}, out.log)
	if len(out.lines) != 0 {
		t.Fatalf("log lines = %q, want none", out.lines)
	}
}
