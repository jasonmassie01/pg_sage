package main

import (
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// env-race: logwatchPollInterval read cfg.LogWatch.PollIntervalMs without
// the hot-reload lock that a dashboard edit (PUT /config,
// logwatch.poll_interval_ms) writes it under, from goroutines a runtime
// start launches. It now reads under config.RLockForHotReload.

func TestRace_Cfg_LogwatchPollIntervalDuringHotReload(t *testing.T) {
	preserveValueGlobals(t)
	cfg = config.DefaultConfig()
	cfg.LogWatch.PollIntervalMs = 100
	stop, started := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			config.LockForHotReload()
			cfg.LogWatch.PollIntervalMs = 100 + (i%2)*100
			config.UnlockForHotReload()
			if i == 0 {
				close(started) // the reads below overlap the later writes
			}
		}
	}()
	<-started
	for i := 0; i < 2000; i++ {
		got := logwatchPollInterval()
		if got != 100*time.Millisecond && got != 200*time.Millisecond {
			close(stop)
			wg.Wait()
			t.Fatalf("read %d: interval = %v, want 100ms or 200ms", i, got)
		}
	}
	close(stop)
	wg.Wait()
}

func TestLogwatchPollIntervalBoundaries(t *testing.T) {
	preserveValueGlobals(t)
	for _, tc := range []struct {
		ms   int
		want time.Duration
	}{
		{ms: -5, want: time.Second},
		{ms: 0, want: time.Second},
		{ms: 1, want: time.Millisecond},
		{ms: 250, want: 250 * time.Millisecond},
	} {
		cfg = config.DefaultConfig()
		cfg.LogWatch.PollIntervalMs = tc.ms
		if got := logwatchPollInterval(); got != tc.want {
			t.Errorf("poll_interval_ms=%d: interval = %v, want %v", tc.ms, got, tc.want)
		}
	}
}
