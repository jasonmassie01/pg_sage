package config

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestWatcherStopIsConcurrentAndIdempotent(t *testing.T) {
	path := writeWatcherConfig(t, DefaultConfig())
	watcher := NewWatcher(path, DefaultConfig(), nil)
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var callers sync.WaitGroup
	for i := 0; i < 32; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			watcher.Stop()
		}()
	}
	waitGroup(t, &callers, "concurrent Stop calls")
	waitClosed(t, watcher.Done(), "watcher shutdown")
	watcher.Stop()
	if err := watcher.Start(); !errors.Is(err, ErrWatcherStopped) {
		t.Fatalf("Start after Stop error = %v, want ErrWatcherStopped", err)
	}
}

func TestWatcherStopBeforeStartIsSafe(t *testing.T) {
	watcher := NewWatcher(writeWatcherConfig(t, DefaultConfig()), DefaultConfig(), nil)
	watcher.Stop()
	watcher.Stop()
	waitClosed(t, watcher.Done(), "pre-start shutdown")
	if err := watcher.Start(); !errors.Is(err, ErrWatcherStopped) {
		t.Fatalf("Start after pre-start Stop error = %v, want ErrWatcherStopped", err)
	}
}

func TestWatcherRejectsSecondStart(t *testing.T) {
	watcher := NewWatcher(writeWatcherConfig(t, DefaultConfig()), DefaultConfig(), nil)
	if err := watcher.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := watcher.Start(); !errors.Is(err, ErrWatcherStarted) {
		t.Fatalf("second Start error = %v, want ErrWatcherStarted", err)
	}
	watcher.Stop()
	waitClosed(t, watcher.Done(), "watcher shutdown")
}

func TestWatcherStopCannotLoseCancellationDuringBlockedCallback(t *testing.T) {
	initial := DefaultConfig()
	path := writeWatcherConfig(t, initial)
	entered := make(chan struct{})
	release := make(chan struct{})
	var callbackOnce sync.Once
	watcher := NewWatcher(path, initial, func(*Config) {
		callbackOnce.Do(func() { close(entered) })
		<-release
	})
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	updated := Clone(initial)
	updated.Collector.IntervalSeconds++
	overwriteWatcherConfig(t, path, updated)
	waitClosed(t, entered, "reload callback")
	stopped := make(chan struct{})
	go func() {
		watcher.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned before the watcher goroutine exited")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	waitClosed(t, stopped, "Stop return")
	waitClosed(t, watcher.Done(), "watcher shutdown")
}

func TestWatcherCandidateLoaderPublishesCompletePrecedenceResolvedSnapshot(
	t *testing.T,
) {
	initial := DefaultConfig()
	loaded := Clone(initial)
	loaded.Postgres.Host = "cli-wins.example"
	loaded.Collector.IntervalSeconds++
	var published *Config
	watcher := NewWatcherWithLoader(
		"config.yaml", initial,
		func() (*Config, error) { return Clone(loaded), nil },
		func(candidate *Config) { published = candidate },
	)

	watcher.reload()

	if published == nil {
		t.Fatal("complete candidate was not published")
	}
	if published.Postgres.Host != "cli-wins.example" {
		t.Fatalf("restart field = %q, want precedence-resolved value",
			published.Postgres.Host)
	}
	if published.Collector.IntervalSeconds != loaded.Collector.IntervalSeconds {
		t.Fatalf("collector interval = %d, want %d",
			published.Collector.IntervalSeconds, loaded.Collector.IntervalSeconds)
	}
}

func TestWatcherAdvancesCurrentOnlyAfterCallbackAcceptsCandidate(t *testing.T) {
	initial := DefaultConfig()
	candidate := Clone(initial)
	candidate.Collector.IntervalSeconds++
	calls := 0
	watcher := NewAcknowledgedWatcherWithLoader(
		"config.yaml", initial,
		func() (*Config, error) { return Clone(candidate), nil },
		func(*Config) error {
			calls++
			if calls == 1 {
				return errors.New("stale generation")
			}
			return nil
		},
	)

	watcher.reload()
	if got := watcher.Current().Collector.IntervalSeconds; got != initial.Collector.IntervalSeconds {
		t.Fatalf("rejected candidate became current: %d", got)
	}
	watcher.reload()
	if calls != 2 {
		t.Fatalf("callback calls = %d, want retry", calls)
	}
	if got := watcher.Current().Collector.IntervalSeconds; got != candidate.Collector.IntervalSeconds {
		t.Fatalf("accepted candidate not current: %d", got)
	}
}

func writeWatcherConfig(t *testing.T, cfg *Config) string {
	t.Helper()
	path := t.TempDir() + string(os.PathSeparator) + "config.yaml"
	overwriteWatcherConfig(t, path, cfg)
	return path
}

func overwriteWatcherConfig(t *testing.T, path string, cfg *Config) {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func waitGroup(t *testing.T, group *sync.WaitGroup, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	waitClosed(t, done, what)
}
