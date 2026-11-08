package autoexplain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// No concurrent access tests here: lockBusy and reportCapture hold no
// shared state; the concurrent lock wait is in explain_lock_db_test.go.

func TestLockBusyMapsLockNotAvailable(t *testing.T) {
	cause := &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
	err := lockBusy(fmt.Errorf("explain: %w", cause))
	if !errors.Is(err, errLockBusy) {
		t.Fatalf("55P03 must map to errLockBusy, got %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("mapping must keep the server error, got %v", err)
	}
	if !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("message lost the server detail: %q", err.Error())
	}
}

func TestLockBusyLeavesOtherErrorsAlone(t *testing.T) {
	timeout := &pgconn.PgError{Code: "57014",
		Message: "canceling statement due to statement timeout"}
	cases := map[string]error{
		"statement timeout": fmt.Errorf("explain: %w", timeout),
		"missing relation":  &pgconn.PgError{Code: "42P01"},
		"plain error":       errors.New("acquire connection: closed pool"),
		"deadlock":          &pgconn.PgError{Code: "40P01"},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out := lockBusy(in)
			if errors.Is(out, errLockBusy) {
				t.Fatalf("%v must not be errLockBusy", in)
			}
			if out != in {
				t.Fatalf("error must pass through unchanged: got %v, want %v", out, in)
			}
		})
	}
}

func TestLockBusyNil(t *testing.T) {
	if err := lockBusy(nil); err != nil {
		t.Fatalf("nil must stay nil, got %v", err)
	}
}

type logLine struct{ level, msg string }

func recordingCollector(lines *[]logLine) *Collector {
	return NewCollector(nil, CollectorConfig{}, &Availability{},
		func(level, format string, args ...any) {
			*lines = append(*lines, logLine{level, fmt.Sprintf(format, args...)})
		})
}

func TestReportCaptureLevels(t *testing.T) {
	busy := lockBusy(&pgconn.PgError{Code: "55P03", Message: "lock timeout"})
	timeout := &pgconn.PgError{Code: "57014", Message: "statement timeout"}
	cases := []struct {
		name      string
		err       error
		wantSkip  bool
		wantLevel string
		wantText  string
	}{
		{"success", nil, false, "", ""},
		{"lock busy", busy, true, "DEBUG", "autoexplain: skip queryid=42 this cycle:"},
		{"statement timeout", fmt.Errorf("explain: %w", timeout), false, "WARN",
			"autoexplain: capture queryid=42:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lines []logLine
			c := recordingCollector(&lines)
			if got := c.reportCapture(42, tc.err); got != tc.wantSkip {
				t.Fatalf("skipped = %v, want %v", got, tc.wantSkip)
			}
			if tc.wantLevel == "" {
				if len(lines) != 0 {
					t.Fatalf("a success must not log, got %v", lines)
				}
				return
			}
			if len(lines) != 1 || lines[0].level != tc.wantLevel ||
				!strings.HasPrefix(lines[0].msg, tc.wantText) ||
				strings.Contains(lines[0].msg, "%!") {
				t.Fatalf("log = %v, want one %s line starting %q", lines, tc.wantLevel, tc.wantText)
			}
		})
	}
}

func TestExplainBoundsShareTheProbeLockTimeout(t *testing.T) {
	if selfmonitor.ProbeLockTimeout != 100*time.Millisecond {
		t.Fatalf("probe lock timeout = %v, want 100ms", selfmonitor.ProbeLockTimeout)
	}
	if explainLockTimeoutSQL != "SET LOCAL lock_timeout = '100ms'" {
		t.Fatalf("lock timeout SQL = %q", explainLockTimeoutSQL)
	}
	if explainIdleTimeoutSQL != "SET LOCAL idle_in_transaction_session_timeout = '10s'" {
		t.Fatalf("idle timeout SQL = %q", explainIdleTimeoutSQL)
	}
}
