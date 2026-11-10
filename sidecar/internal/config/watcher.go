package config

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

var (
	ErrWatcherStarted = errors.New("config watcher already started")
	ErrWatcherStopped = errors.New("config watcher already stopped")
)

type watcherState uint8

const (
	watcherNew watcherState = iota
	watcherStarted
	watcherStopping
	watcherStopped
)

// Watcher monitors config.yaml for changes and calls onChange with
// the hot-reloadable values updated. Non-hot-reloadable values
// (postgres connection, listen addresses) are ignored on reload.
type Watcher struct {
	path     string
	mu       sync.RWMutex
	current  *Config
	loader   func() (*Config, error)
	onChange func(*Config) error
	// lastAttempt is owned by Start and then by the run goroutine.
	lastAttempt fileFingerprint

	lifecycleMu sync.Mutex
	state       watcherState
	stop        chan struct{}
	done        chan struct{}
	stopOnce    sync.Once
	doneOnce    sync.Once
}

// NewAcknowledgedWatcherWithLoader only advances Current after onChange
// accepts the candidate. Rejected candidates remain retryable.
func NewAcknowledgedWatcherWithLoader(
	path string, current *Config,
	loader func() (*Config, error), onChange func(*Config) error,
) *Watcher {
	return &Watcher{
		path:     normalizedPath(path),
		current:  Clone(current),
		loader:   loader,
		onChange: onChange,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins watching the config file for changes.
func (w *Watcher) Start() error {
	w.lifecycleMu.Lock()
	defer w.lifecycleMu.Unlock()
	switch w.state {
	case watcherStarted, watcherStopping:
		return ErrWatcherStarted
	case watcherStopped:
		return ErrWatcherStopped
	}
	if w.path == "" {
		w.state = watcherStopped
		w.doneOnce.Do(func() { close(w.done) })
		return nil
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify: %w", err)
	}

	watchDir := filepath.Dir(w.path)
	if err := watcher.Add(watchDir); err != nil {
		_ = watcher.Close()
		return fmt.Errorf("watch %s: %w", watchDir, err)
	}
	w.lastAttempt = w.startFingerprint()
	w.state = watcherStarted
	go w.run(watcher)
	return nil
}

// Stop stops watching.
func (w *Watcher) Stop() {
	w.lifecycleMu.Lock()
	if w.state == watcherNew {
		w.state = watcherStopped
		w.stopOnce.Do(func() { close(w.stop) })
		w.doneOnce.Do(func() { close(w.done) })
		w.lifecycleMu.Unlock()
		return
	}
	if w.state == watcherStarted {
		w.state = watcherStopping
	}
	w.stopOnce.Do(func() { close(w.stop) })
	done := w.done
	w.lifecycleMu.Unlock()
	<-done
}

// Done closes only after the watcher goroutine and fsnotify handle exit.
func (w *Watcher) Done() <-chan struct{} {
	return w.done
}

func (w *Watcher) run(watcher *fsnotify.Watcher) {
	debounce := newDebouncer()
	defer func() {
		debounce.stop()
		_ = watcher.Close()
		w.lifecycleMu.Lock()
		w.state = watcherStopped
		w.lifecycleMu.Unlock()
		w.doneOnce.Do(func() { close(w.done) })
	}()
	for {
		select {
		case _, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Any event in the directory is only a hint: a ConfigMap swap
			// renames ..data and never names config.yaml itself.
			debounce.arm(time.Now())
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[WARN] [config-watcher] error: %v", err)
		case <-debounce.timer.C:
			debounce.fired()
			if w.stopRequested() {
				return
			}
			w.reloadIfChanged()
		case <-w.stop:
			return
		}
	}
}

func (w *Watcher) stopRequested() bool {
	select {
	case <-w.stop:
		return true
	default:
		return false
	}
}

// reloadIfChanged runs reload only when the resolved file differs from the
// last attempted one, so a rejected file is retried after an edit or touch
// but not on every unrelated event in the directory.
func (w *Watcher) reloadIfChanged() {
	fingerprint, err := fingerprintFile(w.path)
	if err != nil {
		log.Printf("[DEBUG] [config-watcher] %v; keeping previous config "+
			"until the next change", err)
		return
	}
	if fingerprint == w.lastAttempt {
		return
	}
	w.lastAttempt = fingerprint
	w.reload()
}

func (w *Watcher) reload() {
	w.mu.RLock()
	current := Clone(w.current)
	w.mu.RUnlock()
	fresh, err := w.loadCandidate(current)
	if err != nil {
		log.Printf("[WARN] [config-watcher] parse failed: %v", err)
		return
	}
	if err := fresh.validateEffective(); err != nil {
		log.Printf("[WARN] [config-watcher] invalid config, keeping previous: %v", err)
		return
	}

	// Warn about non-hot-reloadable fields that changed.
	warnNonReloadable(current, fresh)

	changed := changedConfigPaths(current, fresh)

	if len(changed) > 0 {
		if w.onChange != nil {
			if err := w.onChange(Clone(fresh)); err != nil {
				log.Printf("[WARN] [config-watcher] reload rejected: %v", err)
				return
			}
		}
		w.mu.Lock()
		w.current = Clone(fresh)
		w.mu.Unlock()
		log.Printf("[INFO] [config-watcher] reloaded: %v", changed)
	}
}

func (w *Watcher) loadCandidate(current *Config) (*Config, error) {
	if w.loader != nil {
		candidate, err := w.loader()
		if err != nil {
			return nil, err
		}
		if candidate == nil {
			return nil, fmt.Errorf("candidate loader returned nil config")
		}
		return Clone(candidate), nil
	}
	candidate := Clone(current)
	if err := loadYAML(w.path, candidate); err != nil {
		return nil, err
	}
	return candidate, nil
}

func normalizedPath(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err == nil {
		path = absolute
	}
	return filepath.Clean(path)
}

// warnNonReloadable logs warnings for fields that changed but require restart.
func warnNonReloadable(current, fresh *Config) {
	if fresh.Postgres.Host != "" && fresh.Postgres.Host != current.Postgres.Host {
		log.Printf("[WARN] [config-watcher] postgres.host changed — restart required")
	}
	if fresh.Postgres.Port != 0 && fresh.Postgres.Port != current.Postgres.Port {
		log.Printf("[WARN] [config-watcher] postgres.port changed — restart required")
	}
	if fresh.Postgres.Database != "" &&
		fresh.Postgres.Database != current.Postgres.Database {
		log.Printf("[WARN] [config-watcher] postgres.database changed — restart required")
	}
	if fresh.Prometheus.ListenAddr != "" &&
		fresh.Prometheus.ListenAddr != current.Prometheus.ListenAddr {
		log.Printf(
			"[WARN] [config-watcher] prometheus.listen_addr changed — restart required",
		)
	}
	if fresh.Prometheus.ServeOnAPI != current.Prometheus.ServeOnAPI {
		log.Printf("[WARN] [config-watcher] prometheus.serve_on_api changed — restart required")
	}
	if fresh.Mode != "" && fresh.Mode != current.Mode {
		log.Printf("[WARN] [config-watcher] mode changed — restart required")
	}
}

// Current returns a read-locked copy of the current config.
func (w *Watcher) Current() *Config {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return Clone(w.current)
}
