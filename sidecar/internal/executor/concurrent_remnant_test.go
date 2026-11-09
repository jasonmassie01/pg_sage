package executor

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Unit tests of the remnant handler's pure parts (executor-05). The
// catalog behaviour is in concurrent_remnant_db_test.go. No concurrent
// access tests here: these functions take no shared state.

func TestRemnantOverallFoldsFailureFirst(t *testing.T) {
	cases := []struct {
		outcomes []string
		want     string
	}{
		{nil, remnantNothingToDrop},
		{[]string{remnantGone}, remnantNothingToDrop},
		{[]string{remnantDropped, remnantCleanupFailed}, remnantCleanupFailed},
		{[]string{remnantOIDMismatch, remnantDropped}, remnantDropped},
		{[]string{remnantGone, remnantOIDMismatch}, remnantOIDMismatch},
		{[]string{remnantValidKept}, remnantValidKept},
		{[]string{remnantInProgress, remnantGone}, remnantInProgress},
	}
	for _, tc := range cases {
		c := &remnantCleanup{}
		for _, o := range tc.outcomes {
			c.Indexes = append(c.Indexes, remnantIndex{Name: "public.i", OID: 1, Outcome: o})
		}
		if got := c.overall(); got != tc.want {
			t.Errorf("overall(%v) = %s, want %s", tc.outcomes, got, tc.want)
		}
	}
}

func TestWithCleanupKeepsTheErrorIdentity(t *testing.T) {
	base := errors.Join(ErrLockNotAvailable, errors.New("canceling statement"))
	c := &remnantCleanup{Outcome: remnantDropped, Indexes: []remnantIndex{
		{Name: `"public"."i"`, OID: 4242, Outcome: remnantDropped}}}
	err := withCleanup(base, c)
	if !errors.Is(err, ErrLockNotAvailable) {
		t.Fatalf("withCleanup lost ErrLockNotAvailable: %v", err)
	}
	for _, want := range []string{"remnant_cleanup: dropped", `"public"."i" (OID 4242)`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if withCleanup(nil, c) != nil || withCleanup(base, nil) != base {
		t.Fatal("withCleanup must pass a nil error or a nil cleanup through unchanged")
	}
}

func TestRecordRemnantCleanupIsNilSafe(t *testing.T) {
	recordRemnantCleanup(nil, &remnantCleanup{Outcome: remnantDropped})
	state := map[string]any{}
	recordRemnantCleanup(state, nil)
	if len(state) != 0 {
		t.Fatalf("a nil cleanup wrote %v", state)
	}
	c := &remnantCleanup{Outcome: remnantCleanupFailed}
	recordRemnantCleanup(state, c)
	if state[remnantCleanupKey] != c {
		t.Fatalf("state = %v, want the cleanup under %q", state, remnantCleanupKey)
	}
}

// A retry that fails on the name a failed cleanup left keeps that failure
// on record; a retry that cleaned up its own remnant replaces it.
func TestRecordRemnantCleanupKeepsALeftRemnant(t *testing.T) {
	state := map[string]any{}
	failed := &remnantCleanup{Outcome: remnantCleanupFailed}
	recordRemnantCleanup(state, failed)
	recordRemnantCleanup(state, &remnantCleanup{Outcome: remnantNothingToDrop})
	if state[remnantCleanupKey] != failed {
		t.Fatalf("state = %+v, want the earlier cleanup failure kept", state[remnantCleanupKey])
	}
	unbound := &remnantCleanup{Outcome: remnantUnbound}
	recordRemnantCleanup(state, unbound)
	if state[remnantCleanupKey] != unbound {
		t.Fatalf("state = %+v, want the later unbound cleanup", state[remnantCleanupKey])
	}
	dropped := &remnantCleanup{Outcome: remnantDropped}
	recordRemnantCleanup(state, dropped)
	if state[remnantCleanupKey] != unbound {
		t.Fatalf("state = %+v, want unbound kept over a later drop", state[remnantCleanupKey])
	}
	clean := map[string]any{}
	first := &remnantCleanup{Outcome: remnantDropped}
	recordRemnantCleanup(clean, first)
	later := &remnantCleanup{Outcome: remnantNothingToDrop}
	recordRemnantCleanup(clean, later)
	if clean[remnantCleanupKey] != later {
		t.Fatalf("state = %+v, want the latest cleanup", clean[remnantCleanupKey])
	}
}

func TestSkippedCleanupAfterAnEmergencyStop(t *testing.T) {
	stopped := &StoppedError{Cause: errors.New("emergency stop")}
	if c := skippedCleanup(stopped); c == nil || c.Outcome != remnantSkippedStopped {
		t.Fatalf("skippedCleanup(stop) = %+v, want skipped_stopped", c)
	}
	if c := skippedCleanup(ErrLockNotAvailable); c != nil {
		t.Fatalf("skippedCleanup(lock timeout) = %+v, want nil (clean up)", c)
	}
}

func TestReindexRemnantNames(t *testing.T) {
	snap := reindexSnapshot{Names: map[string]bool{"x": true, "x_ccnew": true,
		"orders_customer_id_created_at_idx": true}}
	cases := map[string]bool{
		"x_ccnew1":                              true,  // the failed statement's copy
		"x_ccold":                               true,  // the retired original after the swap
		"x_ccnew":                               false, // pre-existing: someone else's
		"x":                                     false, // the original
		"y_ccnew":                               false, // stem begins no name the table had
		"x_cc":                                  false,
		"orders_customer_id_created_at_i_ccnew": true, // stem truncated by the server
		"":                                      false,
	}
	for name, want := range cases {
		if got := snap.added(name); got != want {
			t.Errorf("added(%q) = %v, want %v", name, got, want)
		}
	}
	if (reindexSnapshot{}).added("x_ccnew") {
		t.Fatal("an empty snapshot attributes nothing to the statement")
	}
}

func TestParseReindexTarget(t *testing.T) {
	cases := []struct {
		sql, kind, target string
		ok                bool
	}{
		{"REINDEX INDEX CONCURRENTLY public.i", "INDEX", "public.i", true},
		{"reindex (verbose) table concurrently app.\"Orders\";", "TABLE", `app."Orders"`, true},
		{"REINDEX INDEX CONCURRENTLY i", "", "", false},
		{"REINDEX INDEX public.i", "", "", false},
		{"REINDEX INDEX CONCURRENTLY public.i extra", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		kind, target, err := parseReindexTarget(tc.sql)
		if tc.ok != (err == nil) || kind != tc.kind || target != tc.target {
			t.Errorf("parseReindexTarget(%q) = %q, %q, %v", tc.sql, kind, target, err)
		}
	}
}

func TestGateHoldStaysBelowTheLockTimeout(t *testing.T) {
	cases := map[int]time.Duration{
		0:      gateHoldDefault,
		-1:     gateHoldDefault,
		10:     5 * time.Millisecond,
		1:      500 * time.Microsecond,
		1000:   500 * time.Millisecond,
		30000:  gateHoldMax,
		600000: gateHoldMax,
	}
	for ms, want := range cases {
		if got := gateHold(ms); got != want {
			t.Errorf("gateHold(%d) = %s, want %s", ms, got, want)
		}
	}
}

func TestOnlyConcurrentBuildsAreTracked(t *testing.T) {
	if !isConcurrentCreateIndex("CREATE INDEX CONCURRENTLY i ON public.t (a)") ||
		isConcurrentCreateIndex("CREATE INDEX i ON public.t (a)") ||
		isConcurrentCreateIndex("DROP INDEX CONCURRENTLY public.i") {
		t.Fatal("isConcurrentCreateIndex misclassifies a statement")
	}
	if !isConcurrentReindex("REINDEX INDEX CONCURRENTLY public.i") ||
		isConcurrentReindex("REINDEX INDEX public.i") {
		t.Fatal("isConcurrentReindex misclassifies a statement")
	}
}
