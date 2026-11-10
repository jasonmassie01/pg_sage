package main

import (
	"context"
	"hash/fnv"
	"time"
)

// gameDayMinStartDelay is the least a runtime waits before its first
// game-day check, so a fleet that starts together starts no game day at
// once (benches-01).
const gameDayMinStartDelay = 5 * time.Minute

// gameDayStartDelay spreads the runtimes' first game-day checks over one
// check interval: gameDayMinStartDelay plus a hash of the database name,
// below interval. An interval no longer than the minimum is the delay.
func gameDayStartDelay(database string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	if interval <= gameDayMinStartDelay {
		return interval
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(database)) // hash.Hash writes never fail
	span := uint64(interval - gameDayMinStartDelay)
	return gameDayMinStartDelay + time.Duration(h.Sum64()%span)
}

// everyAfter waits delay, then runs fn at once and every interval until
// ctx ends; it never runs fn when ctx ends first.
func everyAfter(ctx context.Context, delay, interval time.Duration,
	fn func(context.Context)) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	if ctx.Err() != nil {
		return
	}
	every(ctx, interval, fn)
}
