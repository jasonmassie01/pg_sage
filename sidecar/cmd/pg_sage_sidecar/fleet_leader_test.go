package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/leader"
)

// The fleet elector is set after the database runtimes start, while their
// loops (pressure enforcement every 10 s) read it. These tests pin the
// leader gate's states and that the set never races with a loop's read
// (run under -race on Linux; ci-19).

func keepFleetLeader(t *testing.T) {
	t.Helper()
	prev := currentFleetLeader()
	t.Cleanup(func() { setFleetLeader(prev) })
}

func tickedStubElector(t *testing.T, held bool) *leader.Elector {
	t.Helper()
	e := leader.NewElector(stubLease{held: held}, "s", "me", time.Minute)
	if err := e.Tick(context.Background()); err != nil {
		t.Fatalf("tick the stub elector: %v", err)
	}
	return e
}

func TestMayProposeFollowsTheFleetLeader(t *testing.T) {
	keepFleetLeader(t)
	rt := &databaseRuntime{}

	setFleetLeader(nil)
	if !rt.mayPropose() {
		t.Fatal("election off: a writable primary must propose")
	}
	setFleetLeader(tickedStubElector(t, false))
	if rt.mayPropose() {
		t.Fatal("a fleet follower proposed a pressure cancel")
	}
	setFleetLeader(tickedStubElector(t, true))
	if !rt.mayPropose() {
		t.Fatal("the fleet leader on a writable primary must propose")
	}
	setFleetLeader(nil)
	if currentFleetLeader() != nil || !rt.mayPropose() {
		t.Fatal("turning election off again must restore proposing")
	}
}

func TestFleetLeaderSetWhileRuntimeLoopsRead(t *testing.T) {
	keepFleetLeader(t)
	follower, holder := tickedStubElector(t, false), tickedStubElector(t, true)
	rt := &databaseRuntime{}
	stop := make(chan struct{})
	var wg, started sync.WaitGroup
	for range 4 {
		wg.Add(1)
		started.Add(1)
		go func() {
			defer wg.Done()
			first := true
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = rt.mayPropose()
				_ = currentFleetLeader().IsLeader()
				if first {
					started.Done()
					first = false
				}
			}
		}()
	}
	started.Wait() // every reader is in its loop before the sets begin
	for i := 0; i < 200; i++ {
		setFleetLeader([]*leader.Elector{nil, follower, holder}[i%3])
	}
	setFleetLeader(follower)
	close(stop)
	wg.Wait()
	if currentFleetLeader() != follower || rt.mayPropose() {
		t.Fatal("the last set elector (a follower) must decide the gate")
	}
}
