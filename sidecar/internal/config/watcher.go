package config

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

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

	lifecycleMu sync.Mutex
	state       watcherState
	stop        chan struct{}
	done        chan struct{}
	stopOnce    sync.Once
	doneOnce    sync.Once
}

// NewWatcher creates a config file watcher. Call Start() to begin watching.
func NewWatcher(path string, current *Config, onChange func(*Config)) *Watcher {
	return NewWatcherWithLoader(path, current, nil, onChange)
}

// NewWatcherWithLoader reloads a complete precedence-resolved candidate.
// Production uses this so environment and CLI overlays remain authoritative.
func NewWatcherWithLoader(
	path string, current *Config,
	loader func() (*Config, error), onChange func(*Config),
) *Watcher {
	var acknowledged func(*Config) error
	if onChange != nil {
		acknowledged = func(candidate *Config) error {
			onChange(candidate)
			return nil
		}
	}
	return NewAcknowledgedWatcherWithLoader(
		path, current, loader, acknowledged,
	)
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
	defer func() {
		_ = watcher.Close()
		w.lifecycleMu.Lock()
		w.state = watcherStopped
		w.lifecycleMu.Unlock()
		w.doneOnce.Do(func() { close(w.done) })
	}()
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if w.isConfigEvent(event) {
				w.reload()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[WARN] [config-watcher] error: %v", err)
		case <-w.stop:
			return
		}
	}
}

func (w *Watcher) isConfigEvent(event fsnotify.Event) bool {
	if !samePath(normalizedPath(event.Name), w.path) {
		return false
	}
	return event.Has(fsnotify.Write) || event.Has(fsnotify.Create) ||
		event.Has(fsnotify.Rename)
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
	if err := fresh.validate(); err != nil {
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

func samePath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
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
	if fresh.Mode != "" && fresh.Mode != current.Mode {
		log.Printf("[WARN] [config-watcher] mode changed — restart required")
	}
}

// applyHotReload copies hot-reloadable fields from fresh into target.
// Returns list of changed field names.
func applyHotReload(target, fresh *Config) []string {
	var changed []string

	if fresh.Collector.IntervalSeconds != 0 &&
		fresh.Collector.IntervalSeconds != target.Collector.IntervalSeconds {
		target.Collector.IntervalSeconds = fresh.Collector.IntervalSeconds
		changed = append(changed, "collector.interval_seconds")
	}
	if fresh.Collector.BatchSize != 0 &&
		fresh.Collector.BatchSize != target.Collector.BatchSize {
		target.Collector.BatchSize = fresh.Collector.BatchSize
		changed = append(changed, "collector.batch_size")
	}

	// Analyzer fields.
	a := &target.Analyzer
	fa := &fresh.Analyzer
	if fa.IntervalSeconds != 0 && fa.IntervalSeconds != a.IntervalSeconds {
		a.IntervalSeconds = fa.IntervalSeconds
		changed = append(changed, "analyzer.interval_seconds")
	}
	if fa.SlowQueryThresholdMs != 0 && fa.SlowQueryThresholdMs != a.SlowQueryThresholdMs {
		a.SlowQueryThresholdMs = fa.SlowQueryThresholdMs
		changed = append(changed, "analyzer.slow_query_threshold_ms")
	}

	// Safety fields.
	if fresh.Safety.CPUCeilingPct != 0 &&
		fresh.Safety.CPUCeilingPct != target.Safety.CPUCeilingPct {
		target.Safety.CPUCeilingPct = fresh.Safety.CPUCeilingPct
		changed = append(changed, "safety.cpu_ceiling_pct")
	}

	// Trust fields.
	if fresh.Trust.Level != "" && fresh.Trust.Level != target.Trust.Level {
		target.Trust.Level = fresh.Trust.Level
		changed = append(changed, "trust.level")
	}
	if fresh.Trust.MaintenanceWindow != target.Trust.MaintenanceWindow {
		target.Trust.MaintenanceWindow = fresh.Trust.MaintenanceWindow
		changed = append(changed, "trust.maintenance_window")
	}
	target.Trust.Tier3Safe = fresh.Trust.Tier3Safe
	target.Trust.Tier3Moderate = fresh.Trust.Tier3Moderate

	// LLM fields.
	if fresh.LLM.Enabled != target.LLM.Enabled {
		target.LLM.Enabled = fresh.LLM.Enabled
		changed = append(changed, "llm.enabled")
	}
	if fresh.LLM.Endpoint != "" && fresh.LLM.Endpoint != target.LLM.Endpoint {
		target.LLM.Endpoint = fresh.LLM.Endpoint
		changed = append(changed, "llm.endpoint")
	}
	if fresh.LLM.Model != "" && fresh.LLM.Model != target.LLM.Model {
		target.LLM.Model = fresh.LLM.Model
		changed = append(changed, "llm.model")
	}

	// Retention fields.
	if fresh.Retention.SnapshotsDays != 0 &&
		fresh.Retention.SnapshotsDays != target.Retention.SnapshotsDays {
		target.Retention.SnapshotsDays = fresh.Retention.SnapshotsDays
		changed = append(changed, "retention.snapshots_days")
	}

	// Alerting fields.
	if fresh.Alerting.CooldownMinutes != 0 &&
		fresh.Alerting.CooldownMinutes != target.Alerting.CooldownMinutes {
		target.Alerting.CooldownMinutes = fresh.Alerting.CooldownMinutes
		changed = append(changed, "alerting.cooldown_minutes")
	}
	if fresh.Alerting.QuietHoursStart != target.Alerting.QuietHoursStart {
		target.Alerting.QuietHoursStart = fresh.Alerting.QuietHoursStart
		changed = append(changed, "alerting.quiet_hours_start")
	}
	if fresh.Alerting.QuietHoursEnd != target.Alerting.QuietHoursEnd {
		target.Alerting.QuietHoursEnd = fresh.Alerting.QuietHoursEnd
		changed = append(changed, "alerting.quiet_hours_end")
	}
	if fresh.Alerting.CheckIntervalSeconds != 0 &&
		fresh.Alerting.CheckIntervalSeconds !=
			target.Alerting.CheckIntervalSeconds {
		target.Alerting.CheckIntervalSeconds =
			fresh.Alerting.CheckIntervalSeconds
		changed = append(changed, "alerting.check_interval_seconds")
	}
	if fresh.Alerting.Enabled != target.Alerting.Enabled {
		target.Alerting.Enabled = fresh.Alerting.Enabled
		changed = append(changed, "alerting.enabled")
	}
	target.Alerting.Routes = fresh.Alerting.Routes
	target.Alerting.Webhooks = fresh.Alerting.Webhooks

	// Tuner fields.
	if fresh.Tuner.Enabled != target.Tuner.Enabled {
		target.Tuner.Enabled = fresh.Tuner.Enabled
		changed = append(changed, "tuner.enabled")
	}
	if fresh.Tuner.WorkMemMaxMB != 0 &&
		fresh.Tuner.WorkMemMaxMB != target.Tuner.WorkMemMaxMB {
		target.Tuner.WorkMemMaxMB = fresh.Tuner.WorkMemMaxMB
		changed = append(changed, "tuner.work_mem_max_mb")
	}
	if fresh.Tuner.PlanTimeRatio != 0 &&
		fresh.Tuner.PlanTimeRatio != target.Tuner.PlanTimeRatio {
		target.Tuner.PlanTimeRatio = fresh.Tuner.PlanTimeRatio
		changed = append(changed, "tuner.plan_time_ratio")
	}
	if fresh.Tuner.NestedLoopRowThreshold != 0 &&
		fresh.Tuner.NestedLoopRowThreshold !=
			target.Tuner.NestedLoopRowThreshold {
		target.Tuner.NestedLoopRowThreshold =
			fresh.Tuner.NestedLoopRowThreshold
		changed = append(
			changed, "tuner.nested_loop_row_threshold",
		)
	}
	if fresh.Tuner.ParallelMinTableRows != 0 &&
		fresh.Tuner.ParallelMinTableRows !=
			target.Tuner.ParallelMinTableRows {
		target.Tuner.ParallelMinTableRows =
			fresh.Tuner.ParallelMinTableRows
		changed = append(
			changed, "tuner.parallel_min_table_rows",
		)
	}
	if fresh.Tuner.MinQueryCalls != 0 &&
		fresh.Tuner.MinQueryCalls != target.Tuner.MinQueryCalls {
		target.Tuner.MinQueryCalls = fresh.Tuner.MinQueryCalls
		changed = append(changed, "tuner.min_query_calls")
	}
	if fresh.Tuner.VerifyAfterApply !=
		target.Tuner.VerifyAfterApply {
		target.Tuner.VerifyAfterApply =
			fresh.Tuner.VerifyAfterApply
		changed = append(changed, "tuner.verify_after_apply")
	}

	// v0.8.5 Feature 1 — Hint revalidation loop.
	if fresh.Tuner.HintRetirementDays != 0 &&
		fresh.Tuner.HintRetirementDays !=
			target.Tuner.HintRetirementDays {
		target.Tuner.HintRetirementDays =
			fresh.Tuner.HintRetirementDays
		changed = append(changed, "tuner.hint_retirement_days")
	}
	if fresh.Tuner.RevalidationIntervalHours != 0 &&
		fresh.Tuner.RevalidationIntervalHours !=
			target.Tuner.RevalidationIntervalHours {
		target.Tuner.RevalidationIntervalHours =
			fresh.Tuner.RevalidationIntervalHours
		changed = append(changed, "tuner.revalidation_interval_hours")
	}
	if fresh.Tuner.RevalidationKeepRatio != 0 &&
		fresh.Tuner.RevalidationKeepRatio !=
			target.Tuner.RevalidationKeepRatio {
		target.Tuner.RevalidationKeepRatio =
			fresh.Tuner.RevalidationKeepRatio
		changed = append(changed, "tuner.revalidation_keep_ratio")
	}
	if fresh.Tuner.RevalidationRollbackRatio != 0 &&
		fresh.Tuner.RevalidationRollbackRatio !=
			target.Tuner.RevalidationRollbackRatio {
		target.Tuner.RevalidationRollbackRatio =
			fresh.Tuner.RevalidationRollbackRatio
		changed = append(changed, "tuner.revalidation_rollback_ratio")
	}
	if fresh.Tuner.RevalidationExplainTimeoutMs != 0 &&
		fresh.Tuner.RevalidationExplainTimeoutMs !=
			target.Tuner.RevalidationExplainTimeoutMs {
		target.Tuner.RevalidationExplainTimeoutMs =
			fresh.Tuner.RevalidationExplainTimeoutMs
		changed = append(changed, "tuner.revalidation_explain_timeout_ms")
	}

	// v0.8.5 Feature 2 — Stale-stats detection + ANALYZE.
	if fresh.Tuner.StaleStatsEstimateSkew != 0 &&
		fresh.Tuner.StaleStatsEstimateSkew !=
			target.Tuner.StaleStatsEstimateSkew {
		target.Tuner.StaleStatsEstimateSkew =
			fresh.Tuner.StaleStatsEstimateSkew
		changed = append(changed, "tuner.stale_stats_estimate_skew")
	}
	if fresh.Tuner.StaleStatsModRatio != 0 &&
		fresh.Tuner.StaleStatsModRatio !=
			target.Tuner.StaleStatsModRatio {
		target.Tuner.StaleStatsModRatio =
			fresh.Tuner.StaleStatsModRatio
		changed = append(changed, "tuner.stale_stats_mod_ratio")
	}
	if fresh.Tuner.StaleStatsAgeMinutes != 0 &&
		fresh.Tuner.StaleStatsAgeMinutes !=
			target.Tuner.StaleStatsAgeMinutes {
		target.Tuner.StaleStatsAgeMinutes =
			fresh.Tuner.StaleStatsAgeMinutes
		changed = append(changed, "tuner.stale_stats_age_minutes")
	}
	if fresh.Tuner.AnalyzeMaxTableMB != 0 &&
		fresh.Tuner.AnalyzeMaxTableMB !=
			target.Tuner.AnalyzeMaxTableMB {
		target.Tuner.AnalyzeMaxTableMB =
			fresh.Tuner.AnalyzeMaxTableMB
		changed = append(changed, "tuner.analyze_max_table_mb")
	}
	if fresh.Tuner.AnalyzeCooldownMinutes != 0 &&
		fresh.Tuner.AnalyzeCooldownMinutes !=
			target.Tuner.AnalyzeCooldownMinutes {
		target.Tuner.AnalyzeCooldownMinutes =
			fresh.Tuner.AnalyzeCooldownMinutes
		changed = append(changed, "tuner.analyze_cooldown_minutes")
	}
	if fresh.Tuner.AnalyzeMaintenanceThresholdMB != 0 &&
		fresh.Tuner.AnalyzeMaintenanceThresholdMB !=
			target.Tuner.AnalyzeMaintenanceThresholdMB {
		target.Tuner.AnalyzeMaintenanceThresholdMB =
			fresh.Tuner.AnalyzeMaintenanceThresholdMB
		changed = append(changed, "tuner.analyze_maintenance_threshold_mb")
	}
	if fresh.Tuner.AnalyzeTimeoutMs != 0 &&
		fresh.Tuner.AnalyzeTimeoutMs !=
			target.Tuner.AnalyzeTimeoutMs {
		target.Tuner.AnalyzeTimeoutMs =
			fresh.Tuner.AnalyzeTimeoutMs
		changed = append(changed, "tuner.analyze_timeout_ms")
	}
	if fresh.Tuner.MaxConcurrentAnalyze != 0 &&
		fresh.Tuner.MaxConcurrentAnalyze !=
			target.Tuner.MaxConcurrentAnalyze {
		target.Tuner.MaxConcurrentAnalyze =
			fresh.Tuner.MaxConcurrentAnalyze
		changed = append(changed, "tuner.max_concurrent_analyze")
	}

	// v0.8.5 Feature 3 — work_mem role-promotion advisor.
	if fresh.Analyzer.WorkMemPromotionThreshold != 0 &&
		fresh.Analyzer.WorkMemPromotionThreshold !=
			target.Analyzer.WorkMemPromotionThreshold {
		target.Analyzer.WorkMemPromotionThreshold =
			fresh.Analyzer.WorkMemPromotionThreshold
		changed = append(changed, "analyzer.work_mem_promotion_threshold")
	}

	// auto_explain fields.
	if fresh.AutoExplain.LogMinDurationMs != 0 &&
		fresh.AutoExplain.LogMinDurationMs !=
			target.AutoExplain.LogMinDurationMs {
		target.AutoExplain.LogMinDurationMs =
			fresh.AutoExplain.LogMinDurationMs
		changed = append(changed, "auto_explain.log_min_duration_ms")
	}
	if fresh.AutoExplain.MaxPlansPerCycle != 0 &&
		fresh.AutoExplain.MaxPlansPerCycle !=
			target.AutoExplain.MaxPlansPerCycle {
		target.AutoExplain.MaxPlansPerCycle =
			fresh.AutoExplain.MaxPlansPerCycle
		changed = append(changed, "auto_explain.max_plans_per_cycle")
	}

	return changed
}

// Current returns a read-locked copy of the current config.
func (w *Watcher) Current() *Config {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return Clone(w.current)
}
