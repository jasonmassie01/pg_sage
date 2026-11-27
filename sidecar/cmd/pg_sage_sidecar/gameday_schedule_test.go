package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// benches-01: a fleet's runtimes start together, so their game days must
// not: each runtime's first check waits a delay of its own (from its
// database name), never less than gameDayMinStartDelay.

// Boundary: a fleet of three starts no game day in the first minute.
func TestGameDayStartDelayStartsNoneInTheFirstMinute(t *testing.T) {
	seen := map[time.Duration]bool{}
	for _, db := range []string{"orders", "billing", "users"} {
		d := gameDayStartDelay(db, gameDayCheckInterval)
		if d < time.Minute || d < gameDayMinStartDelay || d >= gameDayCheckInterval {
			t.Fatalf("%s: first game-day check after %s, want [%s, %s)", db, d,
				gameDayMinStartDelay, gameDayCheckInterval)
		}
		if again := gameDayStartDelay(db, gameDayCheckInterval); again != d {
			t.Fatalf("%s: delay %s then %s, want one per database", db, d, again)
		}
		seen[d] = true
	}
	if len(seen) != 3 {
		t.Fatalf("three databases share start delays: %v", seen)
	}
}

// Many names stay inside the window and spread across it.
func TestGameDayStartDelayStaysInTheWindow(t *testing.T) {
	var early, late int
	for i := 0; i < 500; i++ {
		d := gameDayStartDelay(fmt.Sprintf("db-%d", i), gameDayCheckInterval)
		if d < gameDayMinStartDelay || d >= gameDayCheckInterval {
			t.Fatalf("db-%d: delay %s outside [%s, %s)", i, d, gameDayMinStartDelay,
				gameDayCheckInterval)
		}
		if d < gameDayCheckInterval/2 {
			early++
		} else {
			late++
		}
	}
	if early < 100 || late < 100 {
		t.Fatalf("delays are not spread: %d early, %d late", early, late)
	}
}

// Zero and tiny intervals: no negative or out-of-range delay.
func TestGameDayStartDelayBoundaries(t *testing.T) {
	if d := gameDayStartDelay("orders", 0); d != 0 {
		t.Fatalf("zero interval: delay %s, want 0", d)
	}
	if d := gameDayStartDelay("orders", time.Minute); d != time.Minute {
		t.Fatalf("an interval under the minimum delay: %s, want the interval", d)
	}
	if d := gameDayStartDelay("", gameDayCheckInterval); d < gameDayMinStartDelay ||
		d >= gameDayCheckInterval {
		t.Fatalf("empty database name: delay %s", d)
	}
}

// everyAfter does not run before its delay, runs at it, and never runs
// once ctx ends first.
func TestEveryAfterWaitsForTheDelay(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		everyAfter(ctx, 300*time.Millisecond, time.Hour, func(context.Context) {
			calls.Add(1)
		})
	}()
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("ran %d times before the delay", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("ran %d times after the delay, want 1", n)
	}
	cancel()
	<-done

	var never atomic.Int32
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	everyAfter(ctx2, time.Hour, time.Hour, func(context.Context) { never.Add(1) })
	if never.Load() != 0 {
		t.Fatal("ran after its context ended")
	}
}
