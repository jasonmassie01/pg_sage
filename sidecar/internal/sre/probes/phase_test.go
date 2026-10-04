package probes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Phase bookkeeping and the retry policy, without a server. The runner
// tests against PostgreSQL (runner_phase_db_test.go) drive the same
// policy end to end.

func TestRetryable_OnlyServerSideTimeoutsRetry(t *testing.T) {
	cases := []struct {
		reason string
		phase  Phase
		want   bool
	}{
		{"statement_timeout", PhaseExecution, true},
		{"lock_timeout", PhaseLockWait, true},
		{"deadline_exceeded", PhaseExecution, true},
		// The pool already had its own budget: waiting again doubles it.
		{"deadline_exceeded", PhasePoolAcquire, false},
		{"concurrency_limit", PhaseQueue, false},
		{"canceled", PhaseExecution, false},
		{"query_failed", PhaseExecution, false},
		{"insufficient_privilege", PhaseExecution, false},
		{"undefined_table", PhaseExecution, false},
		{"", PhaseExecution, false},
		{"statement_timeout", "", false},
	}
	for _, c := range cases {
		if got := retryable(c.reason, c.phase); got != c.want {
			t.Errorf("retryable(%q, %q) = %v, want %v", c.reason, c.phase, got, c.want)
		}
	}
}

func TestPhaseFor_LockTimeoutIsLockWait(t *testing.T) {
	cases := []struct {
		reason string
		stage  Phase
		want   Phase
	}{
		{"lock_timeout", PhaseExecution, PhaseLockWait},
		{"statement_timeout", PhaseExecution, PhaseExecution},
		{"deadline_exceeded", PhasePoolAcquire, PhasePoolAcquire},
		{"concurrency_limit", PhaseQueue, PhaseQueue},
		{"deadline_exceeded", PhaseExecution, PhaseExecution},
	}
	for _, c := range cases {
		if got := phaseFor(c.reason, c.stage); got != c.want {
			t.Errorf("phaseFor(%q, %q) = %q, want %q", c.reason, c.stage, got, c.want)
		}
	}
}

func TestRetryAllowed_BoundedByAttemptsAndCallerDeadline(t *testing.T) {
	spec := Spec{StatementTimeout: 200 * time.Millisecond}
	need := spec.StatementTimeout + time.Second + retryBackoffMax
	bg := context.Background()
	if !retryAllowed(bg, spec, 1) {
		t.Fatal("first failure without a caller deadline must be retried")
	}
	if retryAllowed(bg, spec, probeAttempts) {
		t.Fatalf("attempt %d of %d must not be retried", probeAttempts, probeAttempts)
	}
	if retryAllowed(bg, spec, probeAttempts+1) {
		t.Fatal("past the attempt budget must not be retried")
	}
	if retryAllowed(bg, spec, 0) != true {
		t.Fatal("zero attempts made: a run is allowed")
	}
	short, cancel := context.WithTimeout(bg, need-50*time.Millisecond)
	defer cancel()
	if retryAllowed(short, spec, 1) {
		t.Fatal("a retry that cannot finish before the caller's deadline must not start")
	}
	long, cancel2 := context.WithTimeout(bg, need+500*time.Millisecond)
	defer cancel2()
	if !retryAllowed(long, spec, 1) {
		t.Fatal("a caller deadline with room for a whole attempt allows the retry")
	}
	done, cancel3 := context.WithCancel(bg)
	cancel3()
	if retryAllowed(done, spec, 1) {
		t.Fatal("a canceled caller must not be retried")
	}
}

func TestRetryBackoff_JitterStaysInBounds(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := retryBackoff()
		if d < retryBackoffMin || d > retryBackoffMax {
			t.Fatalf("backoff %s outside [%s, %s]", d, retryBackoffMin, retryBackoffMax)
		}
		seen[d] = true
	}
	if len(seen) < 5 {
		t.Fatalf("backoff is not jittered: %d distinct values in 200 draws", len(seen))
	}
}

func TestUnavailableError_NamesThePhaseThatSpentTheDeadline(t *testing.T) {
	res := Result{ProbeID: XIDRunwayProbe, Status: StatusError, Reason: "deadline_exceeded",
		Phase: PhasePoolAcquire, Timing: Timing{Queue: 3 * time.Millisecond,
			Acquire: 1502 * time.Millisecond, Execution: 0, Attempts: 1}}
	_, err := XIDRunwayOf(res)
	if err == nil {
		t.Fatal("an error result decoded as an observation")
	}
	msg := err.Error()
	for _, want := range []string{"probe xid_runway error: deadline_exceeded",
		"in pool_acquire", "queue 3 ms", "pool acquire 1502 ms", "server execution 0 ms",
		"1 attempt"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	ue, ok := err.(*UnavailableError)
	if !ok || ue.Phase != PhasePoolAcquire || ue.Timing.Acquire != 1502*time.Millisecond {
		t.Fatalf("error = %#v, want *UnavailableError carrying phase and timing", err)
	}
}

func TestUnavailableError_WithoutPhaseKeepsTheLegacyText(t *testing.T) {
	err := (&UnavailableError{ProbeID: "x", Status: StatusNoPrivilege,
		Reason: "insufficient_privilege"}).Error()
	if err != "probe x no_privilege: insufficient_privilege" {
		t.Fatalf("error = %q", err)
	}
}

func TestFailedAt_RecordsPhaseAndKeepsTiming(t *testing.T) {
	res := Result{Rows: []Row{{"a": int64(1)}}, Truncated: true,
		Timing: Timing{Acquire: time.Second, Attempts: 2}}
	got := failedAt(res, PhaseExecution, StatusError, "lock_timeout", nil)
	if got.Phase != PhaseLockWait || got.Rows != nil || got.Truncated {
		t.Fatalf("failedAt = %+v, want lock_wait with rows dropped", got)
	}
	if got.Timing.Acquire != time.Second || got.Timing.Attempts != 2 {
		t.Fatalf("timing lost: %+v", got.Timing)
	}
	ok := failedAt(Result{}, PhaseQueue, StatusError, "concurrency_limit", nil)
	if ok.Phase != PhaseQueue || ok.Reason != "concurrency_limit" {
		t.Fatalf("queue failure = %+v", ok)
	}
}

// The phase is a failure annotation: a usable result carries none, and it
// never reaches the JSON evidence of a healthy probe.
func TestResultJSON_PhaseOnlyOnFailure(t *testing.T) {
	okJSON := mustJSON(t, Result{ProbeID: "x", Status: StatusOK,
		Timing: Timing{Acquire: time.Second, Attempts: 1}})
	if strings.Contains(okJSON, "phase") || strings.Contains(okJSON, "timing") ||
		strings.Contains(okJSON, "acquire") {
		t.Fatalf("ok result JSON leaks phase/timing: %s", okJSON)
	}
	failJSON := mustJSON(t, Result{ProbeID: "x", Status: StatusError,
		Reason: "deadline_exceeded", Phase: PhasePoolAcquire})
	if !strings.Contains(failJSON, `"phase":"pool_acquire"`) {
		t.Fatalf("failed result JSON lacks its phase: %s", failJSON)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
