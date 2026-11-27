package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// settleWindow is longer than the reload debounce plus fsnotify delivery, so
// "no onChange within settleWindow" means the watcher decided not to reload.
const settleWindow = 900 * time.Millisecond

// changeRecorder records every onChange candidate and can reject the first n.
type changeRecorder struct {
	mu       sync.Mutex
	values   []int
	rejectN  int
	notified chan struct{}
}

func newChangeRecorder(rejectN int) *changeRecorder {
	return &changeRecorder{rejectN: rejectN, notified: make(chan struct{}, 64)}
}

func (r *changeRecorder) onChange(candidate *Config) error {
	r.mu.Lock()
	r.values = append(r.values, candidate.Collector.IntervalSeconds)
	reject := len(r.values) <= r.rejectN
	r.mu.Unlock()
	r.notified <- struct{}{}
	if reject {
		return errors.New("rejected by test")
	}
	return nil
}

// waitCurrent waits for Current to reach want. The recorder hears onChange
// before the watcher advances Current, which it does only after onChange
// returns, so Current read right after waitCall can still be the old value.
func waitCurrent(t *testing.T, w *Watcher, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := w.Current().Collector.IntervalSeconds
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Current interval = %d %s, want %d", got, what, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *changeRecorder) snapshot() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.values...)
}

func (r *changeRecorder) waitCall(t *testing.T, what string) {
	t.Helper()
	select {
	case <-r.notified:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for onChange: %s (calls=%v)", what, r.snapshot())
	}
}

func (r *changeRecorder) assertCalls(t *testing.T, want []int) {
	t.Helper()
	got := r.snapshot()
	if len(got) != len(want) {
		t.Fatalf("onChange calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("onChange calls = %v, want %v", got, want)
		}
	}
}

func withInterval(base *Config, interval int) *Config {
	cfg := Clone(base)
	cfg.Collector.IntervalSeconds = interval
	return cfg
}

func startRecordedWatcher(
	t *testing.T, path string, initial *Config, rec *changeRecorder,
) *Watcher {
	t.Helper()
	watcher := NewAcknowledgedWatcherWithLoader(path, initial, nil, rec.onChange)
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(watcher.Stop)
	return watcher
}

func requireSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("SKIP: the kubelet ..data symlink swap needs symlink privilege " +
			"and rename-over-directory-symlink, which Windows lacks; runs on Linux CI")
	}
}

// kubeMount mimics the kubelet atomic writer layout of a ConfigMap volume:
// dir/..<ts>/config.yaml, dir/..data -> ..<ts>, dir/config.yaml ->
// ..data/config.yaml.
type kubeMount struct {
	t   *testing.T
	dir string
	gen string
}

func newKubeMount(t *testing.T, cfg *Config) *kubeMount {
	t.Helper()
	m := &kubeMount{t: t, dir: t.TempDir()}
	m.gen = m.writeGeneration("..2026_a", cfg)
	if err := os.Symlink(m.gen, filepath.Join(m.dir, "..data")); err != nil {
		t.Fatalf("symlink ..data: %v", err)
	}
	link := filepath.Join("..data", "config.yaml")
	if err := os.Symlink(link, m.configPath()); err != nil {
		t.Fatalf("symlink config.yaml: %v", err)
	}
	return m
}

func (m *kubeMount) configPath() string { return filepath.Join(m.dir, "config.yaml") }

func (m *kubeMount) writeGeneration(name string, cfg *Config) string {
	m.t.Helper()
	genDir := filepath.Join(m.dir, name)
	if err := os.Mkdir(genDir, 0o755); err != nil {
		m.t.Fatalf("mkdir %s: %v", name, err)
	}
	overwriteWatcherConfig(m.t, filepath.Join(genDir, "config.yaml"), cfg)
	return name
}

// swap does exactly what the atomic writer does: write the new timestamped
// directory, point ..data_tmp at it, rename ..data_tmp over ..data, then
// remove the old generation.
func (m *kubeMount) swap(name string, cfg *Config) {
	m.t.Helper()
	next := m.writeGeneration(name, cfg)
	tmp := filepath.Join(m.dir, "..data_tmp")
	if err := os.Symlink(next, tmp); err != nil {
		m.t.Fatalf("symlink ..data_tmp: %v", err)
	}
	if err := os.Rename(tmp, filepath.Join(m.dir, "..data")); err != nil {
		m.t.Fatalf("rename ..data_tmp over ..data: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(m.dir, m.gen)); err != nil {
		m.t.Fatalf("remove old generation %s: %v", m.gen, err)
	}
	m.gen = next
}

func TestWatcherReloadsOnConfigMapSymlinkSwap(t *testing.T) {
	requireSymlinks(t)
	initial := DefaultConfig()
	mount := newKubeMount(t, initial)
	rec := newChangeRecorder(0)
	watcher := startRecordedWatcher(t, mount.configPath(), initial, rec)

	want := initial.Collector.IntervalSeconds + 7
	mount.swap("..2026_b", withInterval(initial, want))

	rec.waitCall(t, "ConfigMap symlink swap")
	time.Sleep(settleWindow)
	rec.assertCalls(t, []int{want})
	if got := watcher.Current().Collector.IntervalSeconds; got != want {
		t.Fatalf("Current interval = %d, want %d", got, want)
	}
}

func TestWatcherKeepsCurrentWhenDataTargetVanishesMidSwap(t *testing.T) {
	requireSymlinks(t)
	initial := DefaultConfig()
	mount := newKubeMount(t, initial)
	rec := newChangeRecorder(0)
	watcher := startRecordedWatcher(t, mount.configPath(), initial, rec)

	// ..data now dangles: EvalSymlinks fails, the watcher must not panic,
	// must not publish, and must keep the previous config.
	if err := os.RemoveAll(filepath.Join(mount.dir, mount.gen)); err != nil {
		t.Fatalf("remove ..data target: %v", err)
	}
	time.Sleep(settleWindow)
	rec.assertCalls(t, nil)
	if got := watcher.Current().Collector.IntervalSeconds; got !=
		initial.Collector.IntervalSeconds {
		t.Fatalf("Current interval changed to %d on a broken link", got)
	}

	want := initial.Collector.IntervalSeconds + 3
	mount.swap("..2026_c", withInterval(initial, want))
	rec.waitCall(t, "swap after broken link")
	waitCurrent(t, watcher, want, "after good swap")
}

func TestWatcherDebouncesBurstOfWritesIntoOneReload(t *testing.T) {
	initial := DefaultConfig()
	path := writeWatcherConfig(t, initial)
	rec := newChangeRecorder(0)
	watcher := startRecordedWatcher(t, path, initial, rec)

	first := initial.Collector.IntervalSeconds + 1
	second := initial.Collector.IntervalSeconds + 2
	overwriteWatcherConfig(t, path, withInterval(initial, first))
	time.Sleep(100 * time.Millisecond)
	overwriteWatcherConfig(t, path, withInterval(initial, second))

	rec.waitCall(t, "debounced burst")
	time.Sleep(settleWindow)
	rec.assertCalls(t, []int{second})
	if got := watcher.Current().Collector.IntervalSeconds; got != second {
		t.Fatalf("Current interval = %d, want last write %d", got, second)
	}
}

func TestWatcherIgnoresUnrelatedFileInConfigDirectory(t *testing.T) {
	initial := DefaultConfig()
	path := writeWatcherConfig(t, initial)
	rec := newChangeRecorder(0)
	watcher := startRecordedWatcher(t, path, initial, rec)

	swap := filepath.Join(filepath.Dir(path), ".config.yaml.swp")
	if err := os.WriteFile(swap, []byte("collector: {interval_seconds: 1}\n"),
		0o600); err != nil {
		t.Fatalf("write swap file: %v", err)
	}
	time.Sleep(settleWindow)
	rec.assertCalls(t, nil)
	if got := watcher.Current().Collector.IntervalSeconds; got !=
		initial.Collector.IntervalSeconds {
		t.Fatalf("Current interval changed to %d by an unrelated file", got)
	}
}

func TestWatcherSurvivesConfigFileRemovalAndReloadsWhenRestored(t *testing.T) {
	initial := DefaultConfig()
	path := writeWatcherConfig(t, initial)
	rec := newChangeRecorder(0)
	watcher := startRecordedWatcher(t, path, initial, rec)

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	time.Sleep(settleWindow)
	rec.assertCalls(t, nil)

	want := initial.Collector.IntervalSeconds + 4
	overwriteWatcherConfig(t, path, withInterval(initial, want))
	rec.waitCall(t, "restored config file")
	waitCurrent(t, watcher, want, "after restore")
}

func TestWatcherDoesNotRetryRejectedFileUntilItChanges(t *testing.T) {
	initial := DefaultConfig()
	path := writeWatcherConfig(t, initial)
	rec := newChangeRecorder(1)
	watcher := startRecordedWatcher(t, path, initial, rec)

	want := initial.Collector.IntervalSeconds + 5
	overwriteWatcherConfig(t, path, withInterval(initial, want))
	rec.waitCall(t, "first candidate")
	time.Sleep(settleWindow)
	rec.assertCalls(t, []int{want})
	if got := watcher.Current().Collector.IntervalSeconds; got !=
		initial.Collector.IntervalSeconds {
		t.Fatalf("rejected candidate became current: %d", got)
	}

	// An identical rewrite that leaves the fingerprint (bytes, size, mtime)
	// unchanged is a no-op event and must not re-run the rejected candidate.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	overwriteWatcherConfig(t, path, withInterval(initial, want))
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}
	time.Sleep(settleWindow)
	rec.assertCalls(t, []int{want})

	// touch: same bytes, newer mtime is the operator's retry request.
	touched := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, touched, touched); err != nil {
		t.Fatalf("touch: %v", err)
	}
	rec.waitCall(t, "retry after touch")
	rec.assertCalls(t, []int{want, want})
	waitCurrent(t, watcher, want, "after accepted retry")
}

func TestWatcherStopWhileDebounceArmedFiresNoReload(t *testing.T) {
	initial := DefaultConfig()
	path := writeWatcherConfig(t, initial)
	rec := newChangeRecorder(0)
	watcher := NewAcknowledgedWatcherWithLoader(path, initial, nil, rec.onChange)
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	overwriteWatcherConfig(t, path,
		withInterval(initial, initial.Collector.IntervalSeconds+6))
	time.Sleep(50 * time.Millisecond) // event delivered, debounce armed
	stopped := make(chan struct{})
	go func() {
		watcher.Stop()
		close(stopped)
	}()
	waitClosed(t, stopped, "Stop with armed debounce")
	waitClosed(t, watcher.Done(), "watcher shutdown")
	time.Sleep(settleWindow)
	rec.assertCalls(t, nil)
	if got := watcher.Current().Collector.IntervalSeconds; got !=
		initial.Collector.IntervalSeconds {
		t.Fatalf("Current interval changed to %d after Stop", got)
	}
}

func TestWatcherConcurrentStopDuringEventStormIsClean(t *testing.T) {
	initial := DefaultConfig()
	path := writeWatcherConfig(t, initial)
	rec := newChangeRecorder(0)
	watcher := NewAcknowledgedWatcherWithLoader(path, initial, nil, rec.onChange)
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	quit := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 1; ; i++ {
			select {
			case <-quit:
				return
			default:
			}
			data := []byte(fmt.Sprintf("collector:\n  interval_seconds: %d\n", 100+i))
			// A write may race the reader on Windows; only the watcher's
			// behaviour is under test, so a failed write is not fatal.
			_ = os.WriteFile(path, data, 0o600)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	time.Sleep(150 * time.Millisecond)
	stopped := make(chan struct{})
	go func() {
		watcher.Stop()
		close(stopped)
	}()
	waitClosed(t, stopped, "Stop during event storm")
	afterStop := rec.snapshot()
	close(quit)
	waitClosed(t, writerDone, "writer exit")
	waitClosed(t, watcher.Done(), "watcher shutdown")
	time.Sleep(settleWindow)
	rec.assertCalls(t, afterStop)
}

func TestWatcherStartedBeforeFileExistsReloadsFirstVersion(t *testing.T) {
	initial := DefaultConfig()
	path := filepath.Join(t.TempDir(), "config.yaml")
	rec := newChangeRecorder(0)
	watcher := startRecordedWatcher(t, path, initial, rec)

	want := initial.Collector.IntervalSeconds + 8
	overwriteWatcherConfig(t, path, withInterval(initial, want))
	rec.waitCall(t, "first version of a late config file")
	rec.assertCalls(t, []int{want})
	waitCurrent(t, watcher, want, "for the late file")
}
