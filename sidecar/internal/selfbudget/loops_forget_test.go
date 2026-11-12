package selfbudget

import (
	"testing"
	"time"
)

// A database that leaves the fleet takes its loops (and its LLM wait)
// with it; process loops and other databases' loops stay.
// No concurrent access tests beyond the tracker's mutex, which
// ForgetDatabase takes like every other method.

func TestLoops_ForgetDatabaseDropsOnlyItsLoops(t *testing.T) {
	l := NewLoops()
	l.Add(LoopName("collector", "orders"), time.Second)
	l.Add(LoopName(LLMWaitLoop, "orders"), time.Second)
	l.Add(LoopName("collector", "orders:eu"), time.Second) // a name with a colon
	l.Add(LoopName("collector", "billing"), time.Second)
	l.Add(LoopName("self_config", ""), time.Second)
	l.ForgetDatabase("orders")
	snap := l.Snapshot()
	for _, gone := range []string{"collector:orders", "llm_wait:orders"} {
		if _, ok := snap[gone]; ok {
			t.Errorf("%s survived forgetting orders", gone)
		}
	}
	for _, kept := range []string{"collector:orders:eu", "collector:billing", "self_config"} {
		if snap[kept].Runs != 1 {
			t.Errorf("%s = %+v, want its one run kept", kept, snap[kept])
		}
	}
}

// Nil/empty: an empty name never forgets the process loops; a nil tracker
// does nothing.
func TestLoops_ForgetDatabaseEmptyAndNil(t *testing.T) {
	l := NewLoops()
	l.Add("analyzer", time.Second)
	l.ForgetDatabase("")
	if l.Snapshot()["analyzer"].Runs != 1 {
		t.Fatal("an empty database name forgot a process loop")
	}
	var none *Loops
	none.ForgetDatabase("orders")
}

// State transition: a forgotten loop that runs again counts from zero.
func TestLoops_ForgottenLoopStartsOver(t *testing.T) {
	l := NewLoops()
	l.TrackErr(LoopName("collector", "orders"))(errCycle)
	l.ForgetDatabase("orders")
	l.TrackErr(LoopName("collector", "orders"))(nil)
	s := l.Snapshot()["collector:orders"]
	if s.Runs != 1 || s.Errors != 0 || s.LastSuccess.IsZero() {
		t.Fatalf("after forgetting = %+v, want one fresh successful run", s)
	}
}
