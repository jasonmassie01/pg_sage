package pgssepoch

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// epochs returns a fake Epoch that yields the given values in order.
func epochs(values ...string) func() (string, error) {
	i := 0
	return func() (string, error) {
		if i >= len(values) {
			return "", fmt.Errorf("epoch read %d beyond the script", i)
		}
		v := values[i]
		i++
		return v, nil
	}
}

func TestRun_CleanAttemptPasses(t *testing.T) {
	calls := 0
	res := run(epochs("a", "a"), 3, func() []string { calls++; return nil })
	if calls != 1 || res.problems != nil || res.err != nil || len(res.notes) != 0 {
		t.Fatalf("calls=%d res=%+v", calls, res)
	}
}

func TestRun_FailureWithoutResetIsReportedAtOnce(t *testing.T) {
	calls := 0
	res := run(epochs("a", "a", "a", "a"), 3, func() []string {
		calls++
		return []string{"missing"}
	})
	if calls != 1 || len(res.problems) != 1 || res.problems[0] != "missing" || res.err != nil {
		t.Fatalf("a real failure must not be retried: calls=%d res=%+v", calls, res)
	}
}

func TestRun_ResetDuringAttemptRepeats(t *testing.T) {
	calls := 0
	res := run(epochs("a", "b", "b", "b"), 3, func() []string {
		calls++
		if calls == 1 {
			return []string{"erased"}
		}
		return nil
	})
	if calls != 2 || res.problems != nil || res.err != nil || len(res.notes) != 1 {
		t.Fatalf("calls=%d res=%+v", calls, res)
	}
	if !strings.Contains(res.notes[0], "a -> b") {
		t.Fatalf("note must name the epochs: %q", res.notes[0])
	}
}

func TestRun_ResetEveryAttemptFails(t *testing.T) {
	res := run(epochs("a", "b", "c", "d", "e", "f"), 3, func() []string {
		return []string{"erased"}
	})
	if res.err == nil || !strings.Contains(res.err.Error(), "each of 3 attempts") ||
		!strings.Contains(res.err.Error(), "[erased]") {
		t.Fatalf("want an error after 3 disturbed attempts, got %+v", res)
	}
}

func TestRun_UnavailableBeforeAttemptSkips(t *testing.T) {
	calls := 0
	boom := errors.New("relation \"pg_stat_statements_info\" does not exist")
	res := run(func() (string, error) { return "", boom }, 3,
		func() []string { calls++; return nil })
	if calls != 0 || !errors.Is(res.unavailable, boom) {
		t.Fatalf("calls=%d res=%+v", calls, res)
	}
}

func TestRun_ErrorAfterAttemptFails(t *testing.T) {
	n := 0
	res := run(func() (string, error) {
		n++
		if n == 2 {
			return "", errors.New("connection refused")
		}
		return "a", nil
	}, 3, func() []string { return nil })
	if res.err == nil || !strings.Contains(res.err.Error(), "connection refused") {
		t.Fatalf("res=%+v", res)
	}
}

func TestRun_ZeroAttemptsFails(t *testing.T) {
	if res := run(epochs(), 0, func() []string { return nil }); res.err == nil {
		t.Fatalf("zero attempts must fail, got %+v", res)
	}
}
