package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/earned/hasource"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/gameday"
	"github.com/pg-sage/sidecar/internal/ha"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Sage SRE M7 wiring: one earned-autonomy ledger per control database
// (shared by every database of a meta-database fleet), one limiter per
// database (its HA role, its objects), installed into the executor's
// standing gate before the gate is built.

// autonomyLedgers is the process's ledgers and their registries.
type autonomyLedgers struct {
	mu       sync.Mutex
	byPool   map[*pgxpool.Pool]*earned.Service
	registry *earned.Registry
	gameDays *gameday.Registry
}

func newAutonomyLedgers(enforced bool) *autonomyLedgers {
	return &autonomyLedgers{byPool: map[*pgxpool.Pool]*earned.Service{},
		registry: earned.NewRegistry(enforced), gameDays: gameday.NewRegistry()}
}

var (
	processAutonomyOnce sync.Once
	processAutonomyVal  *autonomyLedgers
)

// processAutonomy is the process-wide ledgers, enforced per the loaded
// config (sre.autonomy.enforce).
func processAutonomy() *autonomyLedgers {
	processAutonomyOnce.Do(func() {
		processAutonomyVal = newAutonomyLedgers(cfg == nil || cfg.SRE.Autonomy.Enforce)
	})
	return processAutonomyVal
}

// autonomyServiceConfig maps the operator's settings; the promotion
// thresholds are the spec's.
func autonomyServiceConfig(s config.SREAutonomyConfig) earned.Config {
	c := earned.DefaultConfig()
	c.ProposalTTL, c.MaxEvidenceAge = s.ProposalTTL(), s.MaxEvidenceAge()
	c.ConcurrencyWindow, c.SafetyWindow = s.ConcurrencyWindow(), s.SafetyWindow()
	c.FailoverCooldown = s.FailoverCooldown()
	c.Log = func(format string, args ...any) { logWarn("autonomy", format, args...) }
	return c
}

// ledgerFor returns the ledger of a control pool, building it once.
func (a *autonomyLedgers) ledgerFor(ctx context.Context, control *pgxpool.Pool,
	s config.SREAutonomyConfig) (*earned.Service, error) {
	if control == nil {
		return nil, fmt.Errorf("%w: no control database", earned.ErrUnavailable)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if svc, ok := a.byPool[control]; ok {
		return svc, nil
	}
	deployment, err := earned.EnsureDeployment(ctx, control)
	if err != nil {
		return nil, err
	}
	store, err := earned.NewPostgresStore(control, deployment)
	if err != nil {
		return nil, err
	}
	svc, err := earned.NewService(store, autonomyServiceConfig(s))
	if err != nil {
		return nil, err
	}
	a.byPool[control] = svc
	return svc, nil
}

// autonomyBinding is one database's place in the ledger.
type autonomyBinding struct {
	database   string
	control    *pgxpool.Pool
	monitored  *pgxpool.Pool
	databaseID *int
	settings   config.SREAutonomyConfig
}

// failClosedLimiter answers every family action with the ledger's error,
// so the gate blocks rather than falls back to the trust ramp.
type failClosedLimiter struct{ err error }

func (l failClosedLimiter) Limit(context.Context, policy.ActionRequest) (
	policy.AutonomyLimit, error) {
	return policy.AutonomyLimit{}, l.err
}

// install binds the database to its ledger and, when enforced, installs
// its limiter into the executor. A ledger that cannot be built fails
// family actions closed.
func (a *autonomyLedgers) install(ctx context.Context, ex *executor.Executor,
	b autonomyBinding) error {
	svc, err := a.ledgerFor(ctx, b.control, b.settings)
	if err == nil {
		// Keep the autonomy this database's configuration already grants
		// (coordinator decision 2026-10-02): M7 gates new autonomy only.
		_, err = svc.SeedCarriedOver(ctx, b.database, ex.OperatorBound())
	}
	if err != nil {
		if b.settings.Enforce {
			ex.WithAutonomy(failClosedLimiter{fmt.Errorf("autonomy ledger: %w", err)})
		}
		return err
	}
	lim := svc.Limiter(earned.Binding{Database: b.database,
		HA:          hasource.New(ha.New(b.monitored, logStructuredWrapper)),
		Concurrency: earned.NewPostgresConcurrency(b.monitored, b.databaseID)})
	if b.settings.Enforce {
		ex.WithAutonomy(lim)
	}
	a.registry.Register(b.database, earned.RegistryEntry{Service: svc, Limiter: lim})
	return nil
}

// autonomyNotifier tells a human about an L3 auto-execution through the
// database's notification rules (and the log).
type autonomyNotifier struct {
	dispatcher executor.EventDispatcher
}

func (n autonomyNotifier) NotifyAutonomous(ctx context.Context, a earned.AutoExecution) error {
	subject := fmt.Sprintf("pg_sage executed %s for %s autonomously (L3)", a.Class, a.Family)
	logInfo("autonomy", "%s on %s: action_log %d", subject, a.Database, a.ActionLogID)
	if n.dispatcher == nil {
		return nil
	}
	return n.dispatcher.Dispatch(ctx, notify.Event{Type: "action_executed",
		Severity: "warning", Subject: subject,
		Body: fmt.Sprintf("Database: %s\nEarned level: L3 (%s x %s)\nSQL: %s",
			a.Database, a.Family, a.Class, a.SQL),
		Data: map[string]any{"database": a.Database, "family": string(a.Family),
			"class": string(a.Class), "action_log_id": a.ActionLogID, "sql": a.SQL},
		DedupKey: fmt.Sprintf("autonomy:%s:%d", a.Database, a.ActionLogID)})
}

// ingestBenchPath ingests a PGIncidentBench report, or every *.json in a
// directory, and reports how many were new.
func ingestBenchPath(ctx context.Context, svc *earned.Service, path string) (int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("bench results path: %w", err)
	}
	files := []string{path}
	if info.IsDir() {
		if files, err = filepath.Glob(filepath.Join(path, "*.json")); err != nil {
			return 0, fmt.Errorf("bench results path: %w", err)
		}
		sort.Strings(files)
	}
	added := 0
	var errs []error
	for _, f := range files {
		run, err := ingestBenchFile(ctx, svc, f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !run.Duplicate {
			added++
		}
	}
	return added, errors.Join(errs...)
}

func ingestBenchFile(ctx context.Context, svc *earned.Service, path string) (
	earned.EvalRun, error) {
	f, err := os.Open(path)
	if err != nil {
		return earned.EvalRun{}, fmt.Errorf("open bench report: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, earned.MaxReportBytes+1))
	if err != nil {
		return earned.EvalRun{}, fmt.Errorf("read bench report %s: %w", path, err)
	}
	run, err := svc.IngestEvalRun(ctx, raw, earned.SourceBench, "bench_results_path", "")
	if err != nil {
		return earned.EvalRun{}, fmt.Errorf("bench report %s: %w",
			filepath.Base(path), err)
	}
	return run, nil
}

// monitoredDSNs are every monitored database's connection strings, so a
// local game-day database can never be one of them.
func monitoredDSNs(extra ...*pgxpool.Pool) []string {
	var out []string
	pools := extra
	if fleetMgr != nil {
		pools = append(pools, fleetMgr.AllPools()...)
	}
	for _, p := range pools {
		if p != nil {
			out = append(out, p.Config().ConnString())
		}
	}
	return out
}

func trimmedFamilies(in []string) []string {
	out := make([]string, 0, len(in))
	for _, f := range in {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
