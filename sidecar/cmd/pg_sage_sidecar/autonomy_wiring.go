package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/earned/hasource"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/gameday"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Sage SRE M7 wiring: one earned-autonomy ledger per database (P0-5),
// kept in its control database (every database of a meta-database fleet
// shares that deployment and its bench evidence, never a ledger), and one
// limiter per database (its HA role, its objects), installed into the
// executor's standing gate before the gate is built.

// ledgerKey is one database's ledger in one control database.
type ledgerKey struct {
	control  *pgxpool.Pool
	database string
}

// autonomyLedgers is the process's ledgers and their registries.
type autonomyLedgers struct {
	mu           sync.Mutex
	byKey        map[ledgerKey]*earned.Service
	registry     *earned.Registry
	gameDays     *gameday.Registry
	localBenches *gameday.BenchRegistry
}

func newAutonomyLedgers(enforced bool) *autonomyLedgers {
	return &autonomyLedgers{byKey: map[ledgerKey]*earned.Service{},
		registry: earned.NewRegistry(enforced), gameDays: gameday.NewRegistry(),
		localBenches: gameday.NewBenchRegistry()}
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

// autonomyServiceConfig maps the operator's settings, the promotion bar
// included (the spec's unless sre.autonomy.promotion lowers it).
func autonomyServiceConfig(s config.SREAutonomyConfig) earned.Config {
	c := earned.DefaultConfig()
	c.Thresholds = classPromotionThresholds(promotionThresholds(s.Promotion),
		s.ClassPromotion)
	c.ProposalTTL, c.MaxEvidenceAge = s.ProposalTTL(), s.MaxEvidenceAge()
	c.ConcurrencyWindow, c.SafetyWindow = s.ConcurrencyWindow(), s.SafetyWindow()
	c.FailoverCooldown = s.FailoverCooldown()
	c.Build = runningBuild()
	c.Log = func(format string, args ...any) { logWarn("autonomy", format, args...) }
	return c
}

// ledgerFor returns a database's ledger in a control pool, building it
// once.
func (a *autonomyLedgers) ledgerFor(ctx context.Context, control *pgxpool.Pool,
	database string, s config.SREAutonomyConfig) (*earned.Service, error) {
	if control == nil {
		return nil, fmt.Errorf("%w: no control database", earned.ErrUnavailable)
	}
	key := ledgerKey{control: control, database: database}
	a.mu.Lock()
	defer a.mu.Unlock()
	if svc, ok := a.byKey[key]; ok {
		return svc, nil
	}
	deployment, err := earned.EnsureDeployment(ctx, control)
	if err != nil {
		return nil, err
	}
	store, err := earned.NewPostgresStore(control, deployment, database)
	if err != nil {
		return nil, err
	}
	svc, err := earned.NewService(store, autonomyServiceConfig(s))
	if err != nil {
		return nil, err
	}
	a.byKey[key] = svc
	return svc, nil
}

// autonomyBinding is one database's place in the ledger.
type autonomyBinding struct {
	database   string
	control    *pgxpool.Pool
	monitored  *pgxpool.Pool
	databaseID *int
	settings   config.SREAutonomyConfig
	// budget is the database's M5 error budget; nil without SLOs (no
	// budget that could burn).
	budget earned.BudgetSource
	// notifier tells the operator about model-root authority changes;
	// set before the ledger is registered, so no grant goes untold.
	notifier earned.RootAuthorityNotifier
}

// failClosedLimiter answers every governed action (incident families and
// self-initiated classes) with the ledger's error, so the gate blocks
// rather than falls back to the trust ramp.
type failClosedLimiter struct{ err error }

func (l failClosedLimiter) Limit(context.Context, policy.ActionRequest) (
	policy.AutonomyLimit, error) {
	return policy.AutonomyLimit{}, l.err
}

// Governs is the ledger's own scope (policy.AutonomyScope).
func (l failClosedLimiter) Governs(req policy.ActionRequest) bool {
	return earned.Governs(req)
}

// install binds the database to its ledger and, when enforced, installs
// its limiter into the executor. A ledger that cannot be built fails
// family actions closed.
func (a *autonomyLedgers) install(ctx context.Context, ex *executor.Executor,
	b autonomyBinding) error {
	svc, err := a.ledgerFor(ctx, b.control, b.database, b.settings)
	if err == nil {
		// Levels stored before the ledger was per database apply to this
		// database at their level, unless it has its own (P0-5 decision).
		_, err = svc.AdoptLegacy(ctx)
	}
	if err == nil {
		// Keep the autonomy this database's configuration already grants
		// (coordinator decision 2026-10-02): M7 gates new autonomy only.
		_, err = svc.SeedCarriedOver(ctx, b.database, ex.OperatorBound())
	}
	if err == nil {
		// One trust system (roadmap 1.2): the ramp floors promotions, and
		// what the ramp already granted self-initiated classes is kept,
		// once, as grandfathered levels that demote normally.
		svc.WithRamp(ex.RampFloor)
		var rep earned.GrandfatherReport
		rep, err = svc.SeedGrandfathered(ctx, b.database, ex.OperatorBound())
		logGrandfathered(rep, logInfo)
	}
	if err != nil {
		if b.settings.Enforce {
			ex.WithAutonomy(failClosedLimiter{fmt.Errorf("autonomy ledger: %w", err)})
		}
		return err
	}
	lim := svc.Limiter(earned.Binding{Database: b.database, Budget: b.budget,
		HA:          hasource.New(newPersistedHAMonitor(b)),
		Concurrency: earned.NewPostgresConcurrency(b.monitored, b.databaseID)})
	if b.settings.Enforce {
		ex.WithAutonomy(lim)
	}
	if b.notifier != nil {
		svc.WithRootAuthorityNotifier(b.notifier)
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
