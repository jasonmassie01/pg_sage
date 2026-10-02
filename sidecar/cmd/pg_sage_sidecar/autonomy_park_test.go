package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// Dogfood lifeos-1 findings 5 and 6: a custodian proposal the policy
// withholds, or an index proposal that cannot be verified (a schema-guard
// FK index with no workload to measure), is an expected outcome. The
// router parks it (the schema guard records it for the operator) and
// logs it once per target and reason, never as an error.

func TestParkExpectedRefusal(t *testing.T) {
	withheld := fmt.Errorf("%w: blast_radius_exceeded", executor.ErrCustodianProposalWithheld)
	unverifiable := fmt.Errorf("route custodian index through verification: %w: no target "+
		"queries to verify against", executor.ErrVerificationUnavailable)
	other := errors.New("connection refused")
	cases := []struct {
		in       error
		parked   bool
		contains string
	}{
		{withheld, true, "withheld by policy"},
		{unverifiable, true, "cannot be verified"},
		{other, false, "connection refused"},
	}
	for _, c := range cases {
		got := parkExpectedRefusal(c.in)
		var parked *schemaguard.ParkedRoute
		if errors.As(got, &parked) != c.parked || !strings.Contains(got.Error(), c.contains) {
			t.Errorf("parkExpectedRefusal(%v) = %v, want parked=%v containing %q", c.in, got,
				c.parked, c.contains)
		}
		if c.parked && !errors.Is(got, c.in) {
			t.Errorf("parked error lost its cause: %v", got)
		}
	}
	if parkExpectedRefusal(nil) != nil {
		t.Fatal("nil error was not passed through")
	}
}

func TestParkedLog_OncePerTargetAndReason(t *testing.T) {
	var lines []string
	l := newParkedLog(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	l.note("public.orders", "withheld by policy: blast_radius_exceeded")
	l.note("public.orders", "withheld by policy: blast_radius_exceeded")
	l.note("public.items", "withheld by policy: blast_radius_exceeded")
	l.note("public.orders", "cannot be verified: no target queries")
	l.note("public.orders", "cannot be verified: no target queries")
	if len(lines) != 3 || !strings.Contains(lines[0], "public.orders") {
		t.Fatalf("lines = %v, want one per target and reason", lines)
	}
}
