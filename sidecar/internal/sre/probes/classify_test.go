package probes

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A probe error is never read as healthy: every failure maps to a typed
// status and a stable reason code.
func TestClassify_PostgresErrorCodes(t *testing.T) {
	cases := []struct {
		code   string
		status Status
		reason string
	}{
		{"42501", StatusNoPrivilege, "insufficient_privilege"},
		{"42P01", StatusUnsupported, "undefined_table"},
		{"42883", StatusUnsupported, "undefined_function"},
		{"42703", StatusUnsupported, "undefined_column"},
		{"0A000", StatusUnsupported, "feature_not_supported"},
		{"55000", StatusUnsupported, "prerequisite_not_met"},
		{"57014", StatusError, "statement_timeout"},
		{"55P03", StatusError, "lock_timeout"},
		{"25006", StatusError, "read_only_violation"},
		{"53300", StatusError, "query_failed"},
	}
	for _, c := range cases {
		err := fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: c.code,
			Message: "boom"})
		status, reason := classify(err)
		if status != c.status || reason != c.reason {
			t.Errorf("code %s -> (%s, %s), want (%s, %s)", c.code, status,
				reason, c.status, c.reason)
		}
	}
}

func TestClassify_ContextAndTransportErrors(t *testing.T) {
	if s, r := classify(context.DeadlineExceeded); s != StatusError ||
		r != "deadline_exceeded" {
		t.Errorf("deadline -> (%s, %s)", s, r)
	}
	if s, r := classify(fmt.Errorf("x: %w", context.Canceled)); s != StatusError ||
		r != "canceled" {
		t.Errorf("canceled -> (%s, %s)", s, r)
	}
	if s, r := classify(errors.New("connection refused")); s != StatusError ||
		r != "query_failed" {
		t.Errorf("transport -> (%s, %s)", s, r)
	}
	if s, r := classify(nil); s != StatusOK || r != "" {
		t.Errorf("nil -> (%s, %s), want ok", s, r)
	}
}

func TestStatus_UsableOnlyForOKAndEmpty(t *testing.T) {
	usable := map[Status]bool{StatusOK: true, StatusEmpty: true,
		StatusError: false, StatusNoPrivilege: false, StatusUnsupported: false,
		Status(""): false}
	for s, want := range usable {
		if got := s.Usable(); got != want {
			t.Errorf("%q.Usable() = %v, want %v", s, got, want)
		}
	}
}
