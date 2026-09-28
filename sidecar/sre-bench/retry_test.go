package srebench

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// A scenario whose premise was broken by the environment (another
// session wrote WAL during a "steady" window) is run again rather than
// scored; other failures are not retried, and the attempts are bounded.

func TestRetryContaminated_RerunsUntilClean(t *testing.T) {
	calls := 0
	r := retryContaminated(3, func() Result {
		calls++
		if calls < 3 {
			return Result{Err: &Contaminated{Reason: "foreign WAL"}}
		}
		return Result{Outcome: Outcome{Root: "x"}}
	})
	if calls != 3 || r.Err != nil || r.Attempts != 3 || r.Outcome.Root != "x" {
		t.Fatalf("calls=%d result=%+v", calls, r)
	}
}

func TestRetryContaminated_GivesUpWithTheLastReason(t *testing.T) {
	calls := 0
	r := retryContaminated(2, func() Result {
		calls++
		return Result{Err: &Contaminated{Reason: fmt.Sprintf("try %d", calls)}}
	})
	var c *Contaminated
	if calls != 2 || r.Attempts != 2 || !errors.As(r.Err, &c) || c.Reason != "try 2" ||
		scored(r) {
		t.Fatalf("calls=%d result=%+v", calls, r)
	}
}

func TestRetryContaminated_DoesNotRetryOtherOutcomes(t *testing.T) {
	for name, res := range map[string]Result{
		"error":   {Err: errors.New("inject: boom")},
		"skipped": {Skipped: "no archive fixture"},
		"scored":  {Outcome: Outcome{Root: "write_surge"}},
	} {
		calls := 0
		r := retryContaminated(3, func() Result { calls++; return res })
		if calls != 1 || r.Attempts != 1 || r.Skipped != res.Skipped ||
			r.Outcome.Root != res.Outcome.Root || !errors.Is(r.Err, res.Err) {
			t.Fatalf("%s: calls=%d result=%+v", name, calls, r)
		}
	}
}

func TestRetryContaminated_ZeroAttemptsRunsOnce(t *testing.T) {
	calls := 0
	r := retryContaminated(0, func() Result { calls++; return Result{} })
	if calls != 1 || r.Attempts != 1 {
		t.Fatalf("calls=%d attempts=%d", calls, r.Attempts)
	}
}

func TestContaminated_ErrorNamesTheReason(t *testing.T) {
	err := error(&Contaminated{Reason: "5 MiB/s of foreign WAL"})
	if err.Error() != "environment contaminated: 5 MiB/s of foreign WAL" {
		t.Fatalf("got %q", err.Error())
	}
}

func TestQuietWindow_Boundaries(t *testing.T) {
	sec := time.Second
	cases := []struct {
		name         string
		bytes        float64
		elapsed      time.Duration
		contaminated bool
		fails        bool
	}{
		{"idle", 0, 3 * sec, false, false},
		{"just under", quietWALRate*3 - 1, 3 * sec, false, false},
		{"at the rate", quietWALRate * 3, 3 * sec, true, true},
		{"surge", 64 << 20, 3 * sec, true, true},
		{"unmeasured", 0, 0, false, true},
		{"negative clock", 0, -sec, false, true},
	}
	for _, c := range cases {
		err := quietWindow(c.bytes, c.elapsed)
		var ct *Contaminated
		if (err != nil) != c.fails || errors.As(err, &ct) != c.contaminated {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}
