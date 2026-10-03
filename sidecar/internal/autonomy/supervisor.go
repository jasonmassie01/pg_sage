package autonomy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/schemaguard"
)

const defaultInterval = time.Minute

type Supervisor struct {
	configs []DatabaseWorkersConfig
	mu      sync.Mutex
	cancel  context.CancelFunc
	workers sync.WaitGroup
	started bool
}

func NewSupervisor(configs []DatabaseWorkersConfig) (*Supervisor, error) {
	seen := make(map[string]bool, len(configs))
	normalized := append([]DatabaseWorkersConfig(nil), configs...)
	for index := range normalized {
		config := &normalized[index]
		name := strings.TrimSpace(config.Database)
		if name == "" || seen[name] {
			return nil, fmt.Errorf("autonomy database identity must be unique")
		}
		if config.Freeze == nil || config.WAL == nil || config.Router == nil ||
			config.Auditor == nil || config.Reporter == nil {
			return nil, fmt.Errorf("autonomy dependencies for %s are incomplete", name)
		}
		config.Database = name
		config.schemaGate = make(chan struct{}, 1)
		config.schemaGate <- struct{}{}
		config.schemaTrigger = make(chan struct{}, 1)
		seen[name] = true
	}
	return &Supervisor{configs: normalized}, nil
}

func (s *Supervisor) RequestSchemaGuard(database string) error {
	for _, config := range s.configs {
		if config.Database != database {
			continue
		}
		if config.Schema == nil {
			return fmt.Errorf("schema guard for database %s is unavailable", database)
		}
		select {
		case config.schemaTrigger <- struct{}{}:
		default:
		}
		return nil
	}
	return fmt.Errorf("schema guard database %s is unknown", database)
}

func (s *Supervisor) TriggerSchemaGuard(ctx context.Context, database string) error {
	for _, config := range s.configs {
		if config.Database != database {
			continue
		}
		if config.Schema == nil {
			return fmt.Errorf("schema guard for database %s is unavailable", database)
		}
		if err := runSchemaGuard(ctx, config); err != nil {
			return fmt.Errorf("trigger schema guard for %s: %w", database, err)
		}
		return nil
	}
	return fmt.Errorf("schema guard database %s is unknown", database)
}

func (s *Supervisor) Start(parent context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel, s.started = cancel, true
	for _, config := range s.configs {
		config := config
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			runDatabaseWorker(ctx, config)
		}()
	}
}

func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() { s.workers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("drain autonomy workers: %w", ctx.Err())
	}
}

func runDatabaseWorker(ctx context.Context, config DatabaseWorkersConfig) {
	ticks, stop := workerTicks(config)
	defer stop()
	parked := map[string]bool{} // parked routes already reported (this worker only)
	ddl := &ddlDebouncer{window: ddlDebounce(config)}
	defer ddl.cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			runDatabaseCycle(ctx, config, parked)
			ddl.ran()
		case <-config.schemaTrigger:
			if ddl.request(time.Now()) {
				runDDLSchemaGuard(ctx, config, parked, ddl)
			}
		case <-ddl.due():
			runDDLSchemaGuard(ctx, config, parked, ddl)
		}
	}
}

func runDDLSchemaGuard(
	ctx context.Context, config DatabaseWorkersConfig, parked map[string]bool,
	ddl *ddlDebouncer,
) {
	if err := runSchemaGuard(ctx, config); err != nil {
		reportWorkerError(config, parked, "DDL-triggered schema guard failed", err)
	}
	ddl.ran()
}

func workerTicks(config DatabaseWorkersConfig) (<-chan time.Time, func()) {
	if config.Tick != nil {
		return config.Tick, func() {}
	}
	interval := config.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	return ticker.C, ticker.Stop
}

func runDatabaseCycle(ctx context.Context, config DatabaseWorkersConfig, parked map[string]bool) {
	for _, custodian := range []Custodian{config.Freeze, config.WAL} {
		proposals, err := custodian.Scan(ctx)
		if err != nil {
			reportWorkerError(config, parked, "custodian scan failed", err)
			continue
		}
		for _, proposal := range proposals {
			proposal.Database = config.Database
			if err := config.Router.Route(ctx, proposal); err != nil {
				reportWorkerError(config, parked, "custodian proposal failed", err)
			}
		}
	}
	if config.Schema != nil {
		if err := runSchemaGuard(ctx, config); err != nil {
			reportWorkerError(config, parked, "schema guard scan failed", err)
		}
	}
	runSelfAudit(ctx, config)
}

func runSchemaGuard(ctx context.Context, config DatabaseWorkersConfig) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-config.schemaGate:
	}
	defer func() { config.schemaGate <- struct{}{} }()
	_, err := config.Schema.Scan(ctx)
	return err
}

func runSelfAudit(ctx context.Context, config DatabaseWorkersConfig) {
	result, err := config.Auditor.SelfAudit(ctx)
	if err != nil {
		reportWorkerError(config, nil, "ledger self-audit failed: "+err.Error(), err)
		return
	}
	if result.OK {
		return
	}
	kinds := make([]string, 0, len(result.Violations))
	for _, violation := range result.Violations {
		kinds = append(kinds, violation.Kind)
	}
	config.Reporter.Report("error",
		fmt.Sprintf("database %s ledger self-audit: %s", config.Database,
			strings.Join(kinds, ",")),
		map[string]any{"database": config.Database, "violations": result.Violations},
	)
}

// reportWorkerError reports a worker failure as an error. A parked route
// (policy withheld the proposal, it cannot be verified yet) is an expected
// outcome: reported once at info per message and reason (dogfood lifeos-1:
// blast_radius_exceeded was an ERROR every cycle).
func reportWorkerError(config DatabaseWorkersConfig, parked map[string]bool, message string,
	err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	var p *schemaguard.ParkedRoute
	if errors.As(err, &p) {
		key := message + "|" + p.Error()
		if parked == nil || parked[key] {
			return
		}
		parked[key] = true
		config.Reporter.Report("info", message+" (parked, not an error)",
			map[string]any{"database": config.Database, "reason": p.Error()})
		return
	}
	config.Reporter.Report("error", message,
		map[string]any{"database": config.Database, "error": err.Error()})
}
