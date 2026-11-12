package cloudtel

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRetryThrottledHonoursRetryAfterAndStopsOnOtherErrors(t *testing.T) {
	sleep := &recordSleep{}
	calls := 0
	raw, err := retryThrottled(context.Background(), sleep.sleep,
		func() ([]byte, time.Duration, error) {
			calls++
			if calls == 1 {
				return nil, 30 * time.Second, fmt.Errorf("%w: 429", ErrThrottled)
			}
			return []byte("ok"), 0, nil
		})
	if err != nil || string(raw) != "ok" || calls != 2 {
		t.Fatalf("raw=%q err=%v calls=%d", raw, err, calls)
	}
	if len(sleep.waits) != 1 || sleep.waits[0] != maxRetryAfter {
		t.Fatalf("waits = %v, want Retry-After capped at %s", sleep.waits, maxRetryAfter)
	}
	calls = 0
	_, err = retryThrottled(context.Background(), sleep.sleep,
		func() ([]byte, time.Duration, error) {
			calls++
			return nil, 0, fmt.Errorf("%w: 403", ErrAuth)
		})
	if !errors.Is(err, ErrAuth) || calls != 1 {
		t.Fatalf("a denial must not be retried: err=%v calls=%d", err, calls)
	}
}

func TestRetryThrottledStopsWhenTheSleepIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := retryThrottled(ctx, sleepContext, func() ([]byte, time.Duration, error) {
		calls++
		return nil, 0, ErrThrottled
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err=%v calls=%d, want the cancellation after one call", err, calls)
	}
}

func TestRetryAfterParsesSeconds(t *testing.T) {
	for in, want := range map[string]time.Duration{"3": 3 * time.Second, " 1 ": time.Second,
		"": 0, "0": 0, "-2": 0, "Wed, 21 Oct 2026 07:28:00 GMT": 0} {
		if got := retryAfter(in); got != want {
			t.Errorf("retryAfter(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestAzureMalformedReplicaIDAndTimestamps(t *testing.T) {
	f := newFakeARM(t).healthy()
	f.body["replicas"] = `{"value":[{"id":"/subscriptions/x/../../evil"}]}`
	if _, err := f.source(t, nil, nil).Collect(context.Background(),
		azureNow); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bad replica id: err = %v, want ErrMalformed", err)
	}
	f = newFakeARM(t).healthy()
	f.body["metrics:orders"] = `{"value":[{"name":{"value":"cpu_percent"},` +
		`"timeseries":[{"data":[{"timeStamp":"yesterday","average":5}]}]}]}`
	if _, err := f.source(t, nil, nil).Collect(context.Background(),
		azureNow); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bad timestamp: err = %v, want ErrMalformed", err)
	}
	f = newFakeARM(t).healthy()
	f.metrics["orders"]["cpu_percent"] = []agoPoint{{value: 5, ago: -MaxClockSkew - time.Minute}}
	if _, err := f.source(t, nil, nil).Collect(context.Background(),
		azureNow); !errors.Is(err, ErrClockSkew) {
		t.Fatalf("future point: err = %v, want ErrClockSkew", err)
	}
}
