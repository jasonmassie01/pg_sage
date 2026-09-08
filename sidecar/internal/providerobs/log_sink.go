package providerobs

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/autoexplain"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/rca"
)

const maxSignalQueue = 1000

// LogSink receives API records and implements the existing RCA LogSource without reading files.
type LogSink struct {
	mu               sync.Mutex
	database         string
	classifier       *logwatch.Classifier
	classifierConfig logwatch.ClassifierConfig
	plansEnabled     bool
	stopped          bool
	signals          []*rca.Signal
	store            func(context.Context, autoexplain.ObservedPlan) error
	rejections       map[string]uint64
	reportRejection  func(string, uint64)
}

var _ rca.LogSource = (*LogSink)(nil)

func NewLogSink(pool *pgxpool.Pool, database string, cfg *config.Config) (*LogSink, error) {
	if cfg == nil || database == "" {
		return nil, errors.New("log sink requires database and config")
	}
	s := &LogSink{database: database, plansEnabled: cfg.AutoExplain.Enabled,
		rejections: make(map[string]uint64)}
	s.store = func(ctx context.Context, plan autoexplain.ObservedPlan) error {
		return autoexplain.StoreObservedPlan(ctx, pool, plan)
	}
	s.classifierConfig = logwatch.ClassifierConfig{DedupWindowS: cfg.LogWatch.DedupWindowS,
		ExcludeApps:      append([]string(nil), cfg.LogWatch.ExcludeApplications...),
		SlowQueryEnabled: cfg.LogWatch.SlowQueryEnabled,
		TempFileMinBytes: int64(cfg.LogWatch.TempFileMinBytes),
		MaxLinesPerCycle: cfg.LogWatch.MaxLinesPerCycle}
	if cfg.RCA.Enabled && cfg.LogWatch.Enabled {
		s.classifier = logwatch.NewClassifier(s.classifierConfig, nil)
	}
	return s, nil
}

func (s *LogSink) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped && s.classifier != nil {
		s.classifier = logwatch.NewClassifier(s.classifierConfig, nil)
	}
	s.stopped = false
	return nil
}

func (s *LogSink) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	s.signals = nil
}

func (s *LogSink) Drain() []*rca.Signal {
	s.mu.Lock()
	defer s.mu.Unlock()
	signals := s.signals
	s.signals = nil
	return signals
}

// Handle rejects overflow before mutating the classifier, keeping failed batches retryable.
func (s *LogSink) Handle(ctx context.Context, entries []logwatch.LogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.stopped {
		return errors.New("provider log sink is stopped")
	}
	matching := s.matchingEntries(entries)
	if len(matching) > maxSignalQueue-len(s.signals) {
		return errors.New("provider RCA queue is full; drain signals before retrying logs")
	}
	if s.classifier != nil && s.classifierConfig.MaxLinesPerCycle > 0 &&
		len(matching) > s.classifierConfig.MaxLinesPerCycle {
		return errors.New("provider log batch exceeds configured classification limit")
	}
	if err := s.storePlans(ctx, matching); err != nil {
		return err
	}
	if s.classifier == nil {
		return nil
	}
	s.classifier.ResetCycle()
	s.classifier.CleanExpiredDedup()
	for _, entry := range matching {
		if signal := s.classifier.Classify(entry); signal != nil {
			s.signals = append(s.signals, signal)
		}
	}
	return nil
}

func (s *LogSink) matchingEntries(entries []logwatch.LogEntry) []logwatch.LogEntry {
	matching := make([]logwatch.LogEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Database == s.database {
			matching = append(matching, entry)
		}
	}
	return matching
}

func (s *LogSink) storePlans(ctx context.Context, entries []logwatch.LogEntry) error {
	if !s.plansEnabled {
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for _, entry := range entries {
		if !actualPlanHeader.MatchString(entry.Message) {
			continue
		}
		plan, err := autoexplain.ParseObservedPlan(entry, s.database)
		if err != nil {
			s.rejectPlan("invalid_json_plan")
			continue
		}
		if plan.QueryID == 0 {
			s.rejectPlan("missing_query_identifier")
			continue
		}
		if err := s.store(bounded, plan); err != nil {
			return &logSinkError{cause: err}
		}
	}
	return nil
}

var actualPlanHeader = regexp.MustCompile(`^duration:\s+\S+\s+ms\s+plan:\s*`)

func (s *LogSink) SetRejectionReporter(report func(string, uint64)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reportRejection = report
}

func (s *LogSink) rejectPlan(reason string) {
	s.rejections[reason]++
	if s.reportRejection != nil {
		s.reportRejection(reason, 1)
	}
}

func (s *LogSink) Rejections() map[string]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]uint64, len(s.rejections))
	for key, count := range s.rejections {
		result[key] = count
	}
	return result
}

type logSinkError struct{ cause error }

func (e *logSinkError) Error() string {
	return "provider auto_explain ingestion failed; " +
		"verify JSON/verbose logging and metadata store access"
}
func (e *logSinkError) Unwrap() error { return e.cause }
