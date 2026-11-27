package ask

import "testing"

// Removing a database from the fleet forgets its answer counters (and only
// its own), so its pg_sage_ask_answers_total series leave /metrics.
// No concurrent access tests beyond the mutex: ForgetDatabase and
// countAnswer share answerCounters.mu, which countAnswer already uses.

func resetAnswerCounters(t *testing.T) {
	t.Helper()
	answerCounters.mu.Lock()
	saved := answerCounters.n
	answerCounters.n = map[answerKey]uint64{}
	answerCounters.mu.Unlock()
	t.Cleanup(func() {
		answerCounters.mu.Lock()
		answerCounters.n = saved
		answerCounters.mu.Unlock()
	})
}

func TestForgetDatabase_DropsOnlyThatDatabase(t *testing.T) {
	resetAnswerCounters(t)
	countAnswer("orders", Answer{Status: "answered", Stop: "final"})
	countAnswer("orders", Answer{Status: "incomplete", Stop: "wall_clock"})
	countAnswer("billing", Answer{Status: "answered", Stop: "final"})
	ForgetDatabase("orders")
	got := AnswerCounts()
	if len(got) != 1 || got[0].Database != "billing" || got[0].Count != 1 {
		t.Fatalf("after forgetting orders = %+v, want only billing's one answer", got)
	}
}

// Nil/empty: an empty name and an unknown database forget nothing.
func TestForgetDatabase_EmptyAndUnknownForgetNothing(t *testing.T) {
	resetAnswerCounters(t)
	countAnswer("", Answer{Status: "answered", Stop: "final"})
	countAnswer("orders", Answer{Status: "answered", Stop: "final"})
	ForgetDatabase("")
	ForgetDatabase("missing")
	if got := AnswerCounts(); len(got) != 2 {
		t.Fatalf("counters = %+v, want both kept", got)
	}
}
