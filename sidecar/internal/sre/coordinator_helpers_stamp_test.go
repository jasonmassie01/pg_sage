package sre

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The scripted runner's stamps (forecast-11): on Windows two time.Now()
// readings close together can be equal, and the causal graph rightly
// refuses two samples of one instant. The fixture's back-to-back calls
// stand in for samples taken a sample delay apart, so each stamp is
// strictly later than the last and still near the present.

func TestScriptedRunner_StampsStrictlyIncrease(t *testing.T) {
	r := newScriptedRunner().script(probes.ConnectionSaturation,
		probes.Result{ProbeID: probes.ConnectionSaturation, Status: probes.StatusOK})
	ctx := context.Background()
	start := time.Now()
	var prev time.Time
	for i := 0; i < 50; i++ {
		id := probes.ConnectionSaturation
		if i%2 == 1 {
			id = probes.LockGraph // unscripted: the empty answer is stamped too
		}
		got := r.Run(ctx, id, probes.Args{}).ObservedAt
		if !got.After(prev) {
			t.Fatalf("call %d stamped %v, not after %v", i, got, prev)
		}
		prev = got
	}
	// 50 calls are at most 50 ms ahead of the host clock: still fresh.
	if ahead := prev.Sub(time.Now()); ahead > 50*time.Millisecond {
		t.Fatalf("the last stamp is %v ahead of now", ahead)
	}
	if prev.Before(start) {
		t.Fatalf("the last stamp %v is before the test began %v", prev, start)
	}
}

// Concurrent calls from several goroutines still get distinct stamps.
func TestScriptedRunner_ConcurrentStampsAreDistinct(t *testing.T) {
	r := newScriptedRunner()
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[time.Time]bool{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			at := r.Run(context.Background(), probes.ConnectionSaturation, probes.Args{}).ObservedAt
			mu.Lock()
			defer mu.Unlock()
			if seen[at] {
				t.Errorf("stamp %v given twice", at)
			}
			seen[at] = true
		}()
	}
	wg.Wait()
	if len(seen) != 8 {
		t.Fatalf("%d distinct stamps from 8 calls", len(seen))
	}
}
