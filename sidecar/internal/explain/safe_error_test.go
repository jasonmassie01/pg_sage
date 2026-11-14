package explain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// security-10: SafeError withholds server text that can quote column
// values (SQLSTATE classes 22 and 23) from agents and models.
// No concurrent access tests: SafeError is a pure function of its input.
// No state transition tests: SafeError holds no state.

const ssn = "123-45-6789"

func castError() *pgconn.PgError {
	return &pgconn.PgError{Severity: "ERROR", Code: "22P02",
		Message:       `invalid input syntax for type integer: "` + ssn + `"`,
		Detail:        "value " + ssn,
		Hint:          "check " + ssn,
		Where:         "column ssn = " + ssn,
		InternalQuery: "SELECT '" + ssn + "'::int",
		TableName:     "people"}
}

// safePgError returns the PgError SafeError's result carries, failing the
// test when there is none.
func safePgError(t *testing.T, err error) *pgconn.PgError {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("SafeError(%v) carries no *pgconn.PgError", err)
	}
	return pgErr
}

func requireNoValue(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("SafeError returned nil for a non-nil error")
	}
	if strings.Contains(err.Error(), ssn) {
		t.Fatalf("withheld error still quotes the value: %q", err.Error())
	}
	p := safePgError(t, err)
	for name, s := range map[string]string{"Message": p.Message, "Detail": p.Detail,
		"Hint": p.Hint, "Where": p.Where, "InternalQuery": p.InternalQuery} {
		if strings.Contains(s, ssn) {
			t.Errorf("withheld PgError %s still quotes the value: %q", name, s)
		}
	}
}

func TestSafeError_WithholdsADataException(t *testing.T) {
	got := SafeError(castError())
	requireNoValue(t, got)
	p := safePgError(t, got)
	if p.Code != "22P02" {
		t.Errorf("SQLSTATE = %q, want 22P02 kept", p.Code)
	}
	if p.Message != WithheldDataMessage {
		t.Errorf("Message = %q, want %q", p.Message, WithheldDataMessage)
	}
	if p.Detail != "" || p.Hint != "" || p.Where != "" || p.InternalQuery != "" {
		t.Errorf("text fields not cleared: %+v", p)
	}
	if p.Severity != "ERROR" || p.TableName != "people" {
		t.Errorf("non-value fields lost: severity %q table %q", p.Severity, p.TableName)
	}
	if !strings.Contains(got.Error(), "SQLSTATE 22P02") {
		t.Errorf("error text %q does not name the SQLSTATE", got.Error())
	}
}

func TestSafeError_DoesNotMutateTheOriginal(t *testing.T) {
	orig := castError()
	_ = SafeError(orig)
	if !strings.Contains(orig.Message, ssn) || !strings.Contains(orig.Detail, ssn) {
		t.Fatalf("SafeError changed the caller's PgError: %+v", orig)
	}
}

func TestSafeError_KeepsTheContextPrefix(t *testing.T) {
	got := SafeError(fmt.Errorf("execute explain: %w", castError()))
	requireNoValue(t, got)
	if !strings.HasPrefix(got.Error(), "execute explain: ") {
		t.Errorf("error %q lost its context prefix", got.Error())
	}
}

// A wrapper that prints the server message without %w-style formatting
// (here: twice, once via %v of the message alone) must not leak either.
func TestSafeError_MessageOutsideThePgErrorTextIsWithheld(t *testing.T) {
	pgErr := castError()
	err := fmt.Errorf("failed (%s): %w", pgErr.Message, pgErr)
	got := SafeError(err)
	requireNoValue(t, got)
	if !strings.Contains(got.Error(), WithheldDataMessage) {
		t.Errorf("error %q lacks the withheld message", got.Error())
	}
}

func TestSafeError_OtherErrorsPassThrough(t *testing.T) {
	plain := errors.New("connection reset by peer")
	missing := &pgconn.PgError{Code: "42P01", Message: `relation "nope" does not exist`}
	wrapped := fmt.Errorf("explain: %w", missing)
	for _, err := range []error{plain, missing, wrapped} {
		got := SafeError(err)
		if got != err {
			t.Errorf("SafeError(%v) = %v, want the same error unchanged", err, got)
		}
	}
}

func TestSafeError_NilIsNil(t *testing.T) {
	if got := SafeError(nil); got != nil {
		t.Fatalf("SafeError(nil) = %v, want nil", got)
	}
}

func TestSafeError_KeepsWrappedSentinels(t *testing.T) {
	sentinel := errors.New("unavailable")
	err := fmt.Errorf("%w: plan failed: %w", sentinel, castError())
	got := SafeError(err)
	requireNoValue(t, got)
	if !errors.Is(got, sentinel) {
		t.Errorf("errors.Is(SafeError(err), sentinel) = false for %v", got)
	}
	if !errors.Is(SafeError(fmt.Errorf("x: %w", context.Canceled)), context.Canceled) {
		t.Error("a pass-through error lost its sentinel")
	}
	// The original PgError is not reachable through errors.As.
	if p := safePgError(t, got); strings.Contains(p.Message, ssn) {
		t.Errorf("errors.As reached the original PgError: %q", p.Message)
	}
}

func TestSafeError_ClassBoundaries(t *testing.T) {
	cases := []struct {
		code     string
		withheld bool
	}{
		{"22000", true}, {"22003", true}, {"22P02", true},
		{"23505", true}, {"23502", true}, {"23514", true},
		{"21000", false}, {"24000", false}, {"25006", false},
		{"42P01", false}, {"42501", false}, {"2", false}, {"", false},
	}
	for _, c := range cases {
		err := &pgconn.PgError{Code: c.code, Message: "key (ssn)=(" + ssn + ")",
			Detail: "Key (ssn)=(" + ssn + ") already exists."}
		got := SafeError(err)
		leaks := strings.Contains(got.Error(), ssn) ||
			strings.Contains(safePgError(t, got).Detail, ssn)
		if leaks == c.withheld {
			t.Errorf("code %q: leaks=%v, want withheld=%v (%v)", c.code, leaks, c.withheld, got)
		}
		if p := safePgError(t, got); p.Code != c.code {
			t.Errorf("code %q: SQLSTATE became %q", c.code, p.Code)
		}
	}
}

func TestSafeError_ConstraintWording(t *testing.T) {
	err := &pgconn.PgError{Code: "23505", Message: "duplicate key value",
		Detail: "Key (ssn)=(" + ssn + ") already exists."}
	p := safePgError(t, SafeError(err))
	if p.Message != WithheldConstraintMessage || p.Detail != "" {
		t.Errorf("23505 = %+v, want message %q and no detail", p, WithheldConstraintMessage)
	}
}

func TestSafeError_InvalidRequestUnchanged(t *testing.T) {
	err := fmt.Errorf("%w: give query or query_id, not both", ErrExplainInvalidRequest)
	got := SafeError(err)
	if got != err || got.Error() != err.Error() {
		t.Errorf("validator error changed: %v", got)
	}
}
