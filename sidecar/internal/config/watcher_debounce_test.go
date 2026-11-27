package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// No concurrent access tests for debouncer: it is owned by one goroutine.

func TestDebouncerArmBoundsQuietPeriodByMaxWait(t *testing.T) {
	d := newDebouncer()
	defer d.stop()
	start := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name  string
		after time.Duration
		want  time.Duration
	}{
		{"first event", 0, reloadDebounce},
		{"mid burst", time.Second, reloadDebounce},
		{"exactly debounce before max", reloadMaxWait - reloadDebounce, reloadDebounce},
		{"inside last debounce", reloadMaxWait - 100*time.Millisecond,
			100 * time.Millisecond},
		{"at max wait", reloadMaxWait, 0},
		{"past max wait", reloadMaxWait + time.Second, 0},
	}
	for _, tc := range cases {
		if got := d.arm(start.Add(tc.after)); got != tc.want {
			t.Fatalf("%s: arm wait = %v, want %v", tc.name, got, tc.want)
		}
	}
	d.fired()
	later := start.Add(10 * time.Second)
	if got := d.arm(later); got != reloadDebounce {
		t.Fatalf("arm after fired = %v, want a fresh %v", got, reloadDebounce)
	}
	if got := d.arm(later.Add(reloadMaxWait)); got != 0 {
		t.Fatalf("new burst max wait not measured from its first event: %v", got)
	}
}

func TestDebouncerFiresOnceAfterQuietPeriod(t *testing.T) {
	d := newDebouncer()
	defer d.stop()
	select {
	case <-d.timer.C:
		t.Fatal("an unarmed debouncer fired")
	case <-time.After(reloadDebounce + 100*time.Millisecond):
	}
	armed := time.Now()
	d.arm(armed)
	select {
	case <-d.timer.C:
		if elapsed := time.Since(armed); elapsed < reloadDebounce-10*time.Millisecond {
			t.Fatalf("fired after %v, before the %v quiet period", elapsed,
				reloadDebounce)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("armed debouncer never fired")
	}
}

// A process writing a file next to config.yaml (shell-redirected logs) must
// not postpone a config reload for as long as it keeps writing.
func TestWatcherReloadsDuringSteadyStreamOfUnrelatedEvents(t *testing.T) {
	initial := DefaultConfig()
	path := writeWatcherConfig(t, initial)
	rec := newChangeRecorder(0)
	watcher := startRecordedWatcher(t, path, initial, rec)

	noise := filepath.Join(filepath.Dir(path), "sidecar.log")
	quit := make(chan struct{})
	noiseDone := make(chan struct{})
	go func() {
		defer close(noiseDone)
		for i := 0; ; i++ {
			select {
			case <-quit:
				return
			case <-time.After(50 * time.Millisecond):
			}
			line := []byte(fmt.Sprintf("log line %d\n", i))
			if err := os.WriteFile(noise, line, 0o600); err != nil {
				t.Errorf("write noise file: %v", err)
				return
			}
		}
	}()
	defer func() {
		close(quit)
		<-noiseDone
	}()

	time.Sleep(100 * time.Millisecond)
	want := initial.Collector.IntervalSeconds + 9
	overwriteWatcherConfig(t, path, withInterval(initial, want))
	deadline := 2*reloadMaxWait + time.Second
	select {
	case <-rec.notified:
	case <-time.After(deadline):
		t.Fatalf("no reload within %v while unrelated events kept arriving "+
			"(calls=%v)", deadline, rec.snapshot())
	}
	rec.assertCalls(t, []int{want})
	waitCurrent(t, watcher, want, "under unrelated events")
}
